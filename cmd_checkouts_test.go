package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckoutsFactsAppearInTheJSONOnlyWhenAsked(t *testing.T) {
	var plain map[string]any
	if err := json.Unmarshal([]byte(jsonPretty(checkoutsCheckout{})), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["facts"]; ok {
		t.Fatalf("facts without asking: %v", plain)
	}
	head, stash := strings.Repeat("a", 40), strings.Repeat("b", 40)
	var asked map[string]any
	text := jsonPretty(checkoutsCheckout{Facts: &factsReport{Head: &head, Stashes: []string{stash}}})
	if err := json.Unmarshal([]byte(text), &asked); err != nil {
		t.Fatal(err)
	}
	facts := asked["facts"].(map[string]any)
	if facts["head"] != head || facts["stashes"].([]any)[0] != stash {
		t.Fatalf("%v", facts)
	}
	if facts["fingerprints"] != nil || facts["tree"] != nil {
		t.Fatalf("fingerprints or tree not null: %v", facts)
	}
	for _, key := range []string{"branch", "upstream", "ahead", "behind", "origin", "status_error", "changed",
		"untracked", "unpushed_commits", "unpushed_tips", "ignored_state", "carry_trees"} {
		if _, ok := facts[key]; !ok {
			t.Fatalf("facts.%s is missing", key)
		}
	}
	short := "0123456789abcdef"
	line := checkoutsFactsLine(&factsReport{Head: &short, Changed: 2, Untracked: 1, UnpushedCommits: 3})
	if line != "head 0123456789ab; 2 changed (1 untracked); 3 commit(s) on no remote" {
		t.Fatal(line)
	}
}

func TestCheckoutsOnlyUnreadableCheckoutsAndFailedFetchesAreWarnings(t *testing.T) {
	list := []checkoutsCheckout{
		{Path: "~/a", State: "behind", Detail: "2 commit(s) behind"},
		{Path: "~/b", State: "missing", Detail: "no folder at that path"},
		{Path: "~/c", State: "current", FetchError: "could not resolve host", Detail: "up to date; the fetch failed"},
	}
	want := []string{"~/b: no folder at that path", "~/c: up to date; the fetch failed"}
	if got := checkoutsWarnings(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

// checkoutsSandbox is a folder with its own HOME and git config, where the
// gitdcy binary and git run.
type checkoutsSandbox struct {
	t    *testing.T
	root string
}

func newCheckoutsSandbox(t *testing.T) *checkoutsSandbox {
	t.Helper()
	factsNeedsSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home/.config/gitdcy"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(filepath.Join(root, "home/.gitconfig"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return &checkoutsSandbox{t, root}
}

func (s *checkoutsSandbox) home() string { return filepath.Join(s.root, "home") }

func (s *checkoutsSandbox) command(program, dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "GIT_DIR" && name != "GIT_WORK_TREE" && name != "GITDCY_READ_ONLY_STATUS" {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "HOME="+s.home(), "XDG_CONFIG_HOME="+filepath.Join(s.home(), ".config"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(s.home(), ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	return cmd
}

func (s *checkoutsSandbox) ok(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.command("git", dir, args...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %q: %v %s", args, err, out)
	}
	return trim(string(out))
}

// gitdcy runs the binary in the sandbox: its standard output, standard
// error and whether it succeeded.
func (s *checkoutsSandbox) gitdcy(args ...string) (string, string, bool) {
	s.t.Helper()
	cmd := s.command(gitdcyBinary(s.t), s.root, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err == nil
}

func (s *checkoutsSandbox) json(args ...string) map[string]any {
	s.t.Helper()
	stdout, stderr, ok := s.gitdcy(args...)
	if !ok {
		s.t.Fatalf("%q failed: %s%s", args, stdout, stderr)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(stdout), &value); err != nil {
		s.t.Fatalf("%v: %s%s", err, stdout, stderr)
	}
	return value
}

// upstream is a bare upstream with one commit, and count clones of it under
// HOME.
func (s *checkoutsSandbox) upstream(count int) (string, []string) {
	forge, seed := filepath.Join(s.root, "forge.git"), filepath.Join(s.root, "seed")
	s.ok(s.root, "init", "-q", "--bare", forge)
	s.ok(s.root, "init", "-q", seed)
	s.commit(seed, "README", "hello\n")
	s.ok(seed, "remote", "add", "origin", forge)
	s.ok(seed, "push", "-q", "-u", "origin", "main")
	var clones []string
	for n := 0; n < count; n++ {
		clone := filepath.Join(s.home(), fmt.Sprintf("code/app%d", n))
		s.ok(s.root, "clone", "-q", forge, clone)
		clones = append(clones, clone)
	}
	return seed, clones
}

func (s *checkoutsSandbox) commit(work, file, content string) string {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.ok(work, "add", file)
	s.ok(work, "commit", "-qm", "change "+file)
	return s.ok(work, "rev-parse", "HEAD")
}

// checkoutsByPath is the checkout with that path in an envelope.
func checkoutsByPath(t *testing.T, envelope map[string]any, path string) map[string]any {
	t.Helper()
	for _, c := range envelope["data"].(map[string]any)["checkouts"].([]any) {
		if c := c.(map[string]any); c["path"] == path {
			return c
		}
	}
	t.Fatalf("%s not in %v", path, envelope)
	return nil
}

func TestCheckoutsACleanCheckoutBehindIsFastForwardedAndNothingElseIsTouched(t *testing.T) {
	s := newCheckoutsSandbox(t)
	seed, clones := s.upstream(4)
	clean, dirty, ahead, diverged := clones[0], clones[1], clones[2], clones[3]
	newest := s.commit(seed, "NEWS", "one\n")
	s.ok(seed, "push", "-q")

	if err := os.WriteFile(filepath.Join(dirty, "README"), []byte("my own edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.ok(ahead, "pull", "-q", "--ff-only")
	aheadHead := s.commit(ahead, "MINE", "mine\n")
	divergedHead := s.commit(diverged, "OTHER", "other\n")
	dirtyHead := s.ok(dirty, "rev-parse", "HEAD")

	paths := []string{"~/code/app0", "~/code/app1", "~/code/app2", "~/code/app3"}
	before := s.json(append([]string{"checkouts", "--format", "json"}, paths...)...)
	if before["operation"] != "checkouts" || before["outcome"] != "success" {
		t.Fatalf("%v", before)
	}
	c := checkoutsByPath(t, before, "~/code/app0")
	if c["state"] != "behind" || c["behind"] != 1.0 || c["branch"] != "main" || c["tracking_branch"] != "origin/main" {
		t.Fatalf("%v", c)
	}
	if !strings.HasSuffix(c["upstream"].(string), "change NEWS") {
		t.Fatalf("%v", c)
	}
	if c["detail"] != "1 commit(s) behind; checkouts update brings it up" {
		t.Fatalf("%v", c)
	}
	d := checkoutsByPath(t, before, "~/code/app1")
	if d["state"] != "behind" || d["changed"] != 1.0 {
		t.Fatalf("%v", d)
	}
	file := d["files"].([]any)[0].(map[string]any)
	if file["path"] != "README" || file["status"] != " M" {
		t.Fatalf("%v", file)
	}
	if got := checkoutsByPath(t, before, "~/code/app2")["state"]; got != "ahead" {
		t.Fatalf("app2 %v", got)
	}
	if got := checkoutsByPath(t, before, "~/code/app3")["state"]; got != "diverged" {
		t.Fatalf("app3 %v", got)
	}

	after := s.json(append([]string{"checkouts", "update", "--format", "json"}, paths...)...)
	if after["operation"] != "checkouts.update" {
		t.Fatalf("%v", after)
	}
	c = checkoutsByPath(t, after, "~/code/app0")
	if c["state"] != "current" || c["updated"] != true || c["detail"] != "fast-forwarded 1 commit(s); up to date" {
		t.Fatalf("%v", c)
	}
	if got := s.ok(clean, "rev-parse", "HEAD"); got != newest {
		t.Fatalf("clean at %s, want %s", got, newest)
	}

	// Local changes, local commits and a diverged branch stay exactly as they were.
	if got := checkoutsByPath(t, after, "~/code/app1")["updated"]; got != false {
		t.Fatalf("app1 updated %v", got)
	}
	if got := s.ok(dirty, "rev-parse", "HEAD"); got != dirtyHead {
		t.Fatal("the dirty checkout moved")
	}
	if data, _ := os.ReadFile(filepath.Join(dirty, "README")); string(data) != "my own edit\n" {
		t.Fatalf("README is %q", data)
	}
	if got := s.ok(ahead, "rev-parse", "HEAD"); got != aheadHead {
		t.Fatal("the checkout ahead moved")
	}
	if got := s.ok(diverged, "rev-parse", "HEAD"); got != divergedHead {
		t.Fatal("the diverged checkout moved")
	}
	if got := checkoutsByPath(t, after, "~/code/app3")["state"]; got != "diverged" {
		t.Fatalf("app3 %v", got)
	}
	if got := s.ok(diverged, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("no merge commit: %s commits", got)
	}
}

func TestCheckoutsNoFetchShowsTheLastFetchAndAnUntrackedFileBlocksTheUpdate(t *testing.T) {
	s := newCheckoutsSandbox(t)
	seed, clones := s.upstream(1)
	s.commit(seed, "NEWS", "one\n")
	s.ok(seed, "push", "-q")

	quiet := s.json("checkouts", "--no-fetch", "--format", "json", "~/code/app0")
	if got := checkoutsByPath(t, quiet, "~/code/app0")["state"]; got != "current" {
		t.Fatalf("state %v", got)
	}

	if err := os.WriteFile(filepath.Join(clones[0], "scratch.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := s.json("checkouts", "update", "--format", "json", "~/code/app0")
	c := checkoutsByPath(t, out, "~/code/app0")
	if c["state"] != "behind" || c["updated"] != false {
		t.Fatalf("%v", c)
	}
	if got := c["files"].([]any)[0].(map[string]any)["status"]; got != "??" {
		t.Fatalf("status %v", got)
	}
	if !strings.Contains(c["detail"].(string), "commit or stash") {
		t.Fatalf("%v", c)
	}
}

func TestCheckoutsMissingPathsBranchesWithoutUpstreamAndFailedFetchesAreSaidPlainly(t *testing.T) {
	s := newCheckoutsSandbox(t)
	_, clones := s.upstream(2)
	s.ok(clones[0], "checkout", "-qb", "local-only")
	if err := os.MkdirAll(filepath.Join(s.home(), "not-a-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.ok(clones[1], "remote", "set-url", "origin", "/nonexistent/gitdcy-forge.git")

	out := s.json("checkouts", "--format", "json", "~/code/app0", "~/code/app1", "~/not-a-repo", "~/gone")
	if out["outcome"] != "partial" {
		t.Fatalf("%v", out)
	}
	local := checkoutsByPath(t, out, "~/code/app0")
	if local["state"] != "no-upstream" || local["branch"] != "local-only" || local["detail"] != "the branch follows no remote branch" {
		t.Fatalf("%v", local)
	}
	unfetched := checkoutsByPath(t, out, "~/code/app1")
	if unfetched["state"] != "current" || unfetched["fetch_error"] == "" ||
		!strings.Contains(unfetched["detail"].(string), "so this is as of the last fetch") {
		t.Fatalf("%v", unfetched)
	}
	if got := checkoutsByPath(t, out, "~/not-a-repo")["detail"]; got != "not a git checkout at that path" {
		t.Fatalf("%v", got)
	}
	if got := checkoutsByPath(t, out, "~/gone")["state"]; got != "missing" {
		t.Fatalf("%v", got)
	}
	if warnings := out["warnings"].([]any); len(warnings) != 3 {
		t.Fatalf("%v", warnings)
	}

	stdout, stderr, ok := s.gitdcy("checkouts", "~/gone")
	said := stdout + stderr
	if !ok {
		t.Fatal(said)
	}
	if !strings.HasPrefix(said, "1 checkout(s): 0 up to date, 0 behind, 1 need attention") {
		t.Fatal(said)
	}
	if !strings.Contains(said, "~/gone") || !strings.Contains(said, "no folder at that path") {
		t.Fatal(said)
	}
}
