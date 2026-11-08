//go:build unix

package main

// `gitdcy rules` end to end: real repositories, real `git push` through the
// installed hook, in a sandbox with its own HOME and git config.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const rulesZero = "0000000000000000000000000000000000000000"

type rulesSandbox struct {
	t    *testing.T
	root string
	path string
	bin  string
}

func newRulesSandbox(t *testing.T) *rulesSandbox {
	t.Helper()
	t.Parallel()
	bin := gitdcyBinary(t)
	s := &rulesSandbox{t: t, root: t.TempDir(), bin: bin}
	s.path = filepath.Dir(bin) + ":" + os.Getenv("PATH")
	rulesMkdir(t, filepath.Join(s.root, "home/.config/gitdcy"))
	rulesWrite(t, filepath.Join(s.root, "home/.gitconfig"), "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n")
	return s
}

func rulesMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func rulesWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rulesRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rulesExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// command is program run in dir with the sandbox's HOME, git config and
// PATH; env adds or replaces variables (pairs).
func (s *rulesSandbox) command(program, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	home := filepath.Join(s.root, "home")
	set := map[string]string{
		"HOME":                home,
		"XDG_CONFIG_HOME":     filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":   filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM": "1",
		"PATH":                s.path,
	}
	for i := 0; i+1 < len(env); i += 2 {
		set[env[i]] = env[i+1]
	}
	drop := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GITDCY_READ_ONLY_STATUS": true}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := set[name]; !ok && !drop[name] {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for name, value := range set {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	return cmd
}

type rulesOut struct {
	stdout, stderr string
	code           int
}

func (o rulesOut) text() string { return o.stdout + o.stderr }

func (s *rulesSandbox) run(cmd *exec.Cmd, input string) (rulesOut, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Stdin = strings.NewReader(input)
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return rulesOut{}, err
	}
	return rulesOut{stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()}, nil
}

func (s *rulesSandbox) mustRun(cmd *exec.Cmd, input string) rulesOut {
	s.t.Helper()
	out, err := s.run(cmd, input)
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *rulesSandbox) git(dir string, args ...string) rulesOut {
	s.t.Helper()
	return s.mustRun(s.command("git", dir, nil, args...), "")
}

func (s *rulesSandbox) gitEnv(dir string, env []string, args ...string) rulesOut {
	s.t.Helper()
	return s.mustRun(s.command("git", dir, env, args...), "")
}

// ok is git's trimmed output; the test fails when git does.
func (s *rulesSandbox) ok(dir string, args ...string) string {
	s.t.Helper()
	out := s.git(dir, args...)
	if out.code != 0 {
		s.t.Fatalf("git %v: %s", args, out.text())
	}
	return trim(out.stdout)
}

func (s *rulesSandbox) gitdcy(dir string, args ...string) rulesOut {
	s.t.Helper()
	return s.mustRun(s.command(s.bin, dir, nil, args...), "")
}

func (s *rulesSandbox) gitdcyStdin(dir, input string, args ...string) rulesOut {
	s.t.Helper()
	return s.mustRun(s.command(s.bin, dir, nil, args...), input)
}

// rules writes the rules file where GitDCY's own config says it is.
func (s *rulesSandbox) rules(text string) string {
	s.t.Helper()
	rules := filepath.Join(s.root, "fleet/repos.toml")
	rulesMkdir(s.t, filepath.Dir(rules))
	rulesWrite(s.t, rules, text)
	rulesWrite(s.t, filepath.Join(s.root, "home/.config/gitdcy/local.toml"), fmt.Sprintf("rules_file = \"%s\"\n", rules))
	return rules
}

// configFiles is the names in GitDCY's config folder, sorted.
func (s *rulesSandbox) configFiles() []string {
	s.t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.root, "home/.config/gitdcy"))
	if err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// repo is a working repository with one commit and a bare origin next to
// it, and its first commit.
func (s *rulesSandbox) repo(name string) (string, string) {
	s.t.Helper()
	work := filepath.Join(s.root, name)
	forge := filepath.Join(s.root, name+"-forge.git")
	s.ok(s.root, "init", "-q", "--bare", forge)
	s.ok(s.root, "init", "-q", work)
	rulesWrite(s.t, filepath.Join(work, "README"), "hello\n")
	s.ok(work, "add", "README")
	s.ok(work, "commit", "-qm", "first")
	s.ok(work, "remote", "add", "origin", forge)
	return work, s.ok(work, "rev-parse", "HEAD")
}

