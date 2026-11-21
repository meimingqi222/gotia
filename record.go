package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mapVersion = 2

// Directives in test files for code coverage cannot see.
const (
	// A "//gotia:package" line anywhere in a test file keeps its package out
	// of the map: whenever the package is affected, all its tests run.
	packageDirective = "//gotia:package"
	// A "//gotia:always" line in a test's doc comment runs that test whenever
	// its package is affected.
	alwaysDirective = "//gotia:always"
)

// coverMap records, for one commit, which functions each top-level test
// executed. Packages and files are module-relative, so the map applies to any
// checkout of the module.
type coverMap struct {
	Version    int               `json:"version"`
	Base       string            `json:"base"`
	Env        map[string]string `json:"env"`         // buildEnvKeys at recording
	BuildFlags []string          `json:"build_flags"` // go test build flags given to record
	Packages   map[string]int    `json:"packages"`    // package dir → recorded top-level tests
	Tests      []string          `json:"tests"`       // "pkgdir TestName"
	Always     []int             `json:"always"`      // tests to run whenever their package is affected
	Funcs      map[string][]int  `json:"funcs"`       // "dir/file.go:Func" → tests that ran it
}

func (c *coverMap) test(i int) (pkg, name string) {
	pkg, name, _ = strings.Cut(c.Tests[i], " ")
	return pkg, name
}

func defaultMapPath(m *module) (string, error) {
	if p := os.Getenv("GOTIA_MAP"); p != "" {
		return filepath.Abs(p)
	}
	out, err := output(m.root, "git", "rev-parse", "--git-path", "gotia")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(m.root, out)
	}
	// One map per module, so a repository with several modules keeps them apart.
	prefix, _ := output(m.root, "git", "rev-parse", "--show-prefix")
	name := strings.Trim(strings.ReplaceAll(prefix, "/", "_"), "_")
	if name == "" {
		name = "root"
	}
	return filepath.Join(out, name+".json"), nil
}

// readMap returns the map at file, or nil and the reason it cannot be used.
// replace is true when no setting explains that, so recording it again is
// always right: the map is missing, from another gotia version, unreadable,
// or its commit is gone.
func readMap(m *module, file string) (c *coverMap, why string, replace bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "no coverage map (run `gotia record` for per-test selection)", true
	}
	c = &coverMap{}
	if err := json.Unmarshal(data, c); err != nil || c.Version != mapVersion {
		return nil, "the coverage map is from another gotia version (run `gotia record`)", true
	}
	env, err := m.buildEnv()
	if err != nil {
		return nil, err.Error(), false
	}
	for _, k := range buildEnvKeys {
		if c.Env[k] != env[k] {
			return nil, fmt.Sprintf("the coverage map was recorded with %s=%q, not %q (run `gotia record`)", k, c.Env[k], env[k]), false
		}
	}
	if _, err := output(m.root, "git", "cat-file", "-e", c.Base+"^{commit}"); err != nil {
		return nil, "the coverage map's commit is gone (run `gotia record`)", true
	}
	return c, "", false
}

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	mapFlag := fs.String("map", "", "map file (default: inside .git, or $GOTIA_MAP)")
	full := fs.Bool("full", false, "record every test, even when the map could be updated incrementally")
	timeout := fs.Duration("timeout", 2*time.Minute, "limit for each test")
	workers := fs.Int("j", runtime.NumCPU(), "tests run at once")
	ignore := fs.String("ignore", envOr("GOTIA_IGNORE", defaultIgnore), "comma-separated globs of non-Go files that no test reads (also $GOTIA_IGNORE)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gotia record [flags] [-- go test build flags, such as -tags]\n\nRecords which functions each test of HEAD executes, running each test alone\nwith coverage in a temporary checkout. With a usable map it re-records only the\ntests that the commits since the map's commit can affect, and keeps the rest.\nUncommitted changes are not recorded.")
		fs.PrintDefaults()
	}
	own, buildFlags := splitArgs(fs, args)
	fs.Parse(own)
	m, err := loadModule(".", buildFlags)
	if err != nil {
		return err
	}
	file := *mapFlag
	if file == "" {
		if file, err = defaultMapPath(m); err != nil {
			return err
		}
	}
	_, err = record(m, recordOptions{file: file, full: *full, timeout: *timeout, workers: *workers,
		buildFlags: buildFlags, ignore: splitList(*ignore)})
	return err
}

