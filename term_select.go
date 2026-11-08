//go:build darwin || openbsd

package main

import (
	"syscall"
	"time"
)

// termWait waits for input on fd within timeout with select(2), through
// libc: macOS's poll(2) does not take devices such as /dev/tty, and
// OpenBSD takes system calls only from libc.
func termWait(fd int, timeout time.Duration) (bool, error) {
	var set syscall.FdSet
	if fd < 0 || fd >= 32*len(set.Bits) {
		return false, syscall.EINVAL
	}
	set.Bits[fd/32] |= 1 << (uint(fd) % 32)
	tv := syscall.NsecToTimeval(int64(timeout))
	if err := syscall.Select(fd+1, &set, nil, nil, &tv); err != nil {
		return false, err
	}
	return set.Bits[fd/32]&(1<<(uint(fd)%32)) != 0, nil
}