func (s *rulesSandbox) commit(work, file, content string) string {
	s.t.Helper()
	rulesWrite(s.t, filepath.Join(work, file), content)
	s.ok(work, "add", file)
	s.ok(work, "commit", "-qm", "change "+file)
	return s.ok(work, "rev-parse", "HEAD")
}

func rulesJSON(t *testing.T, out rulesOut) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(out.stdout), &v); err != nil {
		t.Fatalf("%v: %s", err, out.text())
	}
	return v
}

// rulesAt is the value at path (object keys and list indexes) in v; nil
// when it is not there.
func rulesAt(v any, path ...any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[key]
		case int:
			list, _ := v.([]any)
			if key >= len(list) {
				return nil
			}
			v = list[key]
		}
	}
	return v
}

func rulesLen(v any) int {
	list, _ := v.([]any)
	return len(list)
}

// rulesPrivateAddress is built here so this file holds no address itself.
func rulesPrivateAddress() string { return "someone" + "@" + "gmail.com" }

func rulesRule(first, push, extra string) string {
	return fmt.Sprintf("selfhosted = [\"forge.example.com\"]\n\n[[repo]]\nname = \"example-app\"\nfirst_commit = \"%s\"\npush = %s\nnote = \"Customer data stays on our own forge\"\n%s", first, push, extra)
}

func TestRulesCmdASelfhostedRuleRefusesAPublicRemoteAndLetsItsOwnForgeThrough(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"selfhosted"`, ""))
	if out := s.gitdcy(work, "rules", "install"); out.code != 0 {
		t.Fatal(out.text())
	}
	s.ok(work, "checkout", "-qb", "feature")
	if out := s.git(work, "push", "-q", "origin", "feature"); out.code != 0 {
		t.Fatalf("a path on this device is allowed: %s", out.text())
	}
	head := s.ok(work, "rev-parse", "HEAD")
	input := fmt.Sprintf("refs/heads/feature %s refs/heads/feature %s\n", head, rulesZero)
	out := s.gitdcyStdin(work, input, "rules", "check", "--stdin", "github", "https://user:t0ken@github.com/example-org/app.git")
	said := out.text()
	if out.code != 1 {
		t.Fatal(said)
	}
	for _, want := range []string{"REFUSED", "self-hosted hosts (forge.example.com)", "Customer data stays on our own forge", "question for the repository's owner"} {
		if !strings.Contains(said, want) {
			t.Errorf("no %q: %s", want, said)
		}
	}
	if strings.Contains(said, "t0ken") {
		t.Errorf("a token in a URL is never printed: %s", said)
	}
	if strings.Contains(said, "--no-verify") {
		t.Errorf("no way around it is offered: %s", said)
	}
	out = s.gitdcyStdin(work, input, "rules", "check", "--stdin", "forge", "ssh://git@forge.example.com:2222/team/app.git")
	if out.code != 0 {
		t.Errorf("a listed host is allowed: %s", out.text())
	}
}

func TestRulesCmdAProtectedBranchTakesOnlyAPullRequest(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"anywhere"`, "protected = [\"main\"]\n"))
	s.gitdcy(work, "rules", "install")
	out := s.git(work, "push", "-q", "origin", "main")
	said := out.text()
	if out.code == 0 || !strings.Contains(said, `Branch main of "example-app" takes no direct push`) || !strings.Contains(said, "git push origin HEAD:refs/heads/NEW-BRANCH") {
		t.Fatal(said)
	}
	if out := s.git(work, "push", "-q", "origin", "HEAD:refs/heads/topic"); out.code != 0 {
		t.Fatal(out.text())
	}
	// Deleting the protected branch on the remote writes to it too.
	out = s.gitdcyStdin(work, fmt.Sprintf("(delete) %s refs/heads/main %s\n", rulesZero, first), "rules", "check", "--stdin", "origin", "/srv/git/app.git")
	if out.code != 1 {
		t.Fatal(out.text())
	}
}

