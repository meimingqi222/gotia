# gotia

gotia runs only the Go tests a change can affect. It is meant for the edit
loop, especially for coding agents that otherwise run `go test ./...` after
every small change.

```sh
go get -tool github.com/meimingqi222/gotia@latest   # Go 1.24+
go tool gotia test                                  # instead of go test ./...
go tool gotia record                                # optional: per-test selection
go tool gotia guide >> AGENTS.md                    # tell agents how to use it
```

## What it selects

`gotia test` compares the working tree (committed, staged, unstaged and
untracked changes) with a base commit and prints one line per changed file
saying what it selected and why:

```
gotia: per-test selection from the coverage map at 8d414aa
gotia: harness/openai.go: parseRetryAfter → 16 tests that ran them
gotia: running 16 tests in 2 packages and 3 packages in full; 13 packages skipped
```

- **Without a map** it runs the changed packages and every package that
  imports them, directly or through tests.
- **With a map** (`gotia record`) it runs only the tests that executed a changed
  function. Changes are compared token by token, so comments and formatting
  select nothing. A changed `var` or `const` selects the tests that ran a
  function using it, across packages. A changed test runs by name.
- **It falls back to whole packages** when coverage cannot attribute a change:
  changed types, new or removed methods (interfaces), `init`, `//go:`
  directives and build constraints, side-effect imports, test helpers, test
  data and embedded files. `go.mod`, `go.sum` and files outside every package
  run everything. `*.md` and `.github/**` are ignored by default (`-ignore`).

A stale map stays correct: gotia measures changes against the map's commit,
so it only selects more as HEAD moves on. Re-record after pulling.

## Recording

`gotia record` checks HEAD out into a temporary worktree, builds each test
package with `-cover -coverpkg=<module>/...`, and runs every top-level test
alone, in parallel. A test that fails while recording always runs when its
package is affected.

Subprocesses are covered too: a re-executed test binary and a binary the test
builds with `go build` write their counters to the test's `GOCOVERDIR`. When a
test runs module code in a way coverage cannot see, mark it in its test file:

- `//gotia:always` in a test's doc comment runs it whenever its package is affected.
- `//gotia:package` anywhere in a test file runs that whole package whenever it is affected.

The map lives in `.git/gotia/` and is keyed to the platform and Go version;
`GOTIA_MAP` or `-map` picks another file, for example one CI recorded on main.

## Flags

```
gotia test [-list] [-json] [-all] [-base commit] [-ignore globs] [paths...] [-- go test flags]
gotia record [-j workers] [-timeout d] [-- build flags such as -tags]
gotia why <TestName>        functions a test ran
gotia who <Func>            tests that ran a function (Name, (*T).Method, dir/file.go:Name)
```

`-json` prints the plan, including the exact `go test` commands, for scripts and
agents. Flags after `--` go to every `go test` command; `-run` among them turns
per-test selection off.

## Limits

Selection is as good as the recorded runs: a branch a test reaches only under
some timing may be missing from the map. Run the full suite before delivery and
in CI.
