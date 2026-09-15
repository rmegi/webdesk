package main

import (
	"os/exec"
	"syscall"
)

// killWithParent makes the kernel kill cmd if the agent dies, even when the
// agent is killed outright and gets no chance to clean up.
func killWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
