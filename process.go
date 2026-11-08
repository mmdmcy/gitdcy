package main

// Running programs: a finished program's output, its exit status as Rust
// printed it, and the bounded runner read-only callers use (a time limit,
// an output limit, and on Unix its own process group, so a program that
// leaves children behind cannot hold the output open).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// procOutput is a finished program's exit status and output.
type procOutput struct {
	state  *os.ProcessState
	Stdout []byte
	Stderr []byte
}

// success reports whether the program exited with 0.
func (o *procOutput) success() bool { return o.state != nil && o.state.Success() }

// code is the exit code; false when a signal ended the program.
func (o *procOutput) code() (int, bool) {
	if o.state == nil {
		return 0, false
	}
	c := o.state.ExitCode()
	return c, c >= 0
}

// statusText is the exit status as Rust's ExitStatus printed it.
func (o *procOutput) statusText() string { return exitStatusText(o.state) }

// runOutput runs cmd to the end and collects its output (Rust's
// Command::output): standard input is the null device.
func runOutput(cmd *exec.Cmd) (*procOutput, error) {
	var stdout, stderr bytes.Buffer
	if cmd.Stdout == nil {
		cmd.Stdout = &stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, ioError(err)
	}
	return &procOutput{state: cmd.ProcessState, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

// runStatus runs cmd with this process's standard streams (Rust's
// Command::status).
func runStatus(cmd *exec.Cmd) (*os.ProcessState, error) {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, ioError(err)
	}
	return cmd.ProcessState, nil
}

// setEnv adds or replaces one environment variable for cmd, on top of what
// this process has (Rust's Command::env).
func setEnv(cmd *exec.Cmd, key, value string) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, key+"="+value)
}

// runBounded runs cmd with a time limit and an output limit per stream: a
// program past its time is stopped, and more output than limit is an error.
// Only Unix bounds it; elsewhere it runs as runOutput.
func runBounded(cmd *exec.Cmd, limit int, timeout time.Duration) (*procOutput, error) {
	return runBoundedOS(cmd, limit, timeout)
}

// lossy is bytes as text the way Rust's String::from_utf8_lossy made it:
// each maximal invalid sequence becomes one U+FFFD.
func lossy(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			out.WriteString(string(b[:size]))
			b = b[size:]
			continue
		}
		out.WriteRune(utf8.RuneError)
		b = b[invalidPrefix(b):]
	}
	return out.String()
}

// invalidPrefix is how many bytes at the start of b form one invalid
// sequence: a lead byte and the continuation bytes that could still have
// completed it (the Unicode "maximal subpart"), at least one byte.
func invalidPrefix(b []byte) int {
	lead := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for i := 0; i < need && n < len(b); i++ {
		c := b[n]
		if i == 0 && (c < lo || c > hi) {
			break
		}
		if i > 0 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}

// readText reads a UTF-8 text file (Rust's fs::read_to_string): text that
// is not UTF-8 is an error.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ioError(err)
	}
	if !utf8.Valid(data) {
		return "", errors.New("stream did not contain valid UTF-8")
	}
	return string(data), nil
}

// signalNames are the names Rust printed after a signal number (Linux and
// macOS share these for the signals that end a program here).
var signalNames = map[int]string{
	1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 5: "SIGTRAP", 6: "SIGABRT",
	8: "SIGFPE", 9: "SIGKILL", 11: "SIGSEGV", 13: "SIGPIPE", 14: "SIGALRM", 15: "SIGTERM",
}

// signalText is "N (NAME)" as Rust printed a signal.
func signalText(n int) string {
	if name, ok := signalNames[n]; ok {
		return fmt.Sprintf("%d (%s)", n, name)
	}
	return fmt.Sprintf("%d", n)
}
