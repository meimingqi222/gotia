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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mapVersion = 1

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
	Version   int              `json:"version"`
	Base      string           `json:"base"`
	GOOS      string           `json:"goos"`
	GOARCH    string           `json:"goarch"`
	GoVersion string           `json:"go_version"`
	Packages  map[string]int   `json:"packages"` // package dir → recorded top-level tests
	Tests     []string         `json:"tests"`    // "pkgdir TestName"
	Always    []int            `json:"always"`   // tests to run whenever their package is affected
	Funcs     map[string][]int `json:"funcs"`    // "dir/file.go:Func" → tests that ran it
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
func readMap(m *module, file string) (*coverMap, string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "no coverage map (run `gotia record` for per-test selection)"
	}
	var c coverMap
	if err := json.Unmarshal(data, &c); err != nil || c.Version != mapVersion {
		return nil, "the coverage map is from another gotia version (run `gotia record`)"
	}
	if c.GOOS != runtime.GOOS || c.GOARCH != runtime.GOARCH || c.GoVersion != m.goVersion() {
		return nil, fmt.Sprintf("the coverage map is for %s/%s %s (run `gotia record`)", c.GOOS, c.GOARCH, c.GoVersion)
	}
	if _, err := output(m.root, "git", "cat-file", "-e", c.Base+"^{commit}"); err != nil {
		return nil, "the coverage map's commit is gone (run `gotia record`)"
	}
	return &c, ""
}

func cmdRecord(args []string) error {
	own, buildFlags := splitArgs(args)
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	mapFlag := fs.String("map", "", "map file (default: inside .git, or $GOTIA_MAP)")
	timeout := fs.Duration("timeout", 2*time.Minute, "limit for each test")
	workers := fs.Int("j", runtime.NumCPU(), "tests run at once")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gotia record [flags] [-- go test build flags, such as -tags]\n\nRuns every test of HEAD alone, with coverage, in a temporary checkout and\nrecords which functions each one executed. Uncommitted changes are not recorded.")
		fs.PrintDefaults()
	}
	fs.Parse(own)
	start := time.Now()
	m, err := loadModule(".")
	if err != nil {
		return err
	}
	file := *mapFlag
	if file == "" {
		if file, err = defaultMapPath(m); err != nil {
			return err
		}
	}
	head, err := output(m.root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	prefix, err := output(m.root, "git", "rev-parse", "--show-prefix")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "gotia-record-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "src")
	if _, err := output(m.root, "git", "worktree", "add", "--detach", src, head); err != nil {
		return err
	}
	defer func() {
		_, _ = output(m.root, "git", "worktree", "remove", "--force", src)
	}()
	wm, err := loadModule(filepath.Join(src, filepath.FromSlash(prefix)))
	if err != nil {
		return err
	}

	c := &coverMap{Version: mapVersion, Base: head, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GoVersion: wm.goVersion(), Packages: map[string]int{}, Funcs: map[string][]int{}}
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
	var candidates []*goPackage
	for _, pkg := range wm.packages {
		if len(pkg.TestGoFiles)+len(pkg.XTestGoFiles) > 0 {
			candidates = append(candidates, pkg)
		}
	}
	builds := make([]built, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.NumCPU()/2))
	for i, pkg := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			b := built{pkg: pkg}
			whole, always, err := testDirectives(pkg)
			if err != nil || whole {
				b.err = err
				builds[i] = b
				return
			}
			b.always = always
			b.bin = filepath.Join(tmp, fmt.Sprintf("p%d.test", i))
			if runtime.GOOS == "windows" {
				b.bin += ".exe"
			}
			args := append([]string{"test", "-c", "-cover", "-covermode=set", "-coverpkg=./...", "-o", b.bin}, buildFlags...)
			_, b.err = output(wm.root, "go", append(args, "./"+pkg.rel)...)
			builds[i] = b
		}()
	}
	wg.Wait()
	for _, b := range builds {
		if b.err != nil {
			return fmt.Errorf("build tests of %s: %w", b.pkg.rel, b.err)
		}
		if b.bin == "" {
			skipped = append(skipped, b.pkg.rel)
			continue
		}
		list := exec.Command(b.bin, "-test.list", ".")
		list.Dir = b.pkg.Dir
		out, err := list.Output()
		if err != nil {
			return fmt.Errorf("list tests of %s: %w", b.pkg.rel, err)
		}
		n := 0
		for _, name := range strings.Fields(string(out)) {
			if !isTestFunc(name) {
				continue
			}
			n++
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
	note("recording %d tests in %d packages at %.7s with %d workers", len(c.Tests), len(c.Packages), head, *workers)
	if len(skipped) > 0 {
		note("not recorded (%s): %s", packageDirective, strings.Join(skipped, " "))
	}

	index := &funcIndex{m: wm, files: map[string][]funcSpan{}}
	goflags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -cover -covermode=set -coverpkg=" + wm.path + "/...")
	goenv, err := coverGoEnv(wm, tmp, goflags)
	if err != nil {
		return err
	}
	type result struct {
		index int
		funcs []string
		err   error
	}
	work, results := make(chan job), make(chan result)
	for range max(1, *workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				dir := filepath.Join(tmp, "cov", strconv.Itoa(j.index))
				r := result{index: j.index}
				if r.err = os.MkdirAll(dir, 0o755); r.err == nil {
					r.funcs, r.err = runCovered(j.pkg, j.bin, j.name, dir, *timeout, goflags, goenv, index)
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
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(file+".tmp", data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(file+".tmp", file); err != nil {
		return err
	}
	note("recorded %d tests at %.7s in %s", len(c.Tests), head, time.Since(start).Round(time.Second))
	if dirty, _ := output(m.root, "git", "status", "--porcelain", "--untracked-files=no", "."); dirty != "" {
		note("uncommitted changes were not recorded; `gotia test` selects for them against %.7s", head)
	}
	return nil
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
