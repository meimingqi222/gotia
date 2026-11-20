package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestIncrementalRecordMatchesFull: updating a map across commits must give
// the map a full record of the new commit gives, while running only the
// tests those commits can affect.
func TestIncrementalRecordMatchesFull(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	write := func(files map[string]string) {
		for rel, text := range files {
			p := filepath.Join(root, filepath.FromSlash(rel))
			if text == "" {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.24\n",
		"a/a.go":         "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n",
		"a/a_test.go":    "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n\nfunc TestB(t *testing.T) { B() }\n",
		"b/b.go":         "package b\n\nimport \"example.com/m/a\"\n\nfunc UsesA() int { return a.A() }\n",
		"b/b_test.go":    "package b\n\nimport \"testing\"\n\nfunc TestUsesA(t *testing.T) { UsesA() }\n\nfunc TestNothing(t *testing.T) {}\n",
		"gone/g.go":      "package gone\n\nfunc G() {}\n",
		"gone/g_test.go": "package gone\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) { G() }\n",
	})
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	m, err := loadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := recordOptions{file: filepath.Join(t.TempDir(), "map.json"), timeout: time.Minute, workers: 2, ignore: splitList(defaultIgnore)}
	if _, err := record(m, opts); err != nil {
		t.Fatal(err)
	}
	// B now also runs A, TestA goes, TestC and package c arrive, package
	// gone leaves. TestUsesA and TestNothing ran nothing that changed.
	write(map[string]string{
		"a/a.go":         "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 + A() }\n",
		"a/a_test.go":    "package a\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n\nfunc TestC(t *testing.T) {}\n",
		"c/c.go":         "package c\n\nfunc C() {}\n",
		"c/c_test.go":    "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { C() }\n",
		"gone/g.go":      "",
		"gone/g_test.go": "",
	})
	git("add", "-A")
	git("commit", "-q", "-m", "two")
	if m, err = loadModule(root); err != nil {
		t.Fatal(err)
	}
	stats, err := record(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.kept != 2 {
		t.Errorf("incremental record kept %d tests, want TestUsesA and TestNothing", stats.kept)
	}
	incremental := readTestMap(t, m, opts.file)
	full := opts
	full.full, full.file = true, filepath.Join(t.TempDir(), "full.json")
	if _, err := record(m, full); err != nil {
		t.Fatal(err)
	}
	if want := readTestMap(t, m, full.file); !reflect.DeepEqual(incremental, want) {
		t.Fatalf("incremental map:\n%v\nfull map:\n%v", incremental, want)
	}
	if stats, err := record(m, opts); err != nil || stats.ran+stats.kept != 0 {
		t.Fatalf("a current map was recorded again: %+v %v", stats, err)
	}
}

// readTestMap is a map's content without its indices: each test with the
// functions it ran, and whether it always runs.
func readTestMap(t *testing.T, m *module, file string) map[string][]string {
	t.Helper()
	c, why := readMap(m, file)
	if c == nil {
		t.Fatal(why)
	}
	out := map[string][]string{}
	for i, name := range c.Tests {
		out[name] = []string{}
		if contains(c.Always, i) {
			out[name] = append(out[name], "always")
		}
	}
	for fn, tests := range c.Funcs {
		for _, i := range tests {
			out[c.Tests[i]] = append(out[c.Tests[i]], fn)
		}
	}
	for _, fns := range out {
		sort.Strings(fns)
	}
	return out
}
