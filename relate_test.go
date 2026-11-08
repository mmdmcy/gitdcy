package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRelateEveryRelationMatchesGitAndCountsEachSide(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "all")
	factsSh(t, dir, "git init -q -b main . && git commit -q --allow-empty -m one && git tag one && "+
		"git commit -q --allow-empty -m two && git tag two && "+
		"git switch -qc side one && git commit -q --allow-empty -m side && git tag side && "+
		"git switch -q --orphan alone && git commit -q --allow-empty -m alone && git tag alone && "+
		"git switch -q main")
	shown := func(a, b string) string {
		found := relateOne(dir, a, b, nil)
		return fmt.Sprintf("%s %d %d", found.Relation, found.Ahead, found.Behind)
	}
	for _, c := range [][3]string{
		{"two", "main", "equal 0 0"},
		{"two", "one", "ahead 1 0"},
		{"one", "two", "behind 0 1"},
		{"side", "two", "diverged 1 1"},
		{"alone", "two", "unrelated 1 2"},
	} {
		if got := shown(c[0], c[1]); got != c[2] {
			t.Fatalf("%s %s: %s, want %s", c[0], c[1], got, c[2])
		}
	}
	// What git itself says, for the two checks this replaces.
	isAncestor := func(a, b string) bool {
		cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
		cmd.Dir = dir
		return cmd.Run() == nil
	}
	for _, pair := range [][2]string{{"one", "two"}, {"two", "one"}, {"side", "two"}, {"two", "two"}} {
		relation := relateOne(dir, pair[0], pair[1], nil).Relation
		if isAncestor(pair[0], pair[1]) != (relation == "equal" || relation == "behind") {
			t.Fatalf("%s %s %s", pair[0], pair[1], relation)
		}
	}
	counted := factsSh(t, dir, "git rev-list --left-right --count side...two")
	ten := uint64(10)
	found := relateOne(dir, "side", "two", &ten)
	if got := fmt.Sprintf("%d\t%d", found.Ahead, found.Behind); got != counted {
		t.Fatalf("%q, git says %q", got, counted)
	}
	subjects := func(list *[]relateCommit) []string {
		var out []string
		for _, c := range *list {
			out = append(out, c.Subject)
		}
		return out
	}
	if got := subjects(found.OnlyA); !reflect.DeepEqual(got, []string{"side"}) {
		t.Fatalf("only a %q", got)
	}
	if got := subjects(found.OnlyB); !reflect.DeepEqual(got, []string{"two"}) {
		t.Fatalf("only b %q", got)
	}
	if want := factsSh(t, dir, "git rev-parse side"); found.ACommit == nil || *found.ACommit != want {
		t.Fatalf("a commit %v, want %s", found.ACommit, want)
	}
}

func TestRelateWhatIsNotThereIsMissingAndSaysSo(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "missing")
	factsSh(t, dir, "git init -q -b main . && git commit -q --allow-empty -m one")
	absent := strings.Repeat("0", 40)
	found := relateOne(dir, absent, "HEAD", nil)
	if found.Relation != "missing" || found.BCommit == nil || !strings.HasPrefix(found.Detail, absent) {
		t.Fatalf("%+v", found)
	}
	if got := relateOne(dir, "--all", "HEAD", nil).Relation; got != "missing" {
		t.Fatalf("--all: %s", got)
	}
	nowhere := relateOne("/nonexistent/gitdcy-relate", "HEAD", "HEAD", nil)
	if nowhere.Relation != "missing" || nowhere.Detail != "no folder at that path" {
		t.Fatalf("%+v", nowhere)
	}
	// Before the first commit HEAD names nothing.
	factsSh(t, dir, "git init -q -b main empty")
	if got := relateOne(filepath.Join(dir, "empty"), "HEAD", "HEAD", nil).Relation; got != "missing" {
		t.Fatalf("empty: %s", got)
	}
}

func TestRelateRequestsAreTabSeparatedLinesAnsweredInOrder(t *testing.T) {
	got, err := relateParseRequests("/a b\tHEAD\tmain\n\n~/c\tx\ty\r\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []relateRequest{{"/a b", "HEAD", "main"}, {"~/c", "x", "y"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if _, err := relateParseRequests("/a HEAD main\n"); err == nil {
		t.Fatal("a line without tabs was read")
	}
	var requests []relateRequest
	for n := 0; n < 20; n++ {
		requests = append(requests, relateRequest{fmt.Sprintf("/nonexistent/%d", n), "a", "b"})
	}
	for n, answer := range relateAll(requests, nil) {
		if answer.Repo != fmt.Sprintf("/nonexistent/%d", n) {
			t.Fatalf("answer %d is for %s", n, answer.Repo)
		}
	}
}
