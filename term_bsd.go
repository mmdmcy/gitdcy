//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// tcgetattr and tcsetattr(TCSANOW) as the BSD libcs and macOS make them.
const (
	termGetRequest = syscall.TIOCGETA
	termSetRequest = syscall.TIOCSETA
)

// termMakeRaw is cfmakeraw as macOS's and FreeBSD's libc have it.
func termMakeRaw(t *syscall.Termios) {
	t.Iflag &^= syscall.IMAXBEL | syscall.IXOFF | syscall.INPCK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IGNPAR
	t.Iflag |= syscall.IGNBRK
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN | syscall.NOFLSH | syscall.TOSTOP | syscall.PENDIN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8 | syscall.CREAD
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
}
