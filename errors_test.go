package main

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorsPrintAsAnyhowPrintedThem(t *testing.T) {
	one := wrap(errors.New("no such file"), "read rules.toml")
	if one.Error() != "read rules.toml" || chainText(one) != "read rules.toml: no such file" {
		t.Fatalf("%q %q", one.Error(), chainText(one))
	}
	if !strings.HasPrefix(reportText(one), "read rules.toml\n\nCaused by:\n    no such file") {
		t.Fatal(reportText(one))
	}
	two := wrapf(one, "check %s", "repo")
	if chainText(two) != "check repo: read rules.toml: no such file" {
		t.Fatal(chainText(two))
	}
	if !strings.HasPrefix(reportText(two), "check repo\n\nCaused by:\n    0: read rules.toml\n    1: no such file") {
		t.Fatal(reportText(two))
	}
	lines := wrap(errf("first\nsecond"), "outer")
	if !strings.HasPrefix(reportText(lines), "outer\n\nCaused by:\n    first\n    second") {
		t.Fatal(reportText(lines))
	}
	if chainText(errf("pass --all or a repo id/name")) != "pass --all or a repo id/name" {
		t.Fatal("plain message")
	}
	// An operating-system error reads as Rust's std::io::Error did.
	_, err := readText("/nonexistent/gitdcy-test")
	if err == nil || !strings.HasSuffix(err.Error(), "(os error 2)") {
		t.Fatalf("%v", err)
	}
}

func TestBailReturnsTheMessage(t *testing.T) {
	refuse := func(n int) (int, error) {
		if n > 1 {
			return 0, errf("checks blocked %d repos", n)
		}
		return n, nil
	}
	if n, err := refuse(1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := refuse(3); err == nil || err.Error() != "checks blocked 3 repos" {
		t.Fatal(err)
	}
}
