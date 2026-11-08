//go:build linux

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// tcgetattr and tcsetattr(TCSANOW) as glibc makes them.
const (
	termGetRequest = syscall.TCGETS
	termSetRequest = syscall.TCSETS
)

// termMakeRaw is glibc's cfmakeraw.
func termMakeRaw(t *syscall.Termios) {
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
}

// termWait is poll(2) on fd for input within timeout (ppoll, which every
// Linux architecture has).
func termWait(fd int, timeout time.Duration) (bool, error) {
	p := termPollFd{fd: int32(fd), events: termPollIn}
	ts := syscall.NsecToTimespec(int64(timeout))
	n, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	if errno != 0 {
		return false, errno
	}
	return n > 0, nil
}
