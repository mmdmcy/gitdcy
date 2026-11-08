//go:build dragonfly || freebsd || netbsd

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// termWait is poll(2) on fd for input within timeout.
func termWait(fd int, timeout time.Duration) (bool, error) {
	p := termPollFd{fd: int32(fd), events: termPollIn}
	n, _, errno := syscall.Syscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(timeout.Milliseconds()))
	if errno != 0 {
		return false, errno
	}
	return n > 0, nil
}
