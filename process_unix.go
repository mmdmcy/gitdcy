//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// exitStatusText is Rust's ExitStatus display on Unix.
func exitStatusText(state *os.ProcessState) string {
	if state == nil {
		return "exit status: unknown"
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return fmt.Sprintf("exit status: %d", state.ExitCode())
	}
	switch {
	case ws.Exited():
		return fmt.Sprintf("exit status: %d", ws.ExitStatus())
	case ws.Signaled():
		text := "signal: " + signalText(int(ws.Signal()))
		if ws.CoreDump() {
			text += " (core dumped)"
		}
		return text
	case ws.Stopped():
		return "stopped (not terminated) by signal: " + signalText(int(ws.StopSignal()))
	}
	return fmt.Sprintf("exit status: %d", state.ExitCode())
}

// runBoundedOS runs cmd in its own process group (and, on Linux, killed
// when this process dies), reads each stream up to limit, and stops the
// whole group when cmd ends or its time is up, so children it left behind
// cannot hold the output open.
func runBoundedOS(cmd *exec.Cmd, limit int, timeout time.Duration) (*procOutput, error) {
	setProcessGroup(cmd)
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, wrap(err, "start bounded command")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, wrap(err, "start bounded command")
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, wrap(err, "start bounded command")
	}
	outW.Close()
	errW.Close()

	var stop atomic.Bool
	type result struct {
		data      []byte
		truncated bool
		err       error
	}
	read := func(f *os.File, done chan<- result) {
		data, truncated, err := readBoundedUntilStopped(f, limit, &stop)
		done <- result{data, truncated, err}
	}
	outDone, errDone := make(chan result, 1), make(chan result, 1)
	go read(outR, outDone)
	go read(errR, errDone)

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waited:
		if err := killGroup(cmd); err != nil {
			return nil, err
		}
	case <-timer.C:
		if err := killGroup(cmd); err != nil {
			return nil, err
		}
		<-waited
	}

	// Drain what is there, then stop reading: an escaped child may keep
	// the pipe open, but its later output is not this command's.
	stop.Store(true)
	deadline := time.Now().Add(10 * time.Millisecond)
	outR.SetReadDeadline(deadline)
	errR.SetReadDeadline(deadline)
	out, errOut := <-outDone, <-errDone
	outR.Close()
	errR.Close()
	if out.err != nil {
		return nil, wrap(out.err, "read bounded stdout")
	}
	if errOut.err != nil {
		return nil, wrap(errOut.err, "read bounded stderr")
	}
	if out.truncated || errOut.truncated {
		return nil, errors.New("bounded command exceeded its output safety limit")
	}
	return &procOutput{state: cmd.ProcessState, Stdout: out.data, Stderr: errOut.data}, nil
}

// readBoundedUntilStopped keeps up to limit bytes of f; once stopped it
// reads only what is already there.
func readBoundedUntilStopped(f *os.File, limit int, stop *atomic.Bool) ([]byte, bool, error) {
	var data []byte
	buffer := make([]byte, 8192)
	truncated := false
	for {
		n, err := f.Read(buffer)
		if n > 0 {
			remaining := limit - len(data)
			if remaining < 0 {
				remaining = 0
			}
			kept := n
			if kept > remaining {
				kept = remaining
			}
			data = append(data, buffer[:kept]...)
			if kept < n {
				truncated = true
			}
			if stop.Load() && truncated {
				return data, truncated, nil
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return data, truncated, nil
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if stop.Load() {
				return data, truncated, nil
			}
			continue
		}
		return data, truncated, ioError(err)
	}
}

// killGroup ends cmd's process group; a group already gone is fine.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return wrap(err, "terminate bounded command process group")
	}
	return nil
}
