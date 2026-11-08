package main

import (
	"reflect"
	"testing"
)

func TestScanABareDayIsReadFromItsStart(t *testing.T) {
	day, ago := "2026-10-24", "3 days ago"
	if got := scanSinceText(&day); got != "2026-10-24 00:00" {
		t.Errorf("got %q", got)
	}
	if got := scanSinceText(&ago); got != ago {
		t.Errorf("got %q", got)
	}
	if got := scanSinceText(nil); got != scanSince {
		t.Errorf("got %q", got)
	}
}

func TestScanWorkIsSpreadAndKeptInOrder(t *testing.T) {
	items := make([]int, 20)
	want := make([]int, 20)
	for i := range items {
		items[i], want[i] = i, i*2
	}
	if got := scanParallel(items, func(n int) int { return n * 2 }); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if got := scanParallel([]int{}, func(n int) int { return n }); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}
