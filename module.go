package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

type goPackage struct {
	Dir, ImportPath, Name                       string
	GoFiles, CgoFiles, IgnoredGoFiles           []string
	TestGoFiles, XTestGoFiles                   []string
	EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
	Imports, TestImports, XTestImports          []string

	rel string // module-relative directory, slash-separated ("." for the root)
}

// module is the Go module gotia works on. Paths it hands around are relative
// to root and slash-separated, so a map recorded in one checkout applies to
// another.
type module struct {
	root, path string
	packages   []*goPackage
	byPath     map[string]*goPackage
	byDir      map[string]*goPackage // by rel
}

func loadModule(dir string) (*module, error) {
	gomod, err := output(dir, "go", "env", "GOMOD")
	if err != nil {
		return nil, err
	}
	if gomod == "" || gomod == os.DevNull {
		return nil, errors.New("not inside a Go module")
	}
	m := &module{root: filepath.Dir(gomod), byPath: map[string]*goPackage{}, byDir: map[string]*goPackage{}}
	data, err := os.ReadFile(gomod)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			m.path = strings.Trim(strings.TrimSpace(rest), `"`)
			break
		}
	}
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = m.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %v\n%s", err, stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		pkg := &goPackage{}
		if err := decoder.Decode(pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(m.root, pkg.Dir)
		if err != nil {
			return nil, err
		}
		pkg.rel = filepath.ToSlash(rel)
		m.packages = append(m.packages, pkg)
		m.byPath[pkg.ImportPath] = pkg
		m.byDir[pkg.rel] = pkg
	}
	return m, nil
}

// short names a package for people and agents: its path inside the module.
func (m *module) short(importPath string) string {
	if pkg := m.byPath[importPath]; pkg != nil && pkg.rel != "." {
		return pkg.rel
	}
	return importPath
}

func (m *module) goVersion() string {
	v, _ := output(m.root, "go", "env", "GOVERSION")
	return v
}

// changedFiles lists module-relative files that differ from base, including
// untracked ones. Files outside the module are not listed.
func (m *module) changedFiles(base string) ([]string, error) {
	var files []string
	for _, args := range [][]string{
		{"diff", "--name-only", "--no-renames", "--relative", "-z", base},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		out, err := output(m.root, "git", args...)
		if err != nil {
			return nil, fmt.Errorf("read changes (or pass paths): %w", err)
		}
		for _, name := range strings.Split(out, "\x00") {
			if name != "" {
				files = append(files, name)
			}
		}
	}
	return files, nil
}

// fileAt reads a module-relative file at a commit; nil when it did not exist.
func (m *module) fileAt(commit, rel string) []byte {
	cmd := exec.Command("git", "show", commit+":./"+rel)
	cmd.Dir = m.root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// relPath turns a path given on the command line into a module-relative one.
func (m *module) relPath(name string) (string, bool) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(m.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// dependents returns roots, every package that imports one of them directly or
// transitively, and every package whose tests import one of those.
func (m *module) dependents(roots map[string]bool) map[string]bool {
	prod := map[string]bool{}
	for r := range roots {
		prod[r] = true
	}
	for changed := true; changed; {
		changed = false
		for _, pkg := range m.packages {
			if prod[pkg.ImportPath] {
				continue
			}
			for _, dep := range pkg.Imports {
				if prod[dep] {
					prod[pkg.ImportPath], changed = true, true
					break
				}
			}
		}
	}
	out := map[string]bool{}
	for p := range prod {
		out[p] = true
	}
	for _, pkg := range m.packages {
		for _, dep := range append(append([]string(nil), pkg.TestImports...), pkg.XTestImports...) {
			if prod[dep] {
				out[pkg.ImportPath] = true
			}
		}
	}
	return out
}

// owner finds the package directory a non-Go file belongs to through an
// embed pattern or a testdata directory.
func (m *module) owner(rel string) (pkg *goPackage, embedded, testOnly bool) {
	for _, p := range m.packages {
		for _, f := range p.EmbedFiles {
			if path.Join(p.rel, f) == rel {
				return p, true, false
			}
		}
		for _, f := range append(append([]string(nil), p.TestEmbedFiles...), p.XTestEmbedFiles...) {
			if path.Join(p.rel, f) == rel {
				return p, true, true
			}
		}
	}
	parts := strings.Split(rel, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "testdata" {
			if p := m.byDir[path.Join(append([]string{"."}, parts[:i]...)...)]; p != nil {
				return p, false, true
			}
		}
	}
	return nil, false, false
}

// readLines reads a small text file into lines; missing files read as none.
func readLines(name string) []string {
	f, err := os.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines
}
