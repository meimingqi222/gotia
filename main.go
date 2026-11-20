// Command gotia runs only the Go tests that a change can affect.
//
// Without setup it selects the changed packages and the packages that depend
// on them. After "gotia record", which runs every test of HEAD alone with
// coverage and stores which functions each one executed, it runs only the
// tests that executed a changed function. Changes that coverage cannot
// attribute to a function fall back to package selection, so it never skips
// a test a change can affect for a reason it can see.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const usage = `gotia runs only the Go tests a change can affect.

Usage:
  gotia test [flags] [paths...] [-- go test flags]
        run the tests affected by the given files, or by every change
        against the coverage map's commit (HEAD without a map)
  gotia record [flags] [-- go test build flags]
        record which functions each test of HEAD runs (about a minute)
  gotia why <test>
        list the functions a recorded test ran
  gotia who <function>
        list the recorded tests that ran a function (Name, (*T).Method or file.go:Name)
  gotia guide
        print instructions for coding agents, to add to AGENTS.md

Run "gotia <command> -h" for the flags of a command.
`

func main() {
	commands := map[string]func([]string) error{
		"test": cmdTest, "record": cmdRecord, "why": cmdWhy, "who": cmdWho, "guide": cmdGuide,
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		if os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help" {
			return
		}
		os.Exit(2)
	}
	err := run(os.Args[2:])
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotia:", err)
		os.Exit(1)
	}
}

// exitCode ends gotia with a status but no message: go test already reported.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// splitArgs separates gotia's own arguments from those after "--".
func splitArgs(args []string) (own, rest []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// output runs a command in dir and returns its trimmed standard output.
func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func note(format string, args ...any) {
	fmt.Printf("gotia: "+format+"\n", args...)
}
