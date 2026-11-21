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

- After a change, run ` + "`go tool gotia test`" + ` instead of ` + "`go test ./...`" + `. It runs only the
  tests the change can affect (committed, staged, unstaged and untracked changes)
  and prints one line per changed file saying what it selected and why.
- Pass go test flags after ` + "`--`" + `: ` + "`go tool gotia test -- -race -count=1`" + `. Pass files to
  check only them: ` + "`go tool gotia test path/to/file.go`" + `. ` + "`-list`" + ` previews the plan and
  ` + "`-json`" + ` prints it for scripts.
- To run one test while iterating, use go test directly:
  ` + "`go test -run '^TestName$' ./pkg`" + `.
- Without a coverage map gotia selects whole packages (changed packages and their
  dependents). ` + "`go tool gotia record`" + ` (about a minute) records which functions each
  test runs, so later runs select single tests. Re-record after pulling: it
  only re-runs the tests the new commits affect. A stale map stays correct
  but selects more.
- "no recorded test runs X" means no test executes a function you changed:
  add one if the change matters.
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