type recordOptions struct {
	file       string
	full       bool
	timeout    time.Duration
	workers    int
	buildFlags []string
	ignore     []string
}

type recordStats struct {
	ran, kept int
}

// scope is what an incremental record runs again: every test of a package in
// whole, or the named tests. Tests outside it keep their recorded functions.
type scope struct {
	whole   map[string]bool            // package dir
	tests   map[string]map[string]bool // package dir → test names
	effects []string
}

func (sc *scope) rerun(rel, name string) bool {
	return sc == nil || sc.whole[rel] || sc.tests[rel][name]
}

// incrementalScope selects the tests the commits from old.Base to the
// checkout can affect, the same way gotia test selects them for a change.
// A test outside it ran only functions that did not change, so it still runs
// the same functions. It returns nil and the reason when every test must run.
func incrementalScope(wm *module, old *coverMap, ignore []string) (*scope, string) {
	files, unseen, err := wm.changedFiles(old.Base)
	if err != nil {
		return nil, err.Error()
	}
	p := newPlanner(wm, old, old.Base, ignore)
	for _, rel := range unseen {
		p.unseen(rel)
	}
	sc := &scope{whole: map[string]bool{}, tests: map[string]map[string]bool{}}
	for _, rel := range files {
		dir := path.Dir(rel)
		current, err := os.ReadFile(filepath.Join(wm.root, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) && path.Ext(rel) == ".go" && wm.byDir[dir] == nil && !strings.HasPrefix(rel, "../") {
			// The package is gone; its tests leave the map, and the packages
			// that used it changed too.
			p.effect("%s: its package was removed", rel)
			continue
		}
		if pkg := wm.byDir[dir]; pkg != nil && strings.HasSuffix(rel, "_test.go") && gotiaDirectives(p.before(rel)) != gotiaDirectives(current) {
			sc.whole[pkg.rel] = true
			p.effect("%s: a //gotia: directive changed → package %s", rel, pkg.rel)
		}
		p.file(rel)
	}
	p.finish(false)
	if p.all {
		for _, e := range p.effects {
			if strings.HasSuffix(e, "every package") || strings.Contains(e, "every package (") {
				return nil, e
			}
		}
		return nil, "a change affects every package"
	}
	for ip := range p.whole {
		sc.whole[wm.byPath[ip].rel] = true
	}
	for ip, names := range p.tests {
		if rel := wm.byPath[ip].rel; !sc.whole[rel] {
			sc.tests[rel] = names
		}
	}
	sc.effects = p.effects
	return sc, ""
}

func gotiaDirectives(src []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "//gotia:") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// lockMap takes the lock that lets one gotia record at a time write the
// map: an operating system lock on the map's lock file, which ends with the
// process that holds it however that process ends.
func lockMap(file string) (unlock func(), err error) {
	lock := file + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		f.Close()
		if err == nil {
			err = errors.New("another gotia record is updating the map")
		}
		return nil, err
	}
	return func() {
		_ = unlockFile(f)
		f.Close()
	}, nil
}