func TestRulesCmdNowhereRefusesEvenAPathOnThisDevice(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"nowhere"`, ""))
	s.gitdcy(work, "rules", "install")
	out := s.git(work, "push", "-q", "origin", "HEAD:refs/heads/topic")
	if out.code == 0 || !strings.Contains(out.text(), "is not pushed anywhere") {
		t.Fatal(out.text())
	}
}

func TestRulesCmdAListOfRemotesRefusesOneNotOnIt(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `["github.com/example-org/"]`, ""))
	s.gitdcy(work, "rules", "install")
	out := s.git(work, "push", "-q", "origin", "HEAD:refs/heads/topic")
	if out.code == 0 || !strings.Contains(out.text(), "may only be pushed to github.com/example-org/") {
		t.Fatal(out.text())
	}
	head := s.ok(work, "rev-parse", "HEAD")
	out = s.gitdcyStdin(work, fmt.Sprintf("refs/heads/topic %s refs/heads/topic %s\n", head, rulesZero), "rules", "check", "--stdin", "gh", "git@github.com:Example-Org/app.git")
	if out.code != 0 {
		t.Fatal(out.text())
	}
}

func TestRulesCmdWithoutARuleAPushGoesAnywhereAndIsStillReadForPersonalData(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("personal = [\"Jane Doe\"]\n")
	out := s.gitdcy(work, "rules", "check", "--format", "json")
	if out.code != 0 {
		t.Fatal(out.text())
	}
	v := rulesJSON(t, out)
	if rulesAt(v, "data", "rule") != nil || rulesAt(v, "data", "refused") != false {
		t.Fatal(out.text())
	}

	s.commit(work, "contact.txt", "mail "+rulesPrivateAddress()+"\n")
	out = s.gitdcy(work, "rules", "check")
	said := out.text()
	if out.code != 1 || !strings.Contains(said, "contact.txt:1: a private mail address (s…@gmail.com)") {
		t.Fatal(said)
	}
	if strings.Contains(said, rulesPrivateAddress()) {
		t.Errorf("the address is not printed whole: %s", said)
	}

	s.ok(work, "reset", "-q", "--hard", "HEAD~1")
	s.commit(work, "notes.txt", "met jane doe today\n")
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	if out.code != 1 {
		t.Fatal(out.text())
	}
	v = rulesJSON(t, out)
	if rulesAt(v, "data", "findings", 0, "kind") != "personal-word" || rulesAt(v, "data", "findings", 0, "place") != "notes.txt:1" {
		t.Fatal(out.text())
	}
	if strings.Contains(strings.ToLower(out.text()), "jane doe") {
		t.Error(out.text())
	}

	s.ok(work, "reset", "-q", "--hard", "HEAD~1")
	s.rules(fmt.Sprintf("allow = [\"%s\"]\n", rulesPrivateAddress()))
	s.commit(work, "contact.txt", "mail "+rulesPrivateAddress()+"\n")
	if out := s.gitdcy(work, "rules", "check"); out.code != 0 {
		t.Errorf("an allowed address passes: %s", out.text())
	}
}

func TestRulesCmdTheCopyHoldsOnAMachineWithoutTheRulesFile(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	rules := s.rules(rulesRule(first, `"selfhosted"`, ""))
	out := s.gitdcy(work, "rules", "install", "--format", "json")
	if out.code != 0 || rulesAt(rulesJSON(t, out), "data", "copy_written") != true {
		t.Fatal(out.text())
	}
	if err := os.Rename(rules, withExtension(rules, "toml.elsewhere")); err != nil {
		t.Fatal(err)
	}
	out = s.gitdcy(work, "rules", "--format", "json")
	v := rulesJSON(t, out)
	if rulesAt(v, "data", "source") != "installed-copy" || rulesAt(v, "data", "rule", "push") != "selfhosted" {
		t.Fatal(out.text())
	}
	if rulesAt(v, "outcome") != "partial" {
		t.Errorf("it says the rules file is missing: %s", out.text())
	}
	head := s.ok(work, "rev-parse", "HEAD")
	out = s.gitdcyStdin(work, fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", head, rulesZero), "rules", "check", "--stdin", "github", "git@github.com:example-org/app.git")
	if out.code != 1 || !strings.Contains(out.text(), "gitdcy-rules.toml (a copy of the owner's rules file)") {
		t.Fatal(out.text())
	}
}

func TestRulesCmdAnExistingHookIsKeptAsideAndChained(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"anywhere"`, ""))
	hooks := filepath.Join(work, ".git/hooks")
	marker := filepath.Join(s.root, "their-hook-ran")
	theirs := fmt.Sprintf("#!/bin/sh\ncat > %s\nexit \"${THEIR_EXIT:-0}\"\n", marker)
	rulesMkdir(t, hooks)
	rulesWrite(t, filepath.Join(hooks, "pre-push"), theirs)
	if err := os.Chmod(filepath.Join(hooks, "pre-push"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := s.gitdcy(work, "rules", "install", "--format", "json")
	if out.code != 0 {
		t.Fatal(out.text())
	}
	chained, _ := rulesAt(rulesJSON(t, out), "data", "chained").(string)
	if !strings.HasPrefix(chained, "pre-push.bak-") {
		t.Fatal(out.text())
	}
	if got := rulesRead(t, filepath.Join(hooks, chained)); got != theirs {
		t.Errorf("kept aside as %q", got)
	}
	if out := s.git(work, "push", "-q", "origin", "HEAD:refs/heads/topic"); out.code != 0 {
		t.Fatal(out.text())
	}
	if !strings.Contains(rulesRead(t, marker), "refs/heads/topic") {
		t.Error("their hook got the same input")
	}

	out = s.gitdcy(work, "rules", "install", "--format", "json")
	v := rulesJSON(t, out)
	if rulesAt(v, "data", "hook_written") != false || rulesAt(v, "data", "copy_written") != false {
		t.Errorf("a second run changes nothing: %s", out.text())
	}
	if rulesAt(v, "data", "chained") != chained {
		t.Errorf("the chain is kept: %s", out.text())
	}
	entries, _ := os.ReadDir(hooks)
	backups := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("%d backups", backups)
	}

	s.commit(work, "b.txt", "more\n")
	if out := s.gitEnv(work, []string{"THEIR_EXIT", "1"}, "push", "-q", "origin", "HEAD:refs/heads/topic"); out.code == 0 {
		t.Error("a refusal by their hook stops the push")
	}
}

