package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// plan is what gotia test decided to run and why.
type plan struct {
	Base      string              `json:"base"`               // commit the changes are measured against
	Map       bool                `json:"map"`                // whether per-test selection was used
	MapNote   string              `json:"map_note,omitempty"` // why not, or how stale it is
	Effects   []string            `json:"effects"`            // one line per change: what it selects and why
	Whole     []string            `json:"whole_packages"`     // packages run in full
	Tests     map[string][]string `json:"tests"`              // package → selected top-level tests
	Uncovered []string            `json:"uncovered,omitempty"`
	Skipped   int                 `json:"skipped_packages"` // packages not run at all
	Commands  [][]string          `json:"commands"`
}

type planner struct {
	m       *module
	c       *coverMap
	base    string
	all     bool
	roots   map[string]bool            // package and dependents run whole
	whole   map[string]bool            // package runs whole
	tests   map[string]map[string]bool // package → tests by name
	prod    map[string]bool            // packages with function-level production changes
	touched map[string]bool            // packages whose own tests changed
	names   map[string]map[string]bool // package → changed var and const names
	effects []string
	uncover []string
	parsed  []*parsedFile
	ignore  []string
	fromMap map[int]bool            // tests selected through the map, for counts
	before  func(rel string) []byte // a file at base; nil when it did not exist
}

func newPlanner(m *module, c *coverMap, base string, ignore []string) *planner {
	return &planner{m: m, c: c, base: base, ignore: ignore, before: func(rel string) []byte { return m.fileAt(base, rel) },
		roots: map[string]bool{}, whole: map[string]bool{}, tests: map[string]map[string]bool{},
		prod: map[string]bool{}, touched: map[string]bool{}, names: map[string]map[string]bool{}, fromMap: map[int]bool{}}
}

func (p *planner) effect(format string, args ...any) {
	p.effects = append(p.effects, fmt.Sprintf(format, args...))
}

func (p *planner) addTest(importPath, name string) {
	if p.tests[importPath] == nil {
		p.tests[importPath] = map[string]bool{}
	}
	p.tests[importPath][name] = true
}

// mapTests adds the tests that ran fn ("dir/file.go:Name") and returns how many.
func (p *planner) mapTests(fn string) int {
	n := 0
	for _, i := range p.c.Funcs[fn] {
		rel, name := p.c.test(i)
		if pkg := p.m.byDir[rel]; pkg != nil {
			p.addTest(pkg.ImportPath, name)
			p.fromMap[i] = true
			n++
		}
	}
	return n
}

func (p *planner) ignored(rel string) bool {
	for _, pat := range p.ignore {
		if prefix, ok := strings.CutSuffix(pat, "/**"); ok {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
			}
		} else if !strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, path.Base(rel)); ok {
				return true
			}
		} else if ok, _ := path.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// unseen marks a directory the build reads that git cannot show changes of.
func (p *planner) unseen(rel string) {
	p.all = true
	p.effect("%s: read by the build but outside the repository, so its changes are unknown → every package", rel)
}

