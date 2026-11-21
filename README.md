# gotia

gotia runs only the Go tests a change can affect. It is meant for the edit
loop, especially for coding agents that otherwise run `go test ./...` after
every small change.

```sh
go get -tool github.com/meimingqi222/gotia@latest   # Go 1.24+
go tool gotia test                                  # instead of go test ./...
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
...go test output...
gotia: FAIL harness: TestRetryAfter; rerun: go test -run '^(TestRetryAfter)$' ./harness
```

The last lines are the result for agents and scripts: `gotia: PASS: ...`, or
one `gotia: FAIL` line per failed package with its failed tests and the
command that reruns only them. `gotia: nothing to test` means no test can
reach the change. A repository without commits runs every package.

- **Without a map** it runs the changed packages and every package that
  imports them, directly or through tests.
- **With a map** it runs only the tests that executed a changed
  function. Changes are compared token by token, so comments and formatting
  select nothing. A changed `var` or `const` selects the tests that ran a
  function using it, across packages. A changed test runs by name.
- **It falls back to whole packages** when coverage cannot attribute a change:
  changed types, new or removed methods (interfaces), `init`, `//go:`
  directives and build constraints, side-effect imports, test helpers, test
  data and embedded files. `go.mod`, `go.sum`, `go.work`, the modules of the
  `go.work` in use, local `replace` directories and files outside every
  package run everything; so does a `go.work` or `replace` directory outside
  the repository, whose changes git cannot show. `*.md` and `.github/**` are ignored by default (`-ignore`).

A stale map stays correct: gotia measures changes against the map's commit,
so it only selects more as HEAD moves on.

`gotia test` keeps the map current by itself. When the map is missing or
behind HEAD, it starts `gotia record` in the background after its run, on
half the CPUs, with the map's build flags; the next `gotia test` uses the new
map. A lock file lets one record at a time update the map, and its output
goes to the `.log` file next to the map. A map an older gotia wrote, or
whose commit is gone, is replaced the same way; a map recorded under another
Go version or `GOFLAGS` is left alone, and `gotia record` replaces it. Set
`GOTIA_AUTO_RECORD=0` to turn this off; in CI (`$CI` set) it is off unless
`GOTIA_AUTO_RECORD=1`. Maps named with `-map` or `GOTIA_MAP` are never
updated automatically.

## Recording

`gotia record` (also what the background update runs) checks HEAD out into a temporary worktree, builds each test
package with `-cover -coverpkg=<module>/...`, and runs every top-level test
alone, in parallel. A test that fails while recording always runs when its
package is affected.

When a usable map exists, `gotia record` updates it instead: it selects the
tests the commits since the map's commit can affect, exactly as `gotia test`
would, records only those, and keeps the rest. A kept test ran no function
that changed, so it still runs the same functions. Changes that make
`gotia test` run every package, such as `go.mod`, record every test.
`-full` records every test regardless; run it now and then, since a branch a
test takes only under some timing stays missing from kept entries.

Subprocesses are covered too: a re-executed test binary and a binary the test
builds with `go build` write their counters to the test's `GOCOVERDIR`. When a
test runs module code in a way coverage cannot see, mark it in its test file:

- `//gotia:always` in a test's doc comment runs it whenever its package is affected.
- `//gotia:package` anywhere in a test file runs that whole package whenever it is affected.

The map lives in `.git/gotia/` and is keyed to the platform, the Go version,
`GOFLAGS`, `CGO_ENABLED`, `GOEXPERIMENT` and the build flags given to `gotia
record`. `gotia test` uses it only with the same `-tags`, and `gotia record`
with other build flags records every test again.
`GOTIA_MAP` or `-map` picks another file, for example one CI recorded on main.

## Flags

```
gotia test [-list] [-json] [-all] [-base commit] [-ignore globs] [paths...] [go test flags]
gotia record [-full] [-j workers] [-timeout d] [build flags such as -tags]
gotia why <TestName>        functions a test ran
gotia who <Func>            tests that ran a function (Name, (*T).Method, dir/file.go:Name)
```

Paths limit `gotia test`: `./pkg/...` keeps only the changes under pkg, and a
file or directory runs what it affects as if it changed. `-json` prints the
plan, including the exact `go test` commands, for scripts and agents. Flags
gotia does not define, and everything after `--`, go to every `go test`
command (`-- -json` for go test's own JSON); `-run` among them turns per-test
selection off.

## Limits

Selection is as good as the recorded runs: a branch a test reaches only under
some timing may be missing from the map. Run the full suite before delivery and
in CI.

## License

MIT. See [LICENSE](LICENSE).
