//go:build unix

package main

import (
	"os"
	"os/exec"
	"sync/atomic"
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

// A reader that runs after the drain deadline (a busy machine) still keeps
// what the command wrote before it ended.
func TestALateReaderKeepsWhatAnEndedCommandWrote(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close() // held open, as by an escaped child
	if _, err := w.Write([]byte("# branch.ab +0 -2\n")); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	stop.Store(true)
	r.SetReadDeadline(time.Now().Add(-time.Second))
	data, truncated, err := readBoundedUntilStopped(r, 1024, &stop)
	if err != nil || truncated || string(data) != "# branch.ab +0 -2\n" {
		t.Fatalf("%q %v %v", data, truncated, err)
	}

	if _, err := w.Write(make([]byte, 2048)); err != nil {
		t.Fatal(err)
	}
	r.SetReadDeadline(time.Now().Add(-time.Second))
	if data, truncated, err := readBoundedUntilStopped(r, 1024, &stop); err != nil || !truncated || len(data) != 1024 {
		t.Fatalf("past the limit: %d %v %v", len(data), truncated, err)
	}
}