func (p *planner) file(rel string) {
	m := p.m
	if rel == ".." || strings.HasPrefix(rel, "../") {
		if p.ignored(rel) {
			p.effect("%s: ignored", rel)
			return
		}
		p.all = true
		p.effect("%s: outside the module, read through go.work or a replace → every package", rel)
		return
	}
	if info, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(rel))); err == nil && info.IsDir() {
		n := 0
		for _, pkg := range m.packages {
			if rel == "." || pkg.rel == rel || strings.HasPrefix(pkg.rel, rel+"/") {
				p.roots[pkg.ImportPath] = true
				n++
			}
		}
		p.effect("%s/: directory → its %d packages and their dependents", rel, n)
		return
	}
	base := path.Base(rel)
	if base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum" || strings.HasPrefix(rel, "vendor/") {
		p.all = true
		p.effect("%s: dependencies changed → every package", rel)
		return
	}
	dir := path.Dir(rel)
	pkg := m.byDir[dir]
	if path.Ext(rel) != ".go" {
		if owner, embedded, testOnly := m.owner(rel); owner != nil {
			switch {
			case embedded && testOnly:
				p.whole[owner.ImportPath] = true
				p.effect("%s: embedded by tests → package %s", rel, owner.rel)
			case embedded:
				p.roots[owner.ImportPath] = true
				p.effect("%s: embedded → package %s and its dependents", rel, owner.rel)
			default:
				p.roots[owner.ImportPath] = true
				p.effect("%s: test data → package %s and its dependents", rel, owner.rel)
			}
			return
		}
		if p.ignored(rel) {
			p.effect("%s: ignored", rel)
			return
		}
		if pkg != nil {
			p.roots[pkg.ImportPath] = true
			p.effect("%s: file in package %s → it and its dependents", rel, pkg.rel)
			return
		}
		p.all = true
		p.effect("%s: not a Go file of any package → every package (ignore it with -ignore)", rel)
		return
	}
	if pkg == nil {
		p.all = true
		p.effect("%s: in no package of this build → every package", rel)
		return
	}
	for _, f := range pkg.IgnoredGoFiles {
		if f == base {
			p.effect("%s: not built for this platform; skipped", rel)
			return
		}
	}
	test := strings.HasSuffix(base, "_test.go")
	if p.c == nil {
		if test {
			p.whole[pkg.ImportPath] = true
			p.effect("%s: test file → package %s", rel, pkg.rel)
		} else {
			p.roots[pkg.ImportPath] = true
			p.effect("%s: → package %s and its dependents", rel, pkg.rel)
		}
		return
	}
	current, _ := os.ReadFile(filepath.Join(m.root, filepath.FromSlash(rel)))
	old := p.before(rel)
	if old != nil && string(old) == string(current) {
		p.effect("%s: unchanged since %.7s", rel, p.base)
		return
	}
	fc, err := diffGoFile(old, current)
	if err != nil {
		fc.fallback = "does not parse"
	}
	if fc.fallback != "" {
		if test {
			p.whole[pkg.ImportPath] = true
			p.effect("%s: %s → package %s", rel, fc.fallback, pkg.rel)
		} else {
			p.roots[pkg.ImportPath] = true
			p.effect("%s: %s → package %s and its dependents", rel, fc.fallback, pkg.rel)
		}
		return
	}
	if len(fc.changed)+len(fc.added)+len(fc.names) == 0 {
		p.effect("%s: only comments or layout changed", rel)
		return
	}
	owner := pkg.ImportPath
	if test && isXTest(pkg, base) {
		owner += "_test"
	}
	for _, name := range fc.names {
		if p.names[owner] == nil {
			p.names[owner] = map[string]bool{}
		}
		p.names[owner][name] = true
	}
	if test {
		p.touched[pkg.ImportPath] = true
		var byName, helpers []string
		for _, fn := range append(append([]string(nil), fc.changed...), fc.added...) {
			switch {
			case isTestFunc(fn):
				p.addTest(pkg.ImportPath, fn)
				byName = append(byName, fn)
			case !strings.HasPrefix(fn, "Benchmark"):
				helpers = append(helpers, fn)
			}
		}
		if len(helpers) > 0 {
			p.whole[pkg.ImportPath] = true
			p.effect("%s: test helper %s changed → package %s", rel, strings.Join(helpers, ", "), pkg.rel)
		} else if len(byName) > 0 {
			p.effect("%s: %s → run by name", rel, strings.Join(byName, ", "))
		}
		if len(fc.names) > 0 {
			p.effect("%s: %s changed → the tests that use them", rel, strings.Join(fc.names, ", "))
		}
		return
	}
	p.prod[pkg.ImportPath] = true
	before := len(p.fromMap)
	for _, fn := range fc.changed {
		if p.mapTests(rel+":"+fn) == 0 {
			p.uncover = append(p.uncover, rel+":"+fn)
		}
	}
	var what []string
	what = append(what, fc.changed...)
	if len(fc.names) > 0 {
		what = append(what, fc.names...)
	}
	if len(what) == 0 {
		p.effect("%s: only added %s, which nothing ran before", rel, strings.Join(fc.added, ", "))
		return
	}
	p.effect("%s: %s → %d tests that ran them", rel, strings.Join(what, ", "), len(p.fromMap)-before)
}

type parsedFile struct {
	pkg     *goPackage
	rel     string
	test    bool
	owner   string            // the package its unqualified names belong to
	imports map[string]string // local name → import path
	f       *ast.File
}

func isXTest(pkg *goPackage, base string) bool {
	for _, f := range pkg.XTestGoFiles {
		if f == base {
			return true
		}
	}
	return false
}

func (p *planner) files() []*parsedFile {
	if p.parsed != nil {
		return p.parsed
	}
	for _, pkg := range p.m.packages {
		lists := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.TestGoFiles, pkg.XTestGoFiles}
		for li, list := range lists {
			for _, name := range list {
				f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
				if err != nil {
					continue
				}
				pf := &parsedFile{pkg: pkg, rel: path.Join(pkg.rel, name), test: li >= 2, owner: pkg.ImportPath, imports: map[string]string{}, f: f}
				if li == 3 {
					pf.owner += "_test"
				}
				for _, is := range f.Imports {
					ip, _ := strconv.Unquote(is.Path.Value)
					local := path.Base(ip)
					if dep := p.m.byPath[ip]; dep != nil {
						local = dep.Name
					}
					if is.Name != nil {
						local = is.Name.Name
					}
					pf.imports[local] = ip
				}
				p.parsed = append(p.parsed, pf)
			}
		}
	}
	return p.parsed
}

// uses reports whether n refers to a changed name: unqualified in its own
// package, or qualified through an import. Shadowing makes it select more,
// never less.
func (p *planner) uses(pf *parsedFile, n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if ip, ok := pf.imports[id.Name]; ok && p.names[ip][x.Sel.Name] {
					found = true
				}
			}
		case *ast.Ident:
			found = p.names[pf.owner][x.Name]
		}
		return !found
	})
	return found
}

