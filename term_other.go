//go:build !(linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows)

package main

// The dashboard's terminal elsewhere (Solaris, illumos, AIX): this build has
// no raw-mode code for these systems, so the dashboard says so.

import (
	"runtime"
	"time"
)

type termTTY struct{}

func termOpen() (*termTTY, error) {
	return nil, errf("raw terminal mode is not built for %s", runtime.GOOS)
}

func (t *termTTY) restore() error                        { return nil }
func (t *termTTY) close()                                {}
func (t *termTTY) size() (int, int, error)               { return 0, 0, nil }
func (t *termTTY) keys(time.Duration) ([]dashKey, error) { return nil, nil }
