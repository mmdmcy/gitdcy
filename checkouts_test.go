package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckoutsStatesComeFromBehindAheadAndChanges(t *testing.T) {
	if s, d := checkoutsVerdict(0, 0, 0); s != "current" || d != "up to date" {
		t.Fatalf("0 0 0: %s %s", s, d)
	}
	if s, _ := checkoutsVerdict(0, 0, 3); s != "changed" {
		t.Fatalf("0 0 3: %s", s)
	}
	if s, d := checkoutsVerdict(2, 0, 0); s != "behind" || d != "2 commit(s) behind; checkouts update brings it up" {
		t.Fatalf("2 0 0: %s %s", s, d)
	}
	s, d := checkoutsVerdict(2, 0, 1)
	if s != "behind" || !strings.Contains(d, "commit or stash them first") {
		t.Fatalf("2 0 1: %s %s", s, d)
	}
	if s, _ := checkoutsVerdict(0, 1, 5); s != "ahead" {
		t.Fatalf("0 1 5: %s", s)
	}
	if s, _ := checkoutsVerdict(2, 1, 0); s != "diverged" {
		t.Fatalf("2 1 0: %s", s)
	}
}

func TestCheckoutsPorcelainKeepsEachFileOnceWithItsStatus(t *testing.T) {
	output := []byte(" M src/main.rs\x00R  new.rs\x00old.rs\x00?? notes/\x00UU both.txt\x00")
	var shown [][2]string
	for _, f := range checkoutsParseStatus(output) {
		shown = append(shown, [2]string{f.Status, f.Path})
	}
	want := [][2]string{{" M", "src/main.rs"}, {"R ", "new.rs"}, {"??", "notes/"}, {"UU", "both.txt"}}
	if !reflect.DeepEqual(shown, want) {
		t.Fatalf("got %q, want %q", shown, want)
	}
	if got := checkoutsParseStatus(nil); len(got) != 0 {
		t.Fatalf("empty output gave %q", got)
	}
}

func TestCheckoutsSummaryCountsWhatNeedsAPerson(t *testing.T) {
	var list []checkoutsCheckout
	for _, state := range []string{"current", "behind", "diverged", "missing"} {
		list = append(list, checkoutsCheckout{State: state})
	}
	if got := checkoutsSummary(list); got != "4 checkout(s): 1 up to date, 1 behind, 2 need attention" {
		t.Fatal(got)
	}
}

func TestCheckoutsAPathThatIsNoCheckoutIsMissing(t *testing.T) {
	found := checkoutsOne("/nonexistent/gitdcy-checkouts-test", checkoutsOptions{})
	if found.State != "missing" || found.Detail != "no folder at that path" || found.Path != "/nonexistent/gitdcy-checkouts-test" {
		t.Fatalf("%+v", found)
	}
	if checkoutsOne("/nonexistent/gitdcy-checkouts-test", checkoutsOptions{facts: true}).Facts != nil {
		t.Fatal("a missing checkout has facts")
	}
}

func TestCheckoutsFactsComeOnlyWhenAskedEvenWhenGitFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := checkoutsOne(dir, checkoutsOptions{})
	if plain.State != "error" || plain.Facts != nil {
		t.Fatalf("%+v", plain)
	}
	asked := checkoutsOne(dir, checkoutsOptions{facts: true})
	if asked.State != "error" {
		t.Fatalf("%+v", asked)
	}
	if asked.Facts == nil || asked.Facts.StatusError == nil {
		t.Fatalf("no status error in %+v", asked.Facts)
	}
}
