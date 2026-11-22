//go:build unix

package main

// The rules file's identities on a push: who may author and commit what a
// push sends. Real repositories in a sandbox with its own HOME; nothing is
// pushed anywhere but the sandbox's own bare repository.

import (
	"fmt"
	"strings"
	"testing"
)

const identitiesStranger = "lmicesync smoke <lmicesync@test>"

// pushOf is the hook's line for a push of work's HEAD to a new branch.
func (s *rulesSandbox) pushOf(work string) string {
	return fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", s.ok(work, "rev-parse", "HEAD"), rulesZero)
}

func TestIdentitiesAPushByAnIdentityTheRulesDoNotListIsRefusedWithTheFix(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("identities = [\"Test <test@example.com>\", \"noreply@github.com\"]\n")
	if out := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "origin", "/srv/git/app.git"); out.code != 0 {
		t.Fatalf("the sandbox's own identity passes: %s", out.text())
	}

	// An address alone allows any name.
	rulesWrite(t, work+"/web.txt", "edited on the web\n")
	s.ok(work, "add", "web.txt")
	if out := s.gitEnv(work, []string{"GIT_COMMITTER_NAME", "GitHub", "GIT_COMMITTER_EMAIL", "noreply@github.com"}, "commit", "-qm", "web"); out.code != 0 {
		t.Fatal(out.text())
	}
	if out := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "origin", "/srv/git/app.git"); out.code != 0 {
		t.Fatalf("an address listed alone passes with any name: %s", out.text())
	}

	rulesWrite(t, work+"/smoke.txt", "smoke\n")
	s.ok(work, "add", "smoke.txt")
	if out := s.gitEnv(work, []string{"GIT_AUTHOR_NAME", "lmicesync smoke", "GIT_AUTHOR_EMAIL", "lmicesync@test"}, "commit", "-qm", "smoke"); out.code != 0 {
		t.Fatal(out.text())
	}
	out := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "--format", "json", "origin", "/srv/git/app.git")
	if out.code != 1 {
		t.Fatalf("refused: %s", out.text())
	}
	v := rulesJSON(t, out)
	if rulesAt(v, "data", "refused") != true || rulesAt(v, "data", "findings", 0, "kind") != "identity" ||
		rulesAt(v, "data", "findings", 0, "place") != "author" || rulesAt(v, "data", "findings", 0, "shown") != identitiesStranger {
		t.Fatal(out.text())
	}
	if n := len(rulesAt(v, "data", "findings").([]any)); n != 1 {
		t.Errorf("only the author is not allowed, the committer is: %s", out.text())
	}
	reason, _ := rulesAt(v, "data", "reasons", 0).(string)
	if !strings.Contains(reason, "not allow") || !strings.Contains(reason, "Test <test@example.com>, noreply@github.com") {
		t.Errorf("the reason names what is allowed: %s", reason)
	}
	// From the oldest refused commit's parent on.
	fix := "git rebase -r " + s.ok(work, "rev-parse", "--short", "HEAD") + "^ --exec 'git commit --amend --no-edit --reset-author'"
	if !strings.Contains(reason, fix) {
		t.Errorf("the reason gives the fix (%s): %s", fix, reason)
	}
	if !strings.Contains(out.text(), "question for the repository's owner") {
		t.Errorf("a refusal is a question for the owner: %s", out.text())
	}

	// The text says which commit, which field and which identity.
	said := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "origin", "/srv/git/app.git").text()
	short := s.ok(work, "rev-parse", "--short", "HEAD")
	if !strings.Contains(said, "commit "+short+" author: an identity the rules do not allow ("+identitiesStranger+")") {
		t.Fatal(said)
	}

	// The fix it gives works.
	if out := s.git(work, "rebase", "-q", "-r", short+"^", "--exec", "git commit --amend --no-edit --reset-author"); out.code != 0 {
		t.Fatal(out.text())
	}
	if out := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "origin", "/srv/git/app.git"); out.code != 0 {
		t.Fatalf("after the fix the push passes: %s", out.text())
	}
}