// mapLocked reports whether a gotia record is updating the map.
func mapLocked(file string) bool {
	f, err := os.OpenFile(file+".lock", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	ok, err := tryLock(f)
	if ok {
		_ = unlockFile(f)
	}
	return err == nil && !ok
}

func record(m *module, o recordOptions) (recordStats, error) {
	var stats recordStats
	start := time.Now()
	unlock, err := lockMap(o.file)
	if err != nil {
		return stats, err
	}
	defer unlock()
	head, err := output(m.root, "git", "rev-parse", "HEAD")
	if err != nil {
		return stats, err
	}
	prefix, err := output(m.root, "git", "rev-parse", "--show-prefix")
	if err != nil {
		return stats, err
	}
	var old *coverMap
	why := "-full given"
	if !o.full {
		old, why, _ = readMap(m, o.file)
		why, _, _ = strings.Cut(why, " (run")
		if old != nil && !slices.Equal(old.BuildFlags, o.buildFlags) {
			why = fmt.Sprintf("the map was recorded with build flags %q", old.BuildFlags)
			if len(old.BuildFlags) == 0 {
				why = "the map was recorded without build flags"
			}
			old = nil
		}
		if old != nil && old.Base == head {
			note("the map is already at %.7s; nothing to record (-full records every test again)", head)
			return stats, nil
		}
	}
	tmp, err := os.MkdirTemp("", "gotia-record-")
	if err != nil {
		return stats, err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	// Forget the worktrees of records that were killed.
	_, _ = output(m.root, "git", "worktree", "prune")
	if _, err := output(m.root, "git", "worktree", "add", "--detach", src, head); err != nil {
		return stats, err
	}
	defer func() {
		_, _ = output(m.root, "git", "worktree", "remove", "--force", src)
	}()
	wm, err := loadModule(filepath.Join(src, filepath.FromSlash(prefix)), o.buildFlags)
	if err != nil {
		return stats, err
	}
	var sc *scope
	if old != nil {
		if sc, why = incrementalScope(wm, old, o.ignore); sc == nil {
			old = nil
		} else {
			note("updating the map from %.7s to %.7s", old.Base, head)
			for _, e := range sc.effects {
				note("%s", e)
			}
		}
	}
	if old == nil && !o.full {
		note("recording every test: %s", why)
	}
	env, err := wm.buildEnv()
	if err != nil {
		return stats, err
	}
	c := &coverMap{Version: mapVersion, Base: head, Env: env, BuildFlags: o.buildFlags,
		Packages: map[string]int{}, Funcs: map[string][]int{}}
	// Tests kept from the old map, by their index there.
	oldIndex, oldByPkg, oldAlways := map[string]int{}, map[string][]int{}, map[int]bool{}
	oldToNew := map[int]int{}
	if old != nil {
		for i, t := range old.Tests {
			rel, _ := old.test(i)
			oldIndex[t] = i
			oldByPkg[rel] = append(oldByPkg[rel], i)
		}
		for _, i := range old.Always {
			oldAlways[i] = true
		}
	}
	keep := func(i int) {
		oldToNew[i] = len(c.Tests)
		if oldAlways[i] {
			c.Always = append(c.Always, len(c.Tests))
		}
		c.Tests = append(c.Tests, old.Tests[i])
	}

	type job struct {
		pkg   *goPackage
		bin   string
		name  string
		index int
	}
	var jobs []job
	var skipped []string
	type built struct {
		pkg    *goPackage
		bin    string
		always map[string]bool
		err    error
	}
	var builds []built
	for _, pkg := range wm.packages {
		if len(pkg.TestGoFiles)+len(pkg.XTestGoFiles) == 0 {
			continue
		}
		whole, always, err := testDirectives(pkg)
		switch {
		case err != nil:
			return stats, err
		case whole:
			skipped = append(skipped, pkg.rel)
		case sc == nil || sc.whole[pkg.rel] || len(sc.tests[pkg.rel]) > 0 || old.Packages[pkg.rel] == 0:
			builds = append(builds, built{pkg: pkg, always: always})
		default:
			for _, i := range oldByPkg[pkg.rel] {
				keep(i)
			}
			c.Packages[pkg.rel] = len(oldByPkg[pkg.rel])
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.NumCPU()/2))
	for i := range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			b := &builds[i]
			b.bin = filepath.Join(tmp, fmt.Sprintf("p%d.test", i))
			if runtime.GOOS == "windows" {
				b.bin += ".exe"
			}
			args := append([]string{"test", "-c", "-cover", "-covermode=set", "-coverpkg=./...", "-o", b.bin}, o.buildFlags...)
			_, b.err = output(wm.root, "go", append(args, "./"+b.pkg.rel)...)
		}()
	}
	wg.Wait()
	for _, b := range builds {
		if b.err != nil {
			return stats, fmt.Errorf("build tests of %s: %w", b.pkg.rel, b.err)
		}
		list := exec.Command(b.bin, "-test.list", ".")
		list.Dir = b.pkg.Dir
		out, err := list.Output()
		if err != nil {
			return stats, fmt.Errorf("list tests of %s: %w", b.pkg.rel, err)
		}
		n := 0
		for _, name := range strings.Fields(string(out)) {
			if !isTestFunc(name) {
				continue
			}
			n++
			if i, had := oldIndex[b.pkg.rel+" "+name]; had && !sc.rerun(b.pkg.rel, name) {
				keep(i)
				continue
			}
			if b.always[name] {
				c.Always = append(c.Always, len(c.Tests))
			} else {
				jobs = append(jobs, job{b.pkg, b.bin, name, len(c.Tests)})
			}
			c.Tests = append(c.Tests, b.pkg.rel+" "+name)
		}
		if n > 0 {
			c.Packages[b.pkg.rel] = n
		}
	}
	if old != nil {
		for fn, tests := range old.Funcs {
			for _, i := range tests {
				if j, ok := oldToNew[i]; ok {
					c.Funcs[fn] = append(c.Funcs[fn], j)
				}
			}
		}
	}
	stats.ran, stats.kept = len(jobs), len(oldToNew)
	if old != nil {
		note("recording %d tests in %d packages with %d workers; keeping %d", len(jobs), len(builds), o.workers, stats.kept)
	} else {
		note("recording %d tests in %d packages at %.7s with %d workers", len(c.Tests), len(c.Packages), head, o.workers)
	}
	if len(skipped) > 0 {
		note("not recorded (%s): %s", packageDirective, strings.Join(skipped, " "))
	}

	index := &funcIndex{m: wm, files: map[string][]funcSpan{}}
	goflags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -cover -covermode=set -coverpkg=" + wm.path + "/...")
	goenv, err := coverGoEnv(wm, tmp, goflags)
	if err != nil {
		return stats, err
	}
	type result struct {
		index int
		funcs []string
		err   error
	}
	work, results := make(chan job), make(chan result)
	for range max(1, o.workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				dir := filepath.Join(tmp, "cov", strconv.Itoa(j.index))
				r := result{index: j.index}
				if r.err = os.MkdirAll(dir, 0o755); r.err == nil {
					r.funcs, r.err = runCovered(j.pkg, j.bin, j.name, dir, o.timeout, goflags, goenv, index)
				}
				_ = os.RemoveAll(dir)
				results <- r
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			work <- j
		}
		close(work)
		wg.Wait()
		close(results)
	}()
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, c.Tests[r.index])
			c.Always = append(c.Always, r.index)
			continue
		}
		for _, fn := range r.funcs {
			c.Funcs[fn] = append(c.Funcs[fn], r.index)
		}
	}
	sort.Ints(c.Always)
	for _, t := range c.Funcs {
		sort.Ints(t)
	}
	sort.Strings(failed)
	if len(failed) > 0 {
		note("%d tests failed while recording and will always run when affected: %s", len(failed), strings.Join(failed, ", "))
	}
	data, err := json.Marshal(c)
	if err != nil {
		return stats, err
	}
	if err := os.MkdirAll(filepath.Dir(o.file), 0o755); err != nil {
		return stats, err
	}
	if err := os.WriteFile(o.file+".tmp", data, 0o644); err != nil {
		return stats, err
	}
	if err := os.Rename(o.file+".tmp", o.file); err != nil {
		return stats, err
	}
	note("recorded %d tests at %.7s in %s (%d run, %d kept)", len(c.Tests), head, time.Since(start).Round(time.Second), stats.ran, stats.kept)
	if dirty, _ := output(m.root, "git", "status", "--porcelain", "--untracked-files=no", "."); dirty != "" {
		note("uncommitted changes were not recorded; `gotia test` selects for them against %.7s", head)
	}
	return stats, nil
}

