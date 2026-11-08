package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// factsSh runs a shell script in dir with no global or system git config
// and a test identity, and returns its trimmed standard output.
func factsSh(t *testing.T, dir, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v %s", script, err, stderr.String())
	}
	return trim(string(out))
}

// factsNeedsSh skips a test that runs shell scripts where there is no sh
// (the Rust tests were Unix-only).
func factsNeedsSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs sh scripts; Unix only, as in the Rust build")
	}
}

func factsFresh(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFactsWorkingTreeIDMatchesGitAndWritesNothing(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "repo")
	factsSh(t, dir, "git init -q && echo base > a.txt && echo '*.log' > .gitignore && git add -A && "+
		"git -c commit.gpgsign=false commit -qm base && "+
		"echo edited > a.txt && echo new > b.txt && echo noise > c.log")
	state := func() string {
		return factsSh(t, dir, "git count-objects -v | head -1 && git status --porcelain && cksum .git/index")
	}
	before := state()
	tree := factsWorkingTreeID(dir)
	if tree == nil {
		t.Fatal("no tree id")
	}
	if after := state(); after != before {
		t.Fatalf("the repository changed:\n%s\n%s", before, after)
	}
	expected := factsSh(t, dir, "cp -R . ../copy-$$ && cd ../copy-$$ && git add -A && git write-tree && cd .. && rm -rf copy-$$")
	if *tree != expected {
		t.Fatalf("tree %s, git says %s", *tree, expected)
	}
}

func TestFactsCoverChangesStashesUnpushedWorkAndIgnoredStateWithoutHooks(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "work")
	factsSh(t, dir, "git init -q --bare -b main upstream.git &&git init -q -b main repo && cd repo && "+
		"printf '.env\\nbuild/\\n*.log\\n' > .gitignore && echo one > a.txt && git add -A && "+
		"git commit -qm one && git remote add origin ../upstream.git && "+
		"git push -q -u origin main && echo two > a.txt && git commit -qam two && "+
		"echo stashed > a.txt && git stash -q && echo edited > a.txt && "+
		"mkdir -p notes build && echo n > notes/n.md && echo TOKEN=x > .env && "+
		"echo out > build/app.env && ln -s a.txt link && "+
		"mkdir -p .git/hooks && printf '#!/bin/sh\\ntouch ../hook-ran\\n' > .git/hooks/post-index-change && "+
		"chmod +x .git/hooks/post-index-change && git config core.fsmonitor 'touch ../fsmonitor-ran; false'")
	repo := filepath.Join(dir, "repo")
	commit := func(rev string) string { return factsSh(t, repo, "git -c core.fsmonitor=false rev-parse "+rev) }
	f := factsRead(repo)
	same := func(what string, got *string, want string) {
		t.Helper()
		if got == nil || *got != want {
			t.Fatalf("%s: got %v, want %q", what, got, want)
		}
	}
	same("head", f.Head, commit("HEAD"))
	same("branch", f.Branch, "main")
	same("upstream", f.Upstream, "origin/main")
	if f.Ahead != 1 || f.Behind != 0 {
		t.Fatalf("ahead %d behind %d", f.Ahead, f.Behind)
	}
	same("origin", f.Origin, "../upstream.git")
	if f.StatusError != nil {
		t.Fatalf("status error %q", *f.StatusError)
	}
	// a.txt, link and notes/n.md (each untracked file on its own).
	if f.Changed != 3 || f.Untracked != 2 {
		t.Fatalf("changed %d untracked %d", f.Changed, f.Untracked)
	}
	if want := []string{commit("stash@{0}")}; !reflect.DeepEqual(f.Stashes, want) {
		t.Fatalf("stashes %q, want %q", f.Stashes, want)
	}
	if f.UnpushedCommits != 1 {
		t.Fatalf("unpushed %d", f.UnpushedCommits)
	}
	if want := []string{commit("HEAD")}; !reflect.DeepEqual(f.UnpushedTips, want) {
		t.Fatalf("tips %q, want %q", f.UnpushedTips, want)
	}
	if want := []string{".env"}; !reflect.DeepEqual(f.IgnoredState, want) {
		t.Fatalf("ignored %q", f.IgnoredState)
	}
	if f.Fingerprints == nil {
		t.Fatal("no fingerprints")
	}
	want := []string{
		"sha256:" + factsSHA256Hex([]byte("edited\n")) + " a.txt",
		"link:" + factsSHA256Hex([]byte("a.txt")) + " link",
		"sha256:" + factsSHA256Hex([]byte("n\n")) + " notes/n.md",
	}
	if !reflect.DeepEqual(*f.Fingerprints, want) {
		t.Fatalf("fingerprints %q, want %q", *f.Fingerprints, want)
	}
	if f.Tree == nil || !factsIsObjectID(*f.Tree) {
		t.Fatalf("tree %v", f.Tree)
	}
	if len(f.CarryTrees) != 0 {
		t.Fatalf("carry trees %q", f.CarryTrees)
	}
	for _, name := range []string{"hook-ran", "fsmonitor-ran"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s: a hook or fsmonitor ran", name)
		}
	}

	factsSh(t, repo, "git -c core.fsmonitor=false update-ref refs/carry/peer/x HEAD")
	if got, want := factsRead(repo).CarryTrees, []string{commit("HEAD^{tree}")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("carry trees %q, want %q", got, want)
	}
}

