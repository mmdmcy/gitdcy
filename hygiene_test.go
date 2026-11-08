package main

import (
	"testing"
	"time"
)

func TestHygieneGitHubPlacesAndSkipPrefixesEndAtAPathBoundary(t *testing.T) {
	urls := []string{"ssh://git@forge.example.com:2222/team/app.git", "git@github.com:Example-Org/App.git"}
	if place, _ := githubRepository(urls); place != "example-org/app" {
		t.Errorf("place %q", place)
	}
	if place, ok := githubRepository([]string{"https://gitlab.com/a/b"}); ok {
		t.Errorf("place %q", place)
	}
	rules := HygieneRules{BranchDays: 3, Skip: []string{"github.com/example-org/"}}
	if prefix, _ := rules.skippedBy(urls); prefix != "github.com/example-org/" {
		t.Errorf("skipped by %q", prefix)
	}
	if prefix, ok := rules.skippedBy([]string{"https://github.com/example-org-other/app"}); ok {
		t.Errorf("skipped by %q", prefix)
	}
	if (&HygieneRules{BranchDays: 0}).validate() == nil {
		t.Error("branch_days = 0 accepted")
	}
}

func TestHygieneGitHubTimesAreReadAsDays(t *testing.T) {
	if got, _ := hygUnixTime("1970-01-02T00:00:00Z"); got != hygDay {
		t.Errorf("got %d", got)
	}
	want := hygDaysFromCivil(2026, 11, 4)*hygDay + 12*3600 + 30*60 + 15
	if got, _ := hygUnixTime("2026-11-04T12:30:15Z"); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
	if got := hygDaysFromCivil(2000, 3, 1); got != 11017 {
		t.Errorf("got %d", got)
	}
	if _, ok := hygUnixTime("not a time"); ok {
		t.Error("read a time from nothing")
	}
}

func TestHygienePullRequestsAreKnownUnknownOrNotAsked(t *testing.T) {
	unknown := hygUnknown("gh is not installed here")
	if of := unknown.of("", false); of.State != "not-asked" {
		t.Errorf("state %s", of.State)
	}
	if of := unknown.of("o/r", true); of.State != "unknown" || of.Detail != "gh is not installed here" {
		t.Errorf("%+v", of)
	}
	known := &hygGitHub{found: map[string][]PullRequest{"o/r": {{Number: 4, Title: "t", URL: "u", Author: "a", Days: 2}}}}
	if of := known.of("O/R", true); len(of.Open) != 1 {
		t.Errorf("%+v", of)
	}
	if of := known.of("o/other", true); len(of.Open) != 0 {
		t.Errorf("%+v", of)
	}
	missing := hygAsk("/nonexistent/gh", []string{"o"}, nil, 5*time.Second)
	if of := missing.of("o/r", true); of.State != "unknown" {
		t.Errorf("%+v", of)
	}
}

func TestHygieneGhAnswersAreReadAsSerdeReadThem(t *testing.T) {
	good := `[{"number":7,"title":"T","url":"u","createdAt":"2026-01-01T00:00:00Z","author":null,"repository":{"nameWithOwner":"o/r"},"isDraft":true,"extra":1}]`
	list, ok := hygParsePullRequests([]byte(good))
	if !ok || len(list) != 1 || list[0].number != 7 || !list[0].isDraft || list[0].repository != "o/r" || list[0].author != "" {
		t.Errorf("%v %+v", ok, list)
	}
	for _, bad := range []string{"", "null", "{}", "[null]", `[{"title":"no number"}]`, `[{"number":-1}]`, `[{"number":1,"title":null}]`, `[{"number":1,"author":"x"}]`} {
		if _, ok := hygParsePullRequests([]byte(bad)); ok {
			t.Errorf("read %s", bad)
		}
	}
}
