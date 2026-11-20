package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture is a two-package module: cmd imports lib; cmd's tests are not in
// the map, so a change cmd depends on runs cmd whole.
var fixture = map[string]string{
	"lib/lib.go": `package lib

const Limit = 1

var derived = Limit * 2

func Run() int { return derived }

func Other() int { return 0 }

func Unused() {}

func (a *Agent) Close() {}

type Agent struct{}
`,
	"lib/lib_test.go": `package lib

import "testing"

func TestRun(t *testing.T) { Run() }

func TestLimit(t *testing.T) { _ = Limit }

func helper() int { return 1 }
`,
	"lib/testdata/in.json": `{}`,
	"cmd/main.go": `package main

import "example.com/m/lib"

func main() { println(lib.Limit) }
`,
}

func fixtureModule(t *testing.T) *module {
	root := t.TempDir()
	for rel, text := range fixture {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &module{root: root, path: "example.com/m", byPath: map[string]*goPackage{}, byDir: map[string]*goPackage{}}
	for _, pkg := range []*goPackage{
		{rel: "lib", ImportPath: "example.com/m/lib", Name: "lib", GoFiles: []string{"lib.go"}, TestGoFiles: []string{"lib_test.go"}},
		{rel: "cmd", ImportPath: "example.com/m/cmd", Name: "main", GoFiles: []string{"main.go"}, Imports: []string{"example.com/m/lib"}},
	} {
		pkg.Dir = filepath.Join(root, pkg.rel)
		m.packages = append(m.packages, pkg)
		m.byPath[pkg.ImportPath], m.byDir[pkg.rel] = pkg, pkg
	}
	return m
}

func fixtureMap() *coverMap {
	// Ten recorded lib tests, so a few selected ones stay below half.
	c := &coverMap{Base: "base", Packages: map[string]int{"lib": 10}, Funcs: map[string][]int{
		"lib/lib.go:Run":            {0},
		"lib/lib.go:Other":          {2},
		"lib/lib.go:(*Agent).Close": {3},
		"cmd/main.go:main":          {5}, // TestE runs the built command
	}}
	for _, name := range []string{"TestRun", "TestLimit", "TestOther", "TestClose", "TestAlways", "TestE", "TestF", "TestG", "TestH", "TestI"} {
		c.Tests = append(c.Tests, "lib "+name)
	}
	c.Always = []int{4}
	return c
}

func TestPlan(t *testing.T) {
	type want struct {
		tests     map[string][]string
		whole     []string
		uncovered []string
	}
	lib := func(names ...string) map[string][]string { return map[string][]string{"lib": names} }
	for _, tc := range []struct {
		name, file, from, to string
		noMap                bool
		want                 want
	}{
		{name: "a changed body selects the tests that ran it", file: "lib/lib.go",
			from: "func Other() int { return 0 }", to: "func Other() int { return 1 }",
			want: want{tests: lib("TestAlways", "TestOther"), whole: []string{"cmd"}}},
		{name: "comments and layout select nothing", file: "lib/lib.go",
			from: "func Other() int { return 0 }", to: "// Other is zero.\nfunc Other() int {\n\treturn 0\n}",
			want: want{}},
		{name: "a const reaches its users through other declarations and imports", file: "lib/lib.go",
			from: "const Limit = 1", to: "const Limit = 2",
			want: want{tests: lib("TestAlways", "TestE", "TestLimit", "TestRun"), whole: []string{"cmd"}}},
		{name: "a changed type falls back to packages", file: "lib/lib.go",
			from: "type Agent struct{}", to: "type Agent struct{ n int }",
			want: want{whole: []string{"cmd", "lib"}}},
		{name: "a new method may satisfy an interface", file: "lib/lib.go",
			from: "type Agent", to: "func (a *Agent) String() string { return \"\" }\n\ntype Agent",
			want: want{whole: []string{"cmd", "lib"}}},
		{name: "a side-effect import falls back to packages", file: "lib/lib.go",
			from: "const Limit", to: "import _ \"embed\"\n\nconst Limit",
			want: want{whole: []string{"cmd", "lib"}}},
		{name: "a fallback reaches tests that run the package in a subprocess", file: "cmd/main.go",
			from: "func main()", to: "import _ \"embed\"\n\nfunc main()",
			want: want{tests: lib("TestE"), whole: []string{"cmd"}}},
		{name: "a function no test ran is reported", file: "lib/lib.go",
			from: "func Unused() {}", to: "func Unused() { _ = 1 }",
			want: want{tests: lib("TestAlways"), whole: []string{"cmd"}, uncovered: []string{"lib/lib.go:Unused"}}},
		{name: "a changed test runs by name", file: "lib/lib_test.go",
			from: "{ Run() }", to: "{ Run(); Run() }",
			want: want{tests: lib("TestAlways", "TestRun")}},
		{name: "a changed test helper runs its package", file: "lib/lib_test.go",
			from: "return 1", to: "return 2",
			want: want{whole: []string{"lib"}}},
		{name: "test data runs its package and dependents", file: "lib/testdata/in.json",
			want: want{whole: []string{"cmd", "lib"}}},
		{name: "an ignored file selects nothing", file: "README.md",
			want: want{}},
		{name: "an unknown file runs everything", file: "Makefile",
			want: want{whole: []string{"cmd", "lib"}}},
		{name: "without a map a change runs its package and dependents", file: "lib/lib.go", noMap: true,
			from: "func Other() int { return 0 }", to: "func Other() int { return 1 }",
			want: want{whole: []string{"cmd", "lib"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureModule(t)
			if tc.from != "" {
				path := filepath.Join(m.root, filepath.FromSlash(tc.file))
				text := fixture[tc.file]
				if !strings.Contains(text, tc.from) {
					t.Fatalf("fixture lacks %q", tc.from)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(text, tc.from, tc.to, 1)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := fixtureMap()
			if tc.noMap {
				c = nil
			}
			p := newPlanner(m, c, "base", splitList(defaultIgnore))
			p.before = func(rel string) []byte {
				if text, ok := fixture[rel]; ok {
					return []byte(text)
				}
				return nil
			}
			p.file(tc.file)
			pl := p.finish(false)
			if tc.want.tests == nil {
				tc.want.tests = map[string][]string{}
			}
			if !reflect.DeepEqual(pl.Tests, tc.want.tests) || strings.Join(pl.Whole, " ") != strings.Join(tc.want.whole, " ") ||
				strings.Join(pl.Uncovered, " ") != strings.Join(tc.want.uncovered, " ") {
				t.Fatalf("tests=%v whole=%v uncovered=%v\nwant tests=%v whole=%v uncovered=%v\neffects: %s",
					pl.Tests, pl.Whole, pl.Uncovered, tc.want.tests, tc.want.whole, tc.want.uncovered, strings.Join(pl.Effects, "; "))
			}
		})
	}
}

func TestDiffIgnoresLayoutButNotStatements(t *testing.T) {
	before := []byte("package p\n\nfunc F() {\n\treturn\n}\n")
	for _, tc := range []struct {
		after   string
		changed bool
	}{
		{"package p\n\n// F does nothing.\nfunc F() { return }\n", false},
		{"package p\n\nfunc F() {\n\treturn\n\tF()\n}\n", true},
	} {
		fc, err := diffGoFile(before, []byte(tc.after))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(fc.changed) > 0; got != tc.changed || fc.fallback != "" {
			t.Errorf("%q: changed=%v fallback=%q, want changed=%v", tc.after, fc.changed, fc.fallback, tc.changed)
		}
	}
}