// resolveNames follows changed vars and consts to the declarations and
// functions that use them.
func (p *planner) resolveNames() {
	if len(p.names) == 0 {
		return
	}
	for grew := true; grew; {
		grew = false
		for _, pf := range p.files() {
			for _, decl := range pf.f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					if !p.uses(pf, vs) {
						continue
					}
					for _, name := range vs.Names {
						if p.names[pf.owner] == nil {
							p.names[pf.owner] = map[string]bool{}
						}
						if !p.names[pf.owner][name.Name] {
							p.names[pf.owner][name.Name] = true
							grew = true
						}
					}
				}
			}
		}
	}
	for _, pf := range p.files() {
		for _, decl := range pf.f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !p.uses(pf, fd) {
				continue
			}
			key := funcKey(fd)
			switch {
			case !pf.test:
				p.prod[pf.pkg.ImportPath] = true
				p.mapTests(pf.rel + ":" + key)
			case isTestFunc(key):
				p.addTest(pf.pkg.ImportPath, key)
			case !strings.HasPrefix(key, "Benchmark"):
				p.whole[pf.pkg.ImportPath] = true
			}
		}
	}
}

func (p *planner) finish(goFlagsRun bool) *plan {
	m := p.m
	p.resolveNames()
	pl := &plan{Base: p.base, Map: p.c != nil, Effects: p.effects, Whole: []string{}, Tests: map[string][]string{}, Uncovered: p.uncover, Commands: [][]string{}}
	if p.all {
		for _, pkg := range m.packages {
			p.whole[pkg.ImportPath] = true
		}
	}
	rootDeps := m.dependents(p.roots)
	for dep := range rootDeps {
		p.whole[dep] = true
	}
	// A test can run a package's code without importing it, in a binary it
	// builds or a process it starts. The map saw that code run, so those
	// tests are affected too.
	if p.c != nil && len(rootDeps) > 0 {
		dirs := map[string]bool{}
		for dep := range rootDeps {
			dirs[m.byPath[dep].rel] = true
		}
		for fn := range p.c.Funcs {
			file, _, _ := strings.Cut(fn, ":")
			if dirs[path.Dir(file)] {
				p.mapTests(fn)
			}
		}
	}
	// Function-level changes reach every package that depends on them: the
	// map covers recorded tests; the others run whole.
	affected := m.dependents(p.prod)
	for dep := range affected {
		if p.c == nil || p.c.Packages[m.byPath[dep].rel] == 0 {
			p.whole[dep] = true
		}
	}
	for pkg := range p.touched {
		affected[pkg] = true
	}
	for pkg := range p.whole {
		affected[pkg] = true
	}
	if p.c != nil {
		for _, i := range p.c.Always {
			rel, name := p.c.test(i)
			if pkg := m.byDir[rel]; pkg != nil && affected[pkg.ImportPath] {
				p.addTest(pkg.ImportPath, name)
			}
		}
	}
	for ip, names := range p.tests {
		recorded := 0
		if p.c != nil {
			recorded = p.c.Packages[m.byPath[ip].rel]
		}
		// Half a package costs about as much as all of it.
		if p.whole[ip] || goFlagsRun || 2*len(names) >= recorded {
			p.whole[ip] = true
			continue
		}
		var list []string
		for name := range names {
			list = append(list, name)
		}
		sort.Strings(list)
		pl.Tests[m.short(ip)] = list
	}
	for ip := range p.whole {
		pl.Whole = append(pl.Whole, m.short(ip))
	}
	sort.Strings(pl.Whole)
	sort.Strings(pl.Uncovered)
	run := map[string]bool{}
	for _, w := range pl.Whole {
		run[w] = true
	}
	for t := range pl.Tests {
		run[t] = true
	}
	pl.Skipped = len(m.packages) - len(run)
	return pl
}

// commands turns the plan into go test invocations with the user's flags.
func (pl *plan) commands(m *module, flags []string) {
	pkgArg := func(short string) string {
		if pkg := m.byDir[short]; pkg != nil {
			return "./" + pkg.rel
		}
		return short
	}
	if len(pl.Whole) > 0 {
		cmd := append([]string{"go", "test"}, flags...)
		for _, w := range pl.Whole {
			cmd = append(cmd, pkgArg(w))
		}
		pl.Commands = append(pl.Commands, cmd)
	}
	if len(pl.Tests) == 0 {
		return
	}
	var pkgs []string
	names := map[string]bool{}
	for pkg, tests := range pl.Tests {
		pkgs = append(pkgs, pkgArg(pkg))
		for _, t := range tests {
			names[t] = true
		}
	}
	sort.Strings(pkgs)
	var list []string
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	// A name shared by two selected packages runs in both: more, never fewer.
	cmd := append([]string{"go", "test"}, flags...)
	cmd = append(cmd, "-run", "^("+strings.Join(list, "|")+")$")
	pl.Commands = append(pl.Commands, append(cmd, pkgs...))
}

func (pl *plan) testCount() int {
	n := 0
	for _, t := range pl.Tests {
		n += len(t)
	}
	return n
}