func TestRulesCmdInstallWritesNothingWhenCoreHooksPathWouldSkipTheHook(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"selfhosted"`, ""))
	rulesMkdir(t, filepath.Join(work, ".husky"))
	rulesWrite(t, filepath.Join(work, ".husky/pre-push"), "#!/bin/sh\nnpm test\n")
	s.ok(work, "config", "core.hooksPath", ".husky")
	out := s.gitdcy(work, "rules", "install")
	if out.code != 1 || !strings.Contains(out.text(), "core.hooksPath") {
		t.Fatal(out.text())
	}
	if rulesExists(filepath.Join(work, ".git/hooks/pre-push")) || rulesExists(filepath.Join(work, ".git/gitdcy-rules.toml")) {
		t.Error("something was written")
	}
	// A hook there that calls the repository's own is enough.
	rulesWrite(t, filepath.Join(work, ".husky/pre-push"), "#!/bin/sh\n\"$(git rev-parse --git-dir)/hooks/pre-push\" \"$@\"\n")
	if out := s.gitdcy(work, "rules", "install"); out.code != 0 {
		t.Fatal(out.text())
	}
}

func TestRulesCmdInstallRefusesWithoutARuleAndTwoRulesRefuseEveryPush(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules("selfhosted = []\n")
	out := s.gitdcy(work, "rules", "install")
	if out.code != 1 || !strings.Contains(out.text(), fmt.Sprintf("first_commit = \"%s\"", first)) {
		t.Fatal(out.text())
	}
	if rulesExists(filepath.Join(work, ".git/hooks/pre-push")) {
		t.Error("a hook was written")
	}
	forge := s.ok(work, "remote", "get-url", "origin")
	s.rules(fmt.Sprintf("[[repo]]\nname = \"one\"\nfirst_commit = \"%s\"\n\n[[repo]]\nname = \"two\"\nremotes = [\"%s\"]\npush = \"nowhere\"\n", first, forge))
	if out := s.gitdcy(work, "rules", "install"); out.code != 1 {
		t.Fatal(out.text())
	}
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	if out.code != 1 {
		t.Fatal(out.text())
	}
	v := rulesJSON(t, out)
	reason, _ := rulesAt(v, "data", "reasons", 0).(string)
	if rulesAt(v, "data", "refused") != true || !strings.Contains(reason, "Rules one, two") {
		t.Fatal(out.text())
	}
}

func TestRulesCmdARuleIsFoundByItsRemoteWhenTheFirstCommitIsNotKnown(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	forge := s.ok(work, "remote", "get-url", "origin")
	s.rules(fmt.Sprintf("[[repo]]\nname = \"by-remote\"\nremotes = [\"%s/\"]\npush = \"nowhere\"\n", forge))
	out := s.gitdcy(work, "rules", "--format", "json")
	v := rulesJSON(t, out)
	matched, _ := rulesAt(v, "data", "matched_by", 0).(string)
	if rulesAt(v, "data", "rule", "name") != "by-remote" || !strings.HasPrefix(matched, "remote origin") || rulesAt(v, "data", "remotes", 0, "allowed") != false {
		t.Fatal(out.text())
	}
}

func TestRulesCmdALocalSettingsFileInTheRepositoryCannotMoveTheRulesFile(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"nowhere"`, ""))
	empty := filepath.Join(s.root, "empty.toml")
	rulesWrite(t, empty, "")
	for _, setting := range [][2]string{
		{".gitdcy.local.toml", fmt.Sprintf("rules_file = \"%s\"\n", empty)},
		{".gitdcy.local.yaml", fmt.Sprintf("rules_file: %s\n", empty)},
	} {
		rulesWrite(t, filepath.Join(work, setting[0]), setting[1])
		out := s.gitdcy(work, "rules", "check", "--format", "json")
		v := rulesJSON(t, out)
		if rulesAt(v, "data", "rule") != "example-app" || rulesAt(v, "data", "refused") != true {
			t.Errorf("%s: %s", setting[0], out.text())
		}
		if err := os.Remove(filepath.Join(work, setting[0])); err != nil {
			t.Fatal(err)
		}
	}
}

