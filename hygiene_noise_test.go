//go:build unix

package main

// Reminders that would be noise: a backup copy, an empty clone, another
// person's repository's pull requests, and remote state read against refs a
// device cannot fetch.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rulesScanOne is the scan's JSON for these paths.
func rulesScanOne(s *rulesSandbox, paths ...string) any {
	s.t.Helper()
	return rulesJSON(s.t, s.gitdcy(s.root, append([]string{"rules", "scan", "--format", "json"}, paths...)...))
}

func rulesKinds(dishes any) string {
	var kinds []string
	for i := 0; i < rulesLen(rulesAt(dishes, "reminders")); i++ {
		kind, _ := rulesAt(dishes, "reminders", i, "kind").(string)
		kinds = append(kinds, kind)
	}
	return strings.Join(kinds, ",")
}

func TestHygieneACopyUnderABackupsFolderGetsNoReminders(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("backups/2026-11-01/app")
	rulesOldCommit(s, work, "OLD", 10)
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\n")
	checkout := rulesAt(rulesScanOne(s, work), "data", "checkouts", 0)
	off, _ := rulesAt(checkout, "hygiene_off").(string)
	if rulesAt(checkout, "hygiene") != nil || !strings.Contains(off, "skip_folders") {
		t.Fatalf("a backup copy was reminded of: %v", checkout)
	}
	// A checkout named backups is no copy; skip_folders = [] turns it off.
	own, _ := s.repo("backups")
	rulesOldCommit(s, own, "OLD", 10)
	if checkout := rulesAt(rulesScanOne(s, own), "data", "checkouts", 0); rulesAt(checkout, "hygiene") == nil {
		t.Errorf("a repository named backups lost its reminders: %v", checkout)
	}
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\nskip_folders = []\n")
	if checkout := rulesAt(rulesScanOne(s, work), "data", "checkouts", 0); rulesAt(checkout, "hygiene") == nil {
		t.Errorf("skip_folders = [] still skipped: %v", checkout)
	}
	if (&HygieneRules{BranchDays: 3, SkipFolders: []string{"a/b"}}).validate() == nil {
		t.Error("a path accepted as a folder name")
	}
}

func TestHygieneAnEmptyCloneGetsNoReminders(t *testing.T) {
	s := newRulesSandbox(t)
	_, first := s.repo("app")
	forge := filepath.Join(s.root, "empty-forge.git")
	s.ok(s.root, "init", "-q", "--bare", forge)
	empty := filepath.Join(s.root, "empty")
	s.git(s.root, "clone", "-q", forge, empty)
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\n")
	checkout := rulesAt(rulesScanOne(s, empty), "data", "checkouts", 0)
	off, _ := rulesAt(checkout, "hygiene_off").(string)
	if rulesAt(checkout, "hygiene") != nil || !strings.Contains(off, "empty checkout") {
		t.Fatalf("%v", checkout)
	}
	// A file in it is work: reminded of.
	rulesWrite(t, filepath.Join(empty, "NOTES"), "a start\n")
	checkout = rulesAt(rulesScanOne(s, empty), "data", "checkouts", 0)
	if kinds := rulesKinds(rulesAt(checkout, "hygiene")); kinds != "dirty" {
		t.Errorf("%q: %v", kinds, checkout)
	}
}

// A clone of another person's GitHub project is not asked about, and the
// owners asked are only those of remotes pushed to from here.
func TestHygienePullRequestsAreAskedOnlyForRepositoriesPushedToFromHere(t *testing.T) {
	s := newRulesSandbox(t)
	_, first := s.repo("app")
	theirs, _ := s.repo("theirs")
	s.ok(theirs, "remote", "add", "upstream", "https://github.com/someone-else/project.git")
	asked := filepath.Join(s.root, "gh-asked")
	rulesFakeGh(s, `echo "$*" >> `+asked+"\n"+rulesOnePR)
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\n")
	v := rulesScanOne(s, theirs)
	dishes := rulesAt(v, "data", "checkouts", 0, "hygiene")
	detail, _ := rulesAt(dishes, "pull_requests", "detail").(string)
	if rulesAt(dishes, "pull_requests", "state") != "not-asked" || !strings.Contains(detail, "someone else's") || rulesExists(asked) {
		t.Fatalf("someone else's repository was asked about: %v", dishes)
	}
	// Pushed to its github remote from here: its own, asked by its owner.
	fork, _ := s.repo("fork")
	rulesPushedToGitHub(s, fork, "fork")
	s.ok(fork, "remote", "add", "upstream", "https://github.com/someone-else/project.git")
	v = rulesScanOne(s, theirs, fork)
	if rulesAt(v, "data", "checkouts", 1, "hygiene", "pull_requests", "state") != "known" {
		t.Fatalf("%v", rulesAt(v, "data", "checkouts", 1))
	}
	if said := strings.TrimSpace(rulesRead(t, asked)); !strings.Contains(said, "--owner example-org") || strings.Contains(said, "someone-else") {
		t.Errorf("asked: %s", said)
	}
}

