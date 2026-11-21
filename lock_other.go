//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package main

import "os"

// tryLock always succeeds: this system has no lock gotia uses, so two
// records can run at once. Each replaces the map whole, so it stays valid.
func tryLock(f *os.File) (bool, error) { return true, nil }

func unlockFile(f *os.File) error { return nil }
