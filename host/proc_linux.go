package main

import (
	"os/exec"
	"syscall"
)

// killWithParent makes the kernel kill cmd if the host dies, even when the
// host is killed outright and gets no chance to clean up.
func killWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
