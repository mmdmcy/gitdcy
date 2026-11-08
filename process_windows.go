//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// exitStatusText is Rust's ExitStatus display on Windows.
func exitStatusText(state *os.ProcessState) string {
	if state == nil {
		return "exit code: unknown"
	}
	return fmt.Sprintf("exit code: %d", uint32(state.ExitCode()))
}

// runBoundedOS runs cmd to the end, as the Rust build did on Windows: the
// limits apply on Unix only.
func runBoundedOS(cmd *exec.Cmd, limit int, timeout time.Duration) (*procOutput, error) {
	_ = limit
	_ = timeout
	out, err := runOutput(cmd)
	if err != nil {
		return nil, wrap(err, "run bounded command")
	}
	return out, nil
}
