package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

const defaultIgnore = "*.md,.github/**"

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	list := fs.Bool("list", false, "print the plan and commands without running them")
	jsonOut := fs.Bool("json", false, "print the plan as JSON without running it")
	all := fs.Bool("all", false, "run every package")
	mapFlag := fs.String("map", "", "map file (default: inside .git, or $GOTIA_MAP)")
	base := fs.String("base", "", "commit to measure changes against (default: the map's commit, else HEAD)")
	ignore := fs.String("ignore", envOr("GOTIA_IGNORE", defaultIgnore), "comma-separated globs of non-Go files that no test reads (also $GOTIA_IGNORE)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gotia test [flags] [paths...] [go test flags]\n\nRuns the tests affected by every change (committed, staged, unstaged and\nuntracked) against the base commit. Paths limit it: a file or directory runs\nwhat it affects as if it changed, and a pattern such as ./pkg/... keeps only\nthe changes under pkg. go test flags such as -race follow gotia's own, or \"--\".")
		fs.PrintDefaults()
	}
	own, goFlags := splitArgs(fs, args)
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
		pl = allPlan(m, "-all → every package")
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
	return execute(m, pl, goFlags)
}

func allPlan(m *module, why string) *plan {
	pl := &plan{Effects: []string{why}, Tests: map[string][]string{}}
	for _, pkg := range m.packages {
		pl.Whole = append(pl.Whole, m.short(pkg.ImportPath))
	}
	return pl
}

func makePlan(m *module, mapFile, base string, paths, ignore, goFlags []string) (*plan, error) {
	goFlagsRun := hasFlag(goFlags, "run")
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
	// A pattern such as ./pkg/... keeps the changes under pkg; other paths
	// count as changed themselves.
	var trees, named []string
	if len(paths) == 0 {
		trees = []string{"."}
	}
	for _, name := range paths {
		tree, isPattern := strings.CutSuffix(filepath.ToSlash(name), "...")
		if isPattern {
			if tree = strings.TrimSuffix(tree, "/"); tree == "" {
				tree = "."
			}
			name = tree
		}
		rel, ok := m.relPath(name)
		if !ok {
			return nil, fmt.Errorf("%s is outside the module at %s", name, m.root)
		}
		if isPattern {
			trees = append(trees, rel)
		} else {
			named = append(named, rel)
		}
	}
	userBase := base != ""
	if !userBase {
		base = "HEAD"
		if c != nil {
			base = c.Base
		}
	} else if c != nil && base != c.Base {
		c, mapNote = nil, "-base differs from the map's commit; selecting whole packages"
	}
	commit, err := output(m.root, "git", "rev-parse", "--verify", base+"^{commit}")
	switch {
	case err == nil:
	case userBase:
		return nil, fmt.Errorf("base %s: %w", base, err)
	case len(trees) > 0:
		// No commit to compare with: a new repository, or none at all.
		return allPlan(m, "no commit to measure changes against → every package"), nil
	default:
		commit = ""
	}
	if c != nil {
		if behind, err := output(m.root, "git", "rev-list", "--count", commit+"..HEAD"); err == nil && behind != "0" {
			mapNote = fmt.Sprintf("the map is %s commits behind HEAD; `gotia record` keeps selection narrow", behind)
		}
	}
	files := named
	var unseen []string
	if len(trees) > 0 {
		changed, outside, err := m.changedFiles(commit)
		if err != nil {
			return nil, err
		}
		for _, rel := range changed {
			for _, tree := range trees {
				if tree == "." || rel == tree || strings.HasPrefix(rel, tree+"/") {
					files = append(files, rel)
					break
				}
			}
		}
		if slices.Contains(trees, ".") {
			unseen = outside
		}
	}
	p := newPlanner(m, c, commit, ignore)
	for _, tree := range trees {
		if tree != "." {
			p.effect("%s/...: only the changes under %s", tree, tree)
		}
	}
	for _, rel := range unseen {
		p.unseen(rel)
	}
	for _, rel := range files {
		p.file(rel)
	}
	pl := p.finish(goFlagsRun)
	pl.MapNote = mapNote
	if len(files)+len(unseen) == 0 {
		where := ""
		if len(trees) > 0 && !slices.Contains(trees, ".") {
			where = " under the given patterns"
		}
		pl.Effects = append(pl.Effects, fmt.Sprintf("no changes%s against %.7s", where, commit))
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
		note("nothing to test: no test can be affected")
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

// execute runs the commands at once and prints their output line by line,
// so lines of two commands never mix. It ends with one line for a pass, or
// one line per failed package naming its failed tests and the command that
// reruns them.
func execute(m *module, pl *plan, goFlags []string) error {
	var mu sync.Mutex
	var failures []string
	summarize := !hasFlag(goFlags, "json") // go test -json output is for programs
	errs := make([]error, len(pl.Commands))
	var wg sync.WaitGroup
	for i, c := range pl.Commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Dir = m.root
			cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
			out, err := cmd.StdoutPipe()
			if err != nil {
				errs[i] = err
				return
			}
			if errs[i] = cmd.Start(); errs[i] != nil {
				return
			}
			// go test prints a package's failed tests before its FAIL line.
			var failed []string
			r := bufio.NewReader(out)
			for {
				line, err := r.ReadString('\n')
				if line != "" {
					mu.Lock()
					fmt.Print(line)
					if !strings.HasSuffix(line, "\n") {
						fmt.Println()
					}
					line = strings.TrimRight(line, "\r\n")
					if name, ok := strings.CutPrefix(line, "--- FAIL: "); ok {
						failed = append(failed, strings.Fields(name)[0])
					} else if rest, ok := strings.CutPrefix(line, "FAIL\t"); ok {
						// "FAIL\timport/path\t0.3s" or "FAIL\timport/path [build failed]"
						pkg, detail, _ := strings.Cut(strings.ReplaceAll(rest, "\t", " "), " ")
						failures = append(failures, failure(m, pkg, detail, failed, goFlags))
						failed = nil
					} else if strings.HasPrefix(line, "ok  ") {
						failed = nil
					}
					mu.Unlock()
				}
				if err != nil {
					break
				}
			}
			errs[i] = cmd.Wait()
		}()
	}
	wg.Wait()
	if summarize {
		sort.Strings(failures)
		for _, f := range failures {
			note("%s", f)
		}
	}
	for _, err := range errs {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if summarize && len(failures) == 0 {
				note("FAIL: go test exited with status %d; see its output above", exit.ExitCode())
			}
			return exitCode(exit.ExitCode())
		}
		if err != nil {
			return err
		}
	}
	if summarize {
		if pl.Skipped > 0 {
			note("PASS: every selected test passed; the other %d packages cannot be affected by these changes", pl.Skipped)
		} else {
			note("PASS: every selected test passed")
		}
	}
	return nil
}

