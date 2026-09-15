//go:build !linux

package main

import "os/exec"

// killWithParent is Linux-only; the agent only runs on Linux, but this keeps
// the package building and vetting on other development machines.
func killWithParent(*exec.Cmd) {}