// The YAML settings of a build before 2026-11-08, met by this one.
func TestRulesCmdAYAMLSettingsFileBecomesTOMLAndKeepsACopy(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	rules := s.rules(rulesRule(first, `"nowhere"`, ""))
	config := filepath.Join(s.root, "home/.config/gitdcy")
	if err := os.Remove(filepath.Join(config, "local.toml")); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("# where the rules are\nrules_file: %s\n", rules)
	rulesWrite(t, filepath.Join(config, "local.yaml"), yaml)

	// Read only: read as YAML, nothing written.
	out := s.mustRun(s.command(s.bin, work, []string{"GITDCY_READ_ONLY_STATUS", "1"}, "rules", "--format", "json"), "")
	if rulesAt(rulesJSON(t, out), "data", "rule", "name") != "example-app" {
		t.Fatal(out.text())
	}
	if files := s.configFiles(); strings.Join(files, ",") != "local.yaml" {
		t.Fatalf("%v", files)
	}

	out = s.gitdcy(work, "rules", "--format", "json")
	if rulesAt(rulesJSON(t, out), "data", "rule", "name") != "example-app" {
		t.Fatal(out.text())
	}
	files := s.configFiles()
	if len(files) != 2 || files[0] != "local.toml" || !strings.HasPrefix(files[1], "local.yaml.bak-") {
		t.Fatalf("%v", files)
	}
	if got := rulesRead(t, filepath.Join(config, files[1])); got != yaml {
		t.Errorf("kept %q", got)
	}
	if got := rulesRead(t, filepath.Join(config, "local.toml")); got != fmt.Sprintf("rules_file = \"%s\"\n", rules) {
		t.Errorf("local.toml %q", got)
	}
	// And it keeps working from the TOML.
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	if rulesAt(rulesJSON(t, out), "data", "refused") != true {
		t.Fatal(out.text())
	}
}

func TestRulesCmdYAMLThatWouldLoseSomethingOrCannotBeReadStaysAsItIs(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	rules := s.rules(rulesRule(first, `"nowhere"`, ""))
	config := filepath.Join(s.root, "home/.config/gitdcy")
	if err := os.Remove(filepath.Join(config, "local.toml")); err != nil {
		t.Fatal(err)
	}

	// A key that is no setting: still read, not converted, and doctor says
	// why.
	yaml := fmt.Sprintf("rules_file: %s\ncolour: blue\n", rules)
	rulesWrite(t, filepath.Join(config, "local.yaml"), yaml)
	out := s.gitdcy(work, "rules", "--format", "json")
	if rulesAt(rulesJSON(t, out), "data", "rule", "name") != "example-app" {
		t.Fatal(out.text())
	}
	if files := s.configFiles(); strings.Join(files, ",") != "local.yaml" {
		t.Fatalf("%v", files)
	}
	out = s.gitdcy(work, "doctor")
	if !strings.Contains(out.text(), "local.yaml is read as YAML, not converted to TOML: colour is not a GitDCY setting") {
		t.Error(out.text())
	}

	// YAML this build does not read: refused (the push check fails closed),
	// the file untouched.
	yaml = fmt.Sprintf("base: &r %s\nrules_file: *r\n", rules)
	rulesWrite(t, filepath.Join(config, "local.yaml"), yaml)
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	if out.code != 1 || !strings.Contains(out.text(), "an anchor") {
		t.Fatal(out.text())
	}
	if files := s.configFiles(); strings.Join(files, ",") != "local.yaml" {
		t.Fatalf("%v", files)
	}
	if got := rulesRead(t, filepath.Join(config, "local.yaml")); got != yaml {
		t.Errorf("changed to %q", got)
	}
}