// A fetch refused for want of a login is kept by host; a scan then says it
// once and does not judge the checkout's remote state.
func TestHygieneAFetchRefusedForCredentialsIsSaidOnceAndRemoteStateNotJudged(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.ok(work, "push", "-q", "origin", "main")
	s.ok(work, "branch", "-q", "--set-upstream-to=origin/main")
	// The remote is any host on any transport: a helper that answers as a
	// forge without this device's login does.
	bin := filepath.Join(s.root, "helperbin")
	rulesMkdir(t, bin)
	rulesWrite(t, filepath.Join(bin, "git-remote-fake"), "#!/bin/sh\necho \"fatal: could not read Username for 'https://forge.example.org': terminal prompts disabled\" >&2\nexit 128\n")
	if err := os.Chmod(filepath.Join(bin, "git-remote-fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.path = bin + ":" + s.path
	s.ok(work, "remote", "set-url", "origin", "fake://forge.example.org/team/app.git")
	rulesOldCommit(s, work, "OLD", 10)
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\n")

	before := rulesScanOne(s, work)
	if kinds := rulesKinds(rulesAt(before, "data", "checkouts", 0, "hygiene")); !strings.Contains(kinds, "unpushed") {
		t.Fatalf("without a refused fetch the commits are reminded of: %q", kinds)
	}
	out := s.gitdcy(work, "checkouts", "--format", "json", ".")
	if fetchError, _ := rulesAt(rulesJSON(t, out), "data", "checkouts", 0, "fetch_error").(string); !strings.Contains(fetchError, "could not read Username") {
		t.Fatalf("%s", out.text())
	}
	v := rulesScanOne(s, work)
	dishes := rulesAt(v, "data", "checkouts", 0, "hygiene")
	if rulesAt(dishes, "stale_remote") != "forge.example.org" || rulesKinds(dishes) != "" {
		t.Fatalf("%v", dishes)
	}
	if rulesLen(rulesAt(v, "data", "fetch_refused")) != 1 || rulesAt(v, "data", "fetch_refused", 0, "host") != "forge.example.org" ||
		rulesAt(v, "data", "fetch_refused", 0, "checkouts") != float64(1) {
		t.Fatalf("%v", rulesAt(v, "data", "fetch_refused"))
	}
	if said := s.gitdcy(s.root, "rules", "scan", work).text(); strings.Count(said, "cannot fetch") != 1 {
		t.Error(said)
	}
}

func TestHygieneAGoodFetchClearsTheRefusalAndOtherFailuresKeepNone(t *testing.T) {
	failures := &fetchFailures{Hosts: map[string]fetchFailure{}}
	refused := checkoutsCheckout{Remote: "forge.example.org/team/app", FetchError: "Authentication failed", fetchTried: true, fetchForCredentials: true}
	offline := checkoutsCheckout{Remote: "other.example.org/team/app", FetchError: "Could not resolve host", fetchTried: true}
	if !failures.note([]checkoutsCheckout{refused, offline}, 100) || len(failures.Hosts) != 1 || failures.Hosts["forge.example.org"].At != 100 {
		t.Fatalf("%+v", failures.Hosts)
	}
	good := checkoutsCheckout{Remote: "forge.example.org/team/lib", fetchTried: true}
	if failures.note([]checkoutsCheckout{refused, good}, 200); failures.Hosts["forge.example.org"].At != 200 {
		t.Errorf("a refusal in the same run is kept: %+v", failures.Hosts)
	}
	if !failures.note([]checkoutsCheckout{good}, 300) || len(failures.Hosts) != 0 {
		t.Errorf("a good fetch clears it: %+v", failures.Hosts)
	}
	for said, want := range map[string]bool{
		"remote: HTTP Basic: Access denied\nfatal: Authentication failed for 'https://x/'": true,
		"git@forge.example.org: Permission denied (publickey).":                            true,
		"fatal: unable to access 'https://x/': The requested URL returned error: 401":      true,
		"fatal: Cannot prompt because user interactivity has been disabled.":               true,
		"fatal: unable to access 'https://x/': Could not resolve host: x":                  false,
	} {
		if fetchForWantOfCredentials(said) != want {
			t.Errorf("%q: want %v", said, want)
		}
	}
	if host, _, ok := failures.refusing([]string{"/srv/local/forge.git"}); ok {
		t.Errorf("a local path refused: %s", host)
	}
}