func TestIdentitiesACommitterIsReadTooAndAPrivateAddressIsMasked(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("identities = [\"Test <test@example.com>\"]\n")
	rulesWrite(t, work+"/b.txt", "b\n")
	s.ok(work, "add", "b.txt")
	address := rulesPrivateAddress()
	if out := s.gitEnv(work, []string{"GIT_COMMITTER_NAME", "Someone", "GIT_COMMITTER_EMAIL", address}, "commit", "-qm", "b"); out.code != 0 {
		t.Fatal(out.text())
	}
	out := s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "--format", "json", "origin", "/srv/git/app.git")
	if out.code != 1 || !identitiesFound(t, out, "committer", "Someone <s…@gmail.com>") {
		t.Fatal(out.text())
	}
	if strings.Contains(out.text(), address) {
		t.Errorf("a private address is not printed whole: %s", out.text())
	}
	// A name the entry gives must match too; an address on the list is no
	// personal-data finding either. Both commits are refused: the first one
	// (Test) too, so the fix starts at the root.
	s.rules("identities = [\"Other <test@example.com>\", \"" + address + "\"]\n")
	out = s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "--format", "json", "origin", "/srv/git/app.git")
	findings, _ := rulesAt(rulesJSON(t, out), "data", "findings").([]any)
	if out.code != 1 || len(findings) != 3 || !strings.Contains(out.stdout, "git rebase -r --root ") || !identitiesFound(t, out, "author", "Test <test@example.com>") {
		t.Fatal(out.text())
	}
}

// identitiesFound says whether the check's JSON names an identity finding
// at place showing shown.
func identitiesFound(t *testing.T, out rulesOut, place, shown string) bool {
	findings, _ := rulesAt(rulesJSON(t, out), "data", "findings").([]any)
	for i := range findings {
		if rulesAt(findings, i, "kind") == "identity" && rulesAt(findings, i, "place") == place && rulesAt(findings, i, "shown") == shown {
			return true
		}
	}
	return false
}

func TestIdentitiesARulesOwnListReplacesTheFilesAndNoListChecksNothing(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	rulesWrite(t, work+"/c.txt", "c\n")
	s.ok(work, "add", "c.txt")
	if out := s.gitEnv(work, []string{"GIT_AUTHOR_NAME", "Sam Roe", "GIT_AUTHOR_EMAIL", "sam@example.com"}, "commit", "-qm", "c"); out.code != 0 {
		t.Fatal(out.text())
	}
	check := func() rulesOut {
		return s.gitdcyStdin(work, s.pushOf(work), "rules", "check", "--stdin", "--format", "json", "origin", "/srv/git/app.git")
	}
	s.rules("")
	if out := check(); out.code != 0 {
		t.Fatalf("no identities: no identity check: %s", out.text())
	}
	s.rules("identities = [\"Test <test@example.com>\"]\n")
	if out := check(); out.code != 1 {
		t.Fatalf("Sam is not on the file's list: %s", out.text())
	}
	s.rules("identities = [\"Test <test@example.com>\"]\n" + rulesRule(first, `"anywhere"`, "identities = [\"Test <test@example.com>\", \"Sam Roe <sam@example.com>\"]\n"))
	if out := check(); out.code != 0 {
		t.Fatalf("the rule's own list holds for its repository: %s", out.text())
	}
	out := s.gitdcy(work, "rules", "--format", "json")
	if ids, _ := rulesAt(rulesJSON(t, out), "data", "allowed_identities").([]any); len(ids) != 2 {
		t.Errorf("gitdcy rules shows the identities that hold: %s", out.text())
	}
	if said := s.gitdcy(work, "rules").text(); !strings.Contains(said, "identities allowed to author and commit a push: Test <test@example.com>, Sam Roe <sam@example.com>") {
		t.Errorf("%s", said)
	}
}

func TestIdentitiesAnEntryWithoutAnAddressIsRefused(t *testing.T) {
	for _, text := range []string{
		"identities = [\"Test <>\"]\n",
		"identities = [\"\"]\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x\"]\nidentities = [\"  \"]\n",
	} {
		file, err := rulesParse(text)
		if err == nil {
			err = file.validate()
		}
		if err == nil || !strings.Contains(err.Error(), "has no address") {
			t.Errorf("%q: %v", text, err)
		}
	}
	if !identityAllowed("Test <TEST@example.com>", []string{"<test@example.com>"}) || identityAllowed("Test <test@example.org>", []string{"test@example.com"}) {
		t.Error("an address matches in any case, and only itself")
	}
}
