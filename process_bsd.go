//go:build unix && !linux

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd in its own process group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