// coverGoEnv writes a copy of the user's go env file that adds coverage to
// GOFLAGS. A test that builds a binary often resets GOFLAGS in its
// environment; go then reads GOFLAGS from this file, so the binary is still
// instrumented.
func coverGoEnv(m *module, tmp, goflags string) (string, error) {
	var lines []string
	if file, _ := output(m.root, "go", "env", "GOENV"); file != "" && file != "off" {
		for _, line := range readLines(file) {
			if !strings.HasPrefix(line, "GOFLAGS=") {
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines, "GOFLAGS="+goflags)
	file := filepath.Join(tmp, "goenv")
	return file, os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// runCovered runs one test with coverage and returns the functions it ran.
// Subprocesses share its GOCOVERDIR: a re-executed test binary writes its
// counters there, and so does a binary the test builds with go build, which
// picks -cover up from GOFLAGS or, when the test clears that, from GOENV.
func runCovered(pkg *goPackage, bin, name, dir string, timeout time.Duration, goflags, goenv string, index *funcIndex) ([]string, error) {
	profile := filepath.Join(dir, "profile.txt")
	cmd := exec.Command(bin, "-test.run", "^"+regexp.QuoteMeta(name)+"$", "-test.count=1",
		"-test.timeout="+timeout.String(), "-test.gocoverdir="+dir, "-test.coverprofile="+profile)
	cmd.Dir = pkg.Dir
	cmd.Env = append(os.Environ(), "GOCOVERDIR="+dir, "GOFLAGS="+goflags, "GOENV="+goenv, "GOTIA_RECORD=1")
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	metas, _ := filepath.Glob(filepath.Join(dir, "covmeta.*"))
	if len(metas) > 1 {
		// Counters of other binaries: merge everything in the directory.
		profile = filepath.Join(dir, "merged.txt")
		if _, err := output(dir, "go", "tool", "covdata", "textfmt", "-i="+dir, "-o="+profile); err != nil {
			return nil, err
		}
	}
	return index.profileFuncs(profile)
}

func testDirectives(pkg *goPackage) (whole bool, always map[string]bool, err error) {
	always = map[string]bool{}
	for _, name := range append(append([]string(nil), pkg.TestGoFiles...), pkg.XTestGoFiles...) {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return false, nil, err
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				whole = whole || strings.HasPrefix(c.Text, packageDirective)
			}
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Doc != nil {
				for _, c := range fn.Doc.List {
					if strings.HasPrefix(c.Text, alwaysDirective) {
						always[fn.Name.Name] = true
					}
				}
			}
		}
	}
	return whole, always, nil
}

type funcSpan struct {
	start, end int
	key        string
}

// funcIndex maps coverage blocks of the recorded checkout to the functions
// that contain them.
type funcIndex struct {
	m     *module
	mu    sync.Mutex
	files map[string][]funcSpan
}

func (x *funcIndex) spans(rel string) []funcSpan {
	x.mu.Lock()
	defer x.mu.Unlock()
	if spans, ok := x.files[rel]; ok {
		return spans
	}
	var spans []funcSpan
	fset := token.NewFileSet()
	if f, err := parser.ParseFile(fset, filepath.Join(x.m.root, filepath.FromSlash(rel)), nil, parser.SkipObjectResolution); err == nil {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				spans = append(spans, funcSpan{fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line, funcKey(fn)})
			}
		}
	}
	x.files[rel] = spans
	return spans
}

// profileFuncs lists the module functions with an executed block in a
// set-mode profile ("import/path/file.go:line.col,line.col statements count").
func (x *funcIndex) profileFuncs(profile string) ([]string, error) {
	data, err := os.ReadFile(profile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing was instrumented
		}
		return nil, err
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "mode:") || strings.HasSuffix(line, " 0") {
			continue
		}
		colon := strings.LastIndexByte(line, ':')
		if colon < 0 {
			continue
		}
		dot := strings.IndexByte(line[colon+1:], '.')
		if dot < 0 {
			continue
		}
		startLine, err := strconv.Atoi(line[colon+1 : colon+1+dot])
		pkg := x.m.byPath[path.Dir(line[:colon])]
		if err != nil || pkg == nil {
			continue
		}
		rel := path.Join(pkg.rel, path.Base(line[:colon]))
		for _, s := range x.spans(rel) {
			if s.start <= startLine && startLine <= s.end {
				seen[rel+":"+s.key] = true
				break
			}
		}
	}
	funcs := make([]string, 0, len(seen))
	for fn := range seen {
		funcs = append(funcs, fn)
	}
	return funcs, nil
}
