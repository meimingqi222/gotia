//go:build !unix

package main

import "os/exec"

// detach does nothing: on Windows a child process outlives its parent.
func detach(cmd *exec.Cmd) {}
