package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
)

const defaultIgnore = "*.md,.github/**"

func cmdTest(args []string) error {
	own, goFlags := splitArgs(args)
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	list := fs.Bool("list", false, "print the plan and commands without running them")
	jsonOut := fs.Bool("json", false, "print the plan as JSON without running it")
	all := fs.Bool("all", false, "run every package")
	mapFlag := fs.String("map", "", "map file (default: inside .git, or $GOTIA_MAP)")
	base := fs.String("base", "", "commit to measure changes against (default: the map's commit, else HEAD)")
	ignore := fs.String("ignore", envOr("GOTIA_IGNORE", defaultIgnore), "comma-separated globs of non-Go files that no test reads (also $GOTIA_IGNORE)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gotia test [flags] [paths...] [-- go test flags]\n\nRuns the tests affected by the given files, or by every change (committed,\nstaged, unstaged and untracked) against the base commit.")
		fs.PrintDefaults()
	}
	fs.Parse(own)
	var listFlags []string
	if t := buildTags(goFlags); t != "" {
		listFlags = []string{"-tags=" + t}
	}
	m, err := loadModule(".", listFlags)
	if err != nil {
		return err
	}
	var pl *plan
	if *all {
		pl = &plan{Effects: []string{"-all → every package"}, Tests: map[string][]string{}}
		for _, pkg := range m.packages {
			pl.Whole = append(pl.Whole, m.short(pkg.ImportPath))
		}
	} else {
		if pl, err = makePlan(m, *mapFlag, *base, fs.Args(), splitList(*ignore), goFlags); err != nil {
			return err
		}
	}
	pl.commands(m, goFlags)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(pl)
	}
	report(pl)
	if *list {
		for _, c := range pl.Commands {
			fmt.Println("  " + strings.Join(quote(c), " "))
		}
		return nil
	}
	if len(pl.Commands) == 0 {
		return nil
	}
	return execute(m, pl.Commands)
}

func makePlan(m *module, mapFile, base string, paths, ignore, goFlags []string) (*plan, error) {
	goFlagsRun := hasRun(goFlags)
	var c *coverMap
	var mapNote string
	if mapFile == "" {
		var err error
		if mapFile, err = defaultMapPath(m); err != nil {
			mapNote = "no git repository; per-test selection needs one"
		}
	}
	if mapFile != "" {
		c, mapNote = readMap(m, mapFile)
	}
	if goFlagsRun && c != nil {
		c, mapNote = nil, "-run given; selecting whole packages"
	}
	if c != nil && buildTags(goFlags) != buildTags(c.BuildFlags) {
		c, mapNote = nil, fmt.Sprintf("-tags differ from the map's (%q); selecting whole packages", buildTags(c.BuildFlags))
	}
	if base == "" {
		base = "HEAD"
		if c != nil {
			base = c.Base
		}
	} else if c != nil && base != c.Base {
		c, mapNote = nil, "-base differs from the map's commit; selecting whole packages"
	}
	commit, err := output(m.root, "git", "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", base, err)
	}
	if c != nil {
		if behind, err := output(m.root, "git", "rev-list", "--count", commit+"..HEAD"); err == nil && behind != "0" {
			mapNote = fmt.Sprintf("the map is %s commits behind HEAD; `gotia record` keeps selection narrow", behind)
		}
	}
	var files []string
	if len(paths) == 0 {
		if files, err = m.changedFiles(commit); err != nil {
			return nil, err
		}
	} else {
		for _, name := range paths {
			rel, ok := m.relPath(name)
			if !ok {
				return nil, fmt.Errorf("%s is outside the module at %s", name, m.root)
			}
			files = append(files, rel)
		}
	}
	p := newPlanner(m, c, commit, ignore)
	for _, rel := range files {
		p.file(rel)
	}
	pl := p.finish(goFlagsRun)
	pl.MapNote = mapNote
	if len(files) == 0 {
		pl.Effects = append(pl.Effects, fmt.Sprintf("no changes against %.7s", commit))
	}
	return pl, nil
}

func report(pl *plan) {
	switch {
	case pl.Map:
		note("per-test selection from the coverage map at %.7s", pl.Base)
	case pl.Base != "":
		note("package selection against %.7s", pl.Base)
	}
	if pl.MapNote != "" {
		note("%s", pl.MapNote)
	}
	for _, e := range pl.Effects {
		note("%s", e)
	}
	if len(pl.Uncovered) > 0 {
		note("no recorded test runs %s; add a test if it matters", strings.Join(pl.Uncovered, ", "))
	}
	if len(pl.Commands) == 0 {
		note("nothing to test")
		return
	}
	var parts []string
	if n := pl.testCount(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d tests in %d packages", n, len(pl.Tests)))
	}
	if len(pl.Whole) > 0 {
		parts = append(parts, fmt.Sprintf("%d packages in full", len(pl.Whole)))
	}
	note("running %s; %d packages skipped", strings.Join(parts, " and "), pl.Skipped)
}

// execute runs the commands at once; go test prints each package's result as
// one block, so their output stays readable.
func execute(m *module, commands [][]string) error {
	errs := make([]error, len(commands))
	var wg sync.WaitGroup
	for i, c := range commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Dir = m.root
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			errs[i] = cmd.Run()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitCode(exit.ExitCode())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func hasRun(flags []string) bool {
	for _, f := range flags {
		f = strings.TrimLeft(f, "-")
		if f == "run" || strings.HasPrefix(f, "run=") || f == "test.run" || strings.HasPrefix(f, "test.run=") {
			return true
		}
	}
	return false
}

// buildTags returns the build tags that go command flags set, sorted and
// joined with commas. The last -tags flag wins, as in the go command.
func buildTags(flags []string) string {
	var tags []string
	for i, f := range flags {
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if !strings.HasPrefix(f, "-") || name != "tags" {
			continue
		}
		if !hasValue && i+1 < len(flags) {
			value = flags[i+1]
		}
		tags = strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' })
	}
	sort.Strings(tags)
	return strings.Join(slices.Compact(tags), ",")
}

func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func quote(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " |()^$*?'\"") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return out
}
