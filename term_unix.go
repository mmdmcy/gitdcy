//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package main

// The dashboard's terminal on Unix: termios through ioctl(2) from the
// syscall package (the request numbers and cfmakeraw's flags per system in
// term_linux.go and term_bsd.go, the wait for a key in term_*poll*.go and
// term_select.go).

import (
	"syscall"
	"time"
	"unsafe"
)

// termTTY is the terminal in raw mode: standard input when it is one, else
// /dev/tty.
type termTTY struct {
	fd    int
	owned bool
	saved *syscall.Termios
}

// termWinsize is struct winsize, the same on every Unix (syscall has no
// type for it).
type termWinsize struct {
	row, col, xpixel, ypixel uint16
}

// termPollFd is struct pollfd, the same on every Unix.
type termPollFd struct {
	fd              int32
	events, revents int16
}

// termPollIn is POLLIN, the same on every Unix.
const termPollIn = 0x1

func termIoctl(fd int, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func termGetattr(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := termIoctl(fd, termGetRequest, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

// termSetattr is tcsetattr(fd, TCSANOW, t).
func termSetattr(fd int, t *syscall.Termios) error {
	return termIoctl(fd, termSetRequest, unsafe.Pointer(t))
}

func termOpen() (*termTTY, error) {
	t := &termTTY{fd: syscall.Stdin}
	if _, err := termGetattr(syscall.Stdin); err != nil {
		fd, err := syscall.Open("/dev/tty", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		t.fd, t.owned = fd, true
	}
	saved, err := termGetattr(t.fd)
	if err != nil {
		t.close()
		return nil, err
	}
	raw := *saved
	termMakeRaw(&raw)
	if err := termSetattr(t.fd, &raw); err != nil {
		t.close()
		return nil, err
	}
	t.saved = saved
	return t, nil
}

// restore puts the terminal back as it was; it does nothing the second time.
func (t *termTTY) restore() error {
	if t.saved == nil {
		return nil
	}
	saved := t.saved
	t.saved = nil
	return termSetattr(t.fd, saved)
}

// close closes /dev/tty when this opened it.
func (t *termTTY) close() {
	if t.owned {
		_ = syscall.Close(t.fd)
		t.owned = false
	}
}

// size is the terminal's columns and rows.
func (t *termTTY) size() (int, int, error) {
	var ws termWinsize
	if err := termIoctl(t.fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, err
	}
	return int(ws.col), int(ws.row), nil
}

// keys is the keys typed within timeout, none when nothing was.
func (t *termTTY) keys(timeout time.Duration) ([]dashKey, error) {
	ready, err := termWait(t.fd, timeout)
	if err == syscall.EINTR {
		return nil, nil
	}
	if err != nil || !ready {
		return nil, err
	}
	var buf [1024]byte
	n, err := syscall.Read(t.fd, buf[:])
	if err == syscall.EINTR || err == syscall.EAGAIN {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return dashParseKeys(buf[:n]), nil
}