func TestRulesCmdACheckoutsYAMLIsConvertedOnlyWhereGitIgnoresTheResult(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"nowhere"`, ""))
	rulesWrite(t, filepath.Join(work, ".gitdcy.local.yaml"), "workspace_root: ~/Code\n")
	s.ok(work, "add", ".gitdcy.local.yaml")
	s.ok(work, "commit", "-q", "-m", "local settings, tracked")
	// Its settings are read where GitDCY looks at repositories (status).
	s.gitdcy(work, "status", "--format", "json")
	// Tracked, and nothing ignores the new names: left as it is.
	if !rulesExists(filepath.Join(work, ".gitdcy.local.yaml")) || rulesExists(filepath.Join(work, ".gitdcy.local.toml")) {
		t.Fatal("converted a tracked file")
	}
	if got := s.ok(work, "status", "--porcelain"); got != "" {
		t.Fatal(got)
	}
	// Tracked, even with the new names ignored: still left.
	rulesWrite(t, filepath.Join(work, ".git/info/exclude"), ".gitdcy.local.toml\n.gitdcy.local.yaml.bak-*\n")
	s.gitdcy(work, "status", "--format", "json")
	if !rulesExists(filepath.Join(work, ".gitdcy.local.yaml")) {
		t.Fatal("converted a tracked file")
	}
	if got := s.ok(work, "status", "--porcelain"); got != "" {
		t.Fatal(got)
	}

	s.ok(work, "rm", "-q", "--cached", ".gitdcy.local.yaml")
	s.ok(work, "commit", "-q", "-m", "untracked again")
	rulesWrite(t, filepath.Join(work, ".git/info/exclude"), ".gitdcy.local.yaml\n.gitdcy.local.toml\n.gitdcy.local.yaml.bak-*\n")
	s.gitdcy(work, "status", "--format", "json")
	if rulesExists(filepath.Join(work, ".gitdcy.local.yaml")) {
		t.Fatal("not converted")
	}
	if got := rulesRead(t, filepath.Join(work, ".gitdcy.local.toml")); got != "workspace_root = \"~/Code\"\n" {
		t.Errorf("converted to %q", got)
	}
	if got := s.ok(work, "status", "--porcelain"); got != "" {
		t.Fatal(got)
	}
}

func TestRulesCmdErrorsComeAsTheJSONEnvelope(t *testing.T) {
	s := newRulesSandbox(t)
	notARepo := filepath.Join(s.root, "plain")
	rulesMkdir(t, notARepo)
	out := s.gitdcy(notARepo, "rules", "--format", "json")
	if out.code != 1 {
		t.Fatal(out.text())
	}
	v := rulesJSON(t, out)
	message, _ := rulesAt(v, "error", "message").(string)
	if rulesAt(v, "schema_version") != "gitdcy.response.v1" || rulesAt(v, "operation") != "rules" || rulesAt(v, "outcome") != "error" || !strings.Contains(message, "not inside a git repository") {
		t.Fatal(out.text())
	}

	work, _ := s.repo("app")
	s.rules("[[repo]]\nname = \"x\"\npush = \"github\"\nremotes = [\"x/y\"]\n")
	out = s.gitdcy(work, "rules", "check")
	if out.code != 1 {
		t.Fatalf("a broken rules file refuses the push: %s", out.text())
	}
	if !strings.Contains(out.text(), "is not valid") {
		t.Fatal(out.text())
	}
}

func TestRulesCmdTheHookRefusesWhenGitdcyIsGone(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.rules(rulesRule(first, `"anywhere"`, ""))
	bin := filepath.Join(s.root, "bin")
	rulesMkdir(t, bin)
	data, err := os.ReadFile(s.bin)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(bin, "gitdcy")
	if err := os.WriteFile(copied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	// A test that forks while the copy is still open for writing keeps it
	// busy for a moment ("Text file busy"): start it again.
	var out rulesOut
	for try := 0; ; try++ {
		out, err = s.run(s.command(copied, work, nil, "rules", "install"), "")
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ETXTBSY) || try == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out.code != 0 {
		t.Fatal(out.text())
	}
	if err := os.Rename(copied, copied+".moved"); err != nil {
		t.Fatal(err)
	}
	out = s.gitEnv(work, []string{"PATH", "/usr/bin:/bin"}, "push", "-q", "origin", "HEAD:refs/heads/topic")
	if out.code == 0 || !strings.Contains(out.text(), "gitdcy is not installed here") {
		t.Fatal(out.text())
	}
}

// rulesFakeGh puts a gh in front of the PATH that runs answer (a shell
// line).
func rulesFakeGh(s *rulesSandbox, answer string) {
	bin := filepath.Join(s.root, "fakebin")
	rulesMkdir(s.t, bin)
	rulesWrite(s.t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+answer+"\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.path = bin + ":" + s.path
}

// rulesOldCommit makes a commit dated days ago, on the branch checked out.
func rulesOldCommit(s *rulesSandbox, work, file string, days int64) {
	when := fmt.Sprintf("@%d +0000", time.Now().Unix()-days*86400)
	rulesWrite(s.t, filepath.Join(work, file), "old work\n")
	s.ok(work, "add", file)
	out := s.gitEnv(work, []string{"GIT_AUTHOR_DATE", when, "GIT_COMMITTER_DATE", when}, "commit", "-qm", "old work")
	if out.code != 0 {
		s.t.Fatal(out.text())
	}
}

const rulesOnePR = `echo '[{"number":7,"title":"Fix the thing","url":"https://github.com/example-org/app/pull/7","createdAt":"2026-01-01T00:00:00Z","author":{"login":"someone"},"repository":{"nameWithOwner":"example-org/app"},"isDraft":false}]'`

func TestRulesCmdAScanNamesEachRuleItsForbiddenRemotesAndPushedPersonalData(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	other, _ := s.repo("other")
	// Made in the same second, both first commits would be one and the same.
	s.ok(other, "commit", "-q", "--amend", "-m", "another first")
	s.rules(rulesRule(first, `"selfhosted"`, ""))
	s.ok(work, "remote", "add", "github", "https://github.com/example-org/app.git")
	// What already reached a remote: a commit with a private address.
	s.commit(other, "NOTES", "mail "+rulesPrivateAddress()+"\n")
	s.ok(other, "push", "-q", "origin", "main")

	out := s.gitdcy(s.root, "rules", "scan", "--format", "json", work, other, "/nonexistent/x")
	v := rulesJSON(t, out)
	if out.code != 0 || rulesAt(v, "operation") != "rules.scan" || rulesLen(rulesAt(v, "data", "checkouts")) != 3 {
		t.Fatal(out.text())
	}
	app := rulesAt(v, "data", "checkouts", 0)
	if rulesAt(app, "rule") != "example-app" || rulesLen(rulesAt(app, "forbidden")) != 1 ||
		rulesAt(app, "forbidden", 0, "name") != "github" || rulesAt(app, "forbidden", 0, "url") != "github.com/example-org/app" {
		t.Errorf("%v", app)
	}
	if rulesAt(app, "hygiene") != nil {
		t.Errorf("no [hygiene], no reminders: %v", app)
	}
	otherScan := rulesAt(v, "data", "checkouts", 1)
	if rulesAt(otherScan, "rule") != nil || rulesLen(rulesAt(otherScan, "forbidden")) != 0 || rulesLen(rulesAt(otherScan, "pushed_findings")) != 1 {
		t.Errorf("%v", otherScan)
	}
	if strings.Contains(out.stdout, rulesPrivateAddress()) {
		t.Error("not masked")
	}
	if rulesAt(v, "data", "checkouts", 2, "state") != "missing" || rulesAt(v, "outcome") != "partial" {
		t.Errorf("a missing checkout is a warning: %s", out.text())
	}

	// The same in words: the forbidden remote and the pushed finding.
	said := s.gitdcy(s.root, "rules", "scan", work, other).text()
	if !strings.Contains(said, "remote github -> github.com/example-org/app") || !strings.Contains(said, "pushed commit") {
		t.Error(said)
	}
}

func TestRulesCmdRemindersNameTheDirtyDishesAndNeverRefuseAPush(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.ok(work, "push", "-q", "origin", "main")
	s.ok(work, "branch", "-q", "--set-upstream-to=origin/main")
	s.ok(work, "remote", "add", "github", "https://github.com/example-org/app.git")
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\nbranch_days = 3\n")
	rulesFakeGh(s, rulesOnePR)
	s.ok(work, "checkout", "-qb", "old-idea")
	rulesOldCommit(s, work, "IDEA", 10)
	s.ok(work, "checkout", "-q", "main")
	rulesWrite(t, filepath.Join(work, "README"), "changed\n")

	out := s.gitdcy(work, "rules", "scan", "--format", "json", ".")
	dishes := rulesAt(rulesJSON(t, out), "data", "checkouts", 0, "hygiene")
	var kinds []string
	for i := 0; i < rulesLen(rulesAt(dishes, "reminders")); i++ {
		kind, _ := rulesAt(dishes, "reminders", i, "kind").(string)
		kinds = append(kinds, kind)
	}
	if strings.Join(kinds, ",") != "dirty,unpushed,branch,pull-request" {
		t.Fatalf("%v: %s", kinds, out.text())
	}
	days, _ := rulesAt(dishes, "branches", 0, "days").(float64)
	if rulesAt(dishes, "branches", 0, "branch") != "old-idea" || days < 10 ||
		rulesAt(dishes, "unpushed_before_today") != float64(1) || rulesAt(dishes, "pull_requests", "open", 0, "number") != float64(7) {
		t.Errorf("%s", out.text())
	}

	// Before a push: the reminders, and the push goes.
	s.gitdcy(work, "rules", "install")
	s.ok(work, "checkout", "-q", "--", "README")
	s.commit(work, "NEW", "new\n")
	out = s.git(work, "push", "-q", "origin", "main")
	said := out.text()
	if out.code != 0 {
		t.Fatalf("a reminder never refuses: %s", said)
	}
	for _, want := range []string{"left lying around", "branch old-idea not merged into origin/main", "pull request #7 open", "1 commit(s) on other branches are on no remote"} {
		if !strings.Contains(said, want) {
			t.Errorf("no %q: %s", want, said)
		}
	}
}

func TestRulesCmdWithoutGhPullRequestsAreUnknownAndASkippedRepositoryGetsNoReminders(t *testing.T) {
	s := newRulesSandbox(t)
	work, first := s.repo("app")
	s.ok(work, "remote", "add", "github", "https://github.com/example-org/app.git")
	rulesFakeGh(s, "echo 'To get started with GitHub CLI, please run:  gh auth login' >&2; exit 4")
	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\n")
	out := s.gitdcy(work, "rules", "scan", "--format", "json", ".")
	v := rulesJSON(t, out)
	dishes := rulesAt(v, "data", "checkouts", 0, "hygiene")
	if rulesAt(v, "data", "pull_requests") != "unknown" || rulesAt(dishes, "pull_requests", "state") != "unknown" {
		t.Fatal(out.text())
	}
	found := false
	for i := 0; i < rulesLen(rulesAt(dishes, "reminders")); i++ {
		text, _ := rulesAt(dishes, "reminders", i, "text").(string)
		if rulesAt(dishes, "reminders", i, "kind") == "pull-requests-unknown" && strings.Contains(text, "gh auth login") {
			found = true
		}
	}
	if !found {
		t.Errorf("%s", out.text())
	}
	// In words it is said once for the scan, not per checkout.
	if said := s.gitdcy(work, "rules", "scan", ".").text(); strings.Count(said, "open pull requests") != 1 {
		t.Error(said)
	}

	s.rules(rulesRule(first, `"anywhere"`, "") + "\n[hygiene]\nskip = [\"github.com/Example-Org/\"]\n")
	out = s.gitdcy(work, "rules", "scan", "--format", "json", ".")
	checkout := rulesAt(rulesJSON(t, out), "data", "checkouts", 0)
	off, _ := rulesAt(checkout, "hygiene_off").(string)
	if rulesAt(checkout, "hygiene") != nil || !strings.Contains(off, "keeps its own rules") {
		t.Errorf("%v", checkout)
	}
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	if reminders, ok := rulesAt(rulesJSON(t, out), "data", "reminders").([]any); !ok || len(reminders) != 0 {
		t.Error(out.text())
	}
}
