//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd in its own process group and has the kernel kill
// it when this process dies (Go's fork checks that the parent is still
// there after asking).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