func TestFactsACleanCheckoutAndOneBeforeItsFirstCommitHavePlainFacts(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "clean")
	factsSh(t, dir, "git init -q -b main empty && git init -q -b main clean && cd clean && echo a > a && git add a && git commit -qm a")
	empty := factsRead(filepath.Join(dir, "empty"))
	if empty.Head != nil {
		t.Fatalf("head %q", *empty.Head)
	}
	if empty.Branch == nil || *empty.Branch != "main" {
		t.Fatalf("branch %v", empty.Branch)
	}
	if empty.Changed != 0 || empty.UnpushedCommits != 0 || empty.Fingerprints != nil {
		t.Fatalf("%+v", empty)
	}
	clean := factsRead(filepath.Join(dir, "clean"))
	if clean.Changed != 0 || clean.Fingerprints != nil || clean.Tree != nil {
		t.Fatalf("%+v", clean)
	}
	// No remote at all: every commit exists only here.
	if clean.UnpushedCommits != 1 || clean.Origin != nil {
		t.Fatalf("%+v", clean)
	}
}

func TestFactsAFolderGitCannotReadSaysWhy(t *testing.T) {
	factsNeedsSh(t)
	dir := factsFresh(t, "broken")
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := factsRead(dir)
	if f.StatusError == nil || !strings.HasPrefix(*f.StatusError, "git status failed: ") {
		t.Fatalf("status error %v", f.StatusError)
	}
	if f.Changed != 0 || f.Head != nil {
		t.Fatalf("%+v", f)
	}
}

func TestFactsURLsLoseTheirCredentialsAndNothingElse(t *testing.T) {
	for url, want := range map[string]string{
		"https://example.org/a/b.git":              "https://example.org/a/b.git",
		"https://user:tok@example.org/a/b.git":     "https://example.org/a/b.git",
		"ssh://git@example.org:2222/a/b":           "ssh://example.org:2222/a/b",
		"http://git.example.internal:3000/owner/x": "http://git.example.internal:3000/owner/x",
		"file:///srv/git/x.git":                    "file:///srv/git/x.git",
		"git@example.org:owner/x.git":              "git@example.org:owner/x.git",
	} {
		if got := factsWithoutCredentials(url); got == nil || *got != want {
			t.Fatalf("%s: got %v, want %s", url, got, want)
		}
	}
	for _, url := range []string{"user:pw@host:x", "https://a b"} {
		if got := factsWithoutCredentials(url); got != nil {
			t.Fatalf("%s: got %q", url, *got)
		}
	}
}

func TestFactsNotableIgnoredStateKeepsSecretsAndDataButNotBuildOutput(t *testing.T) {
	for _, path := range []string{".env", "apps/web/.env.local", "services/api/pb_data", "data/app.sqlite3", "WIFI_MESH_FIX.private.md"} {
		if !factsIsNotableIgnoredPath(path) {
			t.Fatalf("%s should be reported", path)
		}
	}
	for _, path := range []string{".env.example", "node_modules/pkg/.env", "target/debug/app.db", "macos/Flutter/ephemeral",
		".DS_Store", "dist", ".rsync-partial/.env", "app/.copy-partial/data.db"} {
		if factsIsNotableIgnoredPath(path) {
			t.Fatalf("%s should be skipped", path)
		}
	}
}
