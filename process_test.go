//go:build unix

package main

import (
	"os/exec"
	"testing"
	"time"
)

func TestEscapedDescendantCannotHoldCommandOutputOpen(t *testing.T) {
	started := time.Now()
	out, err := runBounded(exec.Command("sh", "-c", "setsid sleep 1 & exit 0"), 1024, time.Second)
	if err != nil || !out.success() {
		t.Fatalf("%v", err)
	}
	if time.Since(started) >= 500*time.Millisecond {
		t.Fatalf("took %v", time.Since(started))
	}
}

func TestEscapedContinuousWriterCannotHoldCommandOutputOpen(t *testing.T) {
	started := time.Now()
	if out, err := runBounded(exec.Command("sh", "-c", "setsid sh -c 'yes output' & exit 0"), 1024, time.Second); err == nil && !out.success() {
		t.Fatal("not a success")
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatalf("took %v", time.Since(started))
	}
}

func TestCompletedCommandDrainsValidOutputAbove64KiB(t *testing.T) {
	out, err := runBounded(exec.Command("sh", "-c", "dd if=/dev/zero bs=131072 count=1 2>/dev/null"), 256*1024, time.Second)
	if err != nil || len(out.Stdout) != 131072 {
		t.Fatalf("%v %d", err, len(out.Stdout))
	}
}