// failure describes a failed package of go test output for the summary.
func failure(m *module, importPath, detail string, tests, goFlags []string) string {
	pkg := importPath
	rerun := append([]string{"go", "test"}, withoutFlag(goFlags, "run")...)
	what := "no test reported failing (a panic, timeout or TestMain?)"
	switch {
	case strings.Contains(detail, "build failed") || strings.Contains(detail, "setup failed"):
		what = strings.Trim(detail, "[]")
	case len(tests) > 0:
		what = strings.Join(tests, ", ")
		rerun = append(rerun, "-run", "^("+strings.Join(tests, "|")+")$")
	}
	if p := m.byPath[importPath]; p != nil {
		pkg = "./" + p.rel
	}
	return fmt.Sprintf("FAIL %s: %s; rerun: %s", m.short(importPath), what, strings.Join(quote(append(rerun, pkg)), " "))
}

// hasFlag reports whether go command flags set name, as -name, --name,
// -name=v or -test.name.
func hasFlag(flags []string, name string) bool {
	for _, f := range flags {
		if !strings.HasPrefix(f, "-") {
			continue
		}
		f, _, _ = strings.Cut(strings.TrimLeft(f, "-"), "=")
		if f == name || f == "test."+name {
			return true
		}
	}
	return false
}

// withoutFlag drops a flag that takes a value, such as -run, from flags.
func withoutFlag(flags []string, name string) []string {
	var out []string
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		n, _, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if strings.HasPrefix(f, "-") && (n == name || n == "test."+name) {
			if !hasValue {
				i++
			}
			continue
		}
		out = append(out, f)
	}
	return out
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
