//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own, so it outlives gotia and the
// terminal or agent tool that ran it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
