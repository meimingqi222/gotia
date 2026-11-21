package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

func loadMapForInfo() (*module, *coverMap, error) {
	m, err := loadModule(".", nil)
	if err != nil {
		return nil, nil, err
	}
	file, err := defaultMapPath(m)
	if err != nil {
		return nil, nil, err
	}
	c, why := readMap(m, file)
	if c == nil {
		return nil, nil, errors.New(why)
	}
	return m, c, nil
}

func cmdWhy(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gotia why <TestName | pkgdir TestName>")
	}
	_, c, err := loadMapForInfo()
	if err != nil {
		return err
	}
	want := args[0]
	var matched []int
	for i, t := range c.Tests {
		if _, name := c.test(i); t == want || name == want {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no recorded test %s", want)
	}
	for _, i := range matched {
		var funcs []string
		for fn, tests := range c.Funcs {
			if contains(tests, i) {
				funcs = append(funcs, fn)
			}
		}
		sort.Strings(funcs)
		label := "ran"
		if contains(c.Always, i) {
			label = "always runs when its package is affected; ran"
		}
		fmt.Printf("%s %s %d functions:\n", c.Tests[i], label, len(funcs))
		for _, fn := range funcs {
			fmt.Println("  " + fn)
		}
	}
	return nil
}

func cmdWho(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gotia who <Name | (*T).Method | dir/file.go:Name>")
	}
	_, c, err := loadMapForInfo()
	if err != nil {
		return err
	}
	want := args[0]
	var keys []string
	for fn := range c.Funcs {
		if fn == want || strings.HasSuffix(fn, ":"+want) {
			keys = append(keys, fn)
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("no recorded test ran %s (or it does not exist at %.7s)", want, c.Base)
	}
	sort.Strings(keys)
	for _, fn := range keys {
		fmt.Printf("%s ran in %d tests:\n", fn, len(c.Funcs[fn]))
		for _, i := range c.Funcs[fn] {
			fmt.Println("  " + c.Tests[i])
		}
	}
	return nil
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

const guide = `## Running tests

- After changing Go code, run ` + "`go tool gotia test`" + ` instead of ` + "`go test ./...`" + `. It runs only
  the tests the change can affect (committed, staged, unstaged and untracked
  changes) and prints one line per changed file saying what it selected and why.
- It takes go test flags directly: ` + "`go tool gotia test -race -count=1`" + `. ` + "`./pkg/...`" + ` keeps
  only the changes under pkg; a file or directory runs what it affects as if it
  changed. ` + "`-list`" + ` previews the plan; ` + "`-json`" + ` prints it with the exact go test
  commands without running them (` + "`-- -json`" + ` passes -json to go test instead).
- The last lines give the result. ` + "`gotia: PASS: ...`" + ` means every test the change can
  affect passed. ` + "`gotia: FAIL pkg: TestX; rerun: go test ...`" + ` names each failed test
  and the command that reruns only it: use that command while fixing, then run
  ` + "`go tool gotia test`" + ` again.
- ` + "`gotia: nothing to test`" + ` is a correct result: no test can reach what changed
  (comments, formatting, docs). Do not run ` + "`go test ./...`" + ` because of it.
- "no recorded test runs X" means no test executes a function you changed:
  add one if the change matters.
- gotia keeps its coverage map (which functions each test runs) up to date by
  itself: when the map is missing or behind HEAD, it updates it in the
  background after the run. Until then it selects more, never fewer, tests.
  You do not need to run ` + "`go tool gotia record`" + `.
- Run the full suite (` + "`go test ./...`" + `) before delivery; CI runs it too.
- Coverage sees subprocesses that re-run the test binary or binaries a test
  builds with ` + "`go build`" + `. When a test runs module code another way, put
  ` + "`//gotia:always`" + ` in its doc comment, or ` + "`//gotia:package`" + ` in its file to run the
  whole package whenever it is affected.
`

func cmdGuide(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, "usage: gotia guide >> AGENTS.md")
		return nil
	}
	fmt.Print(guide)
	return nil
}
