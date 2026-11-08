package main

// GitDCY's errors: a message and the error that caused it, printed as the
// Rust build printed them. err.Error() is the message alone ("{}"),
// chainText the whole chain joined with ": " ("{:#}", what the JSON
// envelope carries) and reportText what a failed command prints after
// "Error: " ("{:?}": the message, then "Caused by:" and each cause).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// wrapped is a message in front of the error that caused it.
type wrapped struct {
	msg   string
	cause error
}

func (e *wrapped) Error() string { return e.msg }
func (e *wrapped) Unwrap() error { return e.cause }

// errf is an error that is only a message (Rust's err!/bail!).
func errf(format string, args ...any) error {
	if len(args) == 0 {
		return errors.New(format)
	}
	return fmt.Errorf(format, args...)
}

// wrap puts context in front of err (Rust's .context()); nil stays nil.
func wrap(err error, context string) error {
	if err == nil {
		return nil
	}
	return &wrapped{msg: context, cause: ioError(err)}
}

// wrapf is wrap with a formatted context (Rust's .with_context()).
func wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return wrap(err, fmt.Sprintf(format, args...))
}

// causes is err and every error that caused it, outermost first. Only the
// errors this file makes have causes: a Go error's own text stays whole, as
// a Rust error's Display did.
func causes(err error) []error {
	var list []error
	for err != nil {
		list = append(list, err)
		w, ok := err.(*wrapped)
		if !ok {
			break
		}
		err = w.cause
	}
	return list
}

// chainText is the message and every cause joined with ": ".
func chainText(err error) string {
	parts := []string{}
	for _, e := range causes(err) {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, ": ")
}

// reportText is what a failed command prints after "Error: ".
func reportText(err error) string {
	list := causes(err)
	var b strings.Builder
	b.WriteString(list[0].Error())
	rest := list[1:]
	if len(rest) == 0 {
		return b.String()
	}
	b.WriteString("\n\nCaused by:")
	numbered := len(rest) > 1
	for n, cause := range rest {
		first, indent := "    ", "    "
		if numbered {
			first, indent = fmt.Sprintf("%5d: ", n), "       "
		}
		b.WriteString("\n" + first)
		for i, line := range strings.Split(cause.Error(), "\n") {
			if i > 0 {
				b.WriteString("\n" + indent)
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

// osText is how Rust's std::io::Error showed an operating-system error:
// the system's own description and "(os error N)".
type osText struct {
	text string
	err  error
}

func (e *osText) Error() string { return e.text }
func (e *osText) Unwrap() error { return e.err }

// ioError turns an operating-system error from os, os/exec or io/fs into the
// text Rust's std::io::Error printed ("No such file or directory (os error
// 2)"); any other error is returned as it is.
func ioError(err error) error {
	if err == nil {
		return nil
	}
	if _, done := err.(*osText); done {
		return err
	}
	if _, ours := err.(*wrapped); ours {
		return err
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		return &osText{text: errnoText(errno), err: err}
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		// Rust spawned the program and the system answered "not found".
		return &osText{text: errnoText(syscall.ENOENT), err: err}
	}
	return err
}

// errnoText is the system's description of errno and its number, as Rust
// printed it: strerror's sentence on Unix (Go's table is strerror's with the
// first letter lowered), FormatMessage's on Windows.
func errnoText(errno syscall.Errno) string {
	text := errno.Error()
	if runtime.GOOS != "windows" && text != "" {
		text = strings.ToUpper(text[:1]) + text[1:]
	}
	return fmt.Sprintf("%s (os error %d)", text, int(errno))
}

// notFound reports whether err says a file is not there.
func notFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// fail prints a failed command's error as Rust's main did and exits 1.
func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", reportText(err))
	os.Exit(1)
}
