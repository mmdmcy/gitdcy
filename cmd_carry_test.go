package main

// `gitdcy carry` against the shell scripts it replaces, on throwaway
// repositories: the same refs, stash entries, working tree and lines, the
// refused cases included (a branch name taken, a dirty checkout, another
// commit). Both run as processes with the same clean environment.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// carryG is how the reference scripts run git: no hooks, no fsmonitor.
const carryG = "G='git -c core.hooksPath=/dev/null -c core.fsmonitor=false'"

// carryLab is a folder of throwaway repositories with a home of its own.
type carryLab struct {
	t    *testing.T
	root string
}

func newCarryLab(t *testing.T) *carryLab {
	t.Helper()
	factsNeedsSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &carryLab{t, root}
}

func (l *carryLab) path(name string) string { return filepath.Join(l.root, name) }

func (l *carryLab) command(program, dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+l.path("home"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	return cmd
}

// trySh is a script's success and its trimmed standard output.
func (l *carryLab) trySh(dir, script string) (bool, string) {
	out, err := l.command("sh", dir, "-c", script).Output()
	return err == nil, trim(string(out))
}

func (l *carryLab) sh(dir, script string) string {
	l.t.Helper()
	cmd := l.command("sh", dir, "-c", script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		l.t.Fatalf("%s\n%v %s", script, err, stderr.String())
	}
	return trim(string(out))
}

// gitdcy is the binary's success and its trimmed standard output followed
// by its trimmed standard error.
func (l *carryLab) gitdcy(dir string, args ...string) (bool, string) {
	l.t.Helper()
	cmd := l.command(gitdcyBinary(l.t), dir, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return err == nil, trim(stdout.String()) + trim(stderr.String())
}

func (l *carryLab) copy(from, to string) string {
	l.sh(l.root, fmt.Sprintf("rm -rf %s && cp -R %s %s", to, from, to))
	return l.path(to)
}

// state is everything about a checkout carry may change.
func (l *carryLab) state(dir string) string {
	return l.sh(dir, "git for-each-ref --format='%(refname) %(objectname)' && echo -- && "+
		"git stash list --format='%H %gs' && echo -- && "+
		"git status --porcelain --untracked-files=all && echo -- && "+
		"find . -path ./.git -prune -o -type f -print | LC_ALL=C sort | "+
		"while IFS= read -r f; do cksum \"$f\"; done")
}

// carryReferencePlace is the placement script `gitdcy carry place`
// replaces, word for word apart from the message and the folder.
func carryReferencePlace(prefix, from, operation, head, message string) string {
	return strings.NewReplacer("{G}", carryG, "{prefix}", prefix, "{from}", from, "{operation}", operation,
		"{head}", head, "{message}", message).Replace(`set -eu
{G}
P={prefix}
for ref in $($G for-each-ref --format='%(refname)' "$P/heads/"); do
  b=${ref#"$P/heads/"}; sha=$($G rev-parse "$ref")
  if $G update-ref "refs/heads/$b" "$sha" "" 2>/dev/null; then echo "branch $b"
  elif [ "$($G rev-parse "refs/heads/$b")" = "$sha" ]; then :
  elif $G update-ref "refs/heads/carry/{from}/$b" "$sha" "" 2>/dev/null; then echo "branch carry/{from}/$b"
  else echo "kept $ref"; fi
done
for i in $($G for-each-ref --format='%(refname:lstrip=-1)' "$P/stash/" | sort -rn); do
  sha=$($G rev-parse "$P/stash/$i")
  if ! $G stash list --format=%H | grep -qx "$sha"; then $G stash store -m '{message}' "$sha" && echo "stash $sha"; fi
done
wip=$($G rev-parse -q --verify "$P/wip^{commit}")
if [ "$($G rev-parse -q --verify HEAD || true)" = '{head}' ] && [ -z "$($G status --porcelain)" ] && $G diff --binary HEAD "$wip" | $G apply --whitespace=nowarn; then echo 'applied the working tree'
elif $G update-ref 'refs/heads/carry/{from}/wip-{operation}' "$wip" "" 2>/dev/null; then echo 'branch carry/{from}/wip-{operation} (successor checkout differs)'
else echo "kept $P/wip"; fi
`)
}

// carryReferenceFastForward is the fast-forward script `place
// --fast-forward` replaces.
func carryReferenceFastForward(prefix, branch, head string) string {
	return strings.NewReplacer("{G}", carryG, "{prefix}", prefix, "{branch}", branch, "{head}", head,
		"{short}", head[:8]).Replace(`set -eu
{G}
if [ "$($G symbolic-ref -q --short HEAD || true)" != {branch} ]; then echo 'not on {branch}; left as is'
elif $G merge-base --is-ancestor {head} HEAD; then echo 'already ahead; left as is'
elif ! $G merge-base --is-ancestor HEAD {head}; then echo 'diverged; left as is, {prefix}/head keeps the device commit'
elif $G merge --ff-only --quiet {head}; then echo 'fast-forwarded to {short}'
else echo 'fast-forward refused (local changes in the way); {prefix}/head keeps the commit'; fi
`)
}

// carryReferenceSend is the push script `gitdcy carry send` replaces, into
// a local path.
func carryReferenceSend(prefix, to string, tips, stashes []string, message string) string {
	script := "set -eu\n" + carryG + "\nrefs=\"\"\n"
	for _, tip := range tips {
		script += strings.NewReplacer("{tip}", tip, "{prefix}", prefix).Replace(`found=0
for b in $($G for-each-ref --points-at {tip} --format='%(refname:lstrip=2)' refs/heads); do refs="$refs {tip}:{prefix}/heads/$b"; found=1; done
[ "$found" = 1 ] || refs="$refs {tip}:{prefix}/tips/{tip}"
`)
	}
	for index, stash := range stashes {
		script += fmt.Sprintf("refs=\"$refs %s:%s/stash/%d\"\n", stash, prefix, index)
	}
	script += strings.NewReplacer("{prefix}", prefix, "{message}", message, "{RECEIVE}", carryReceivePack, "{to}", to).Replace(`objects=$($G rev-parse --path-format=absolute --git-path objects)
index=$($G rev-parse --path-format=absolute --git-path index)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' 0
mkdir "$scratch/objects"
[ -f "$index" ] && cp "$index" "$scratch/index"
GIT_INDEX_FILE="$scratch/index" GIT_OBJECT_DIRECTORY="$scratch/objects" GIT_ALTERNATE_OBJECT_DIRECTORIES="$objects"
export GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
$G add -A
tree=$($G write-tree)
if $G rev-parse -q --verify HEAD >/dev/null; then wip=$($G commit-tree "$tree" -p HEAD -m '{message}'); else wip=$($G commit-tree "$tree" -m '{message}'); fi
refs="$refs $wip:{prefix}/wip"
$G push --no-verify --quiet --receive-pack='{RECEIVE}' '{to}' $refs
echo pushed to {prefix}
`)
	return script
}

// carryDeviceAndReceiver is a device with two local-only branches, a stash
// and uncommitted work (an edit, an untracked file with a space, a
// deletion), and a receiving checkout of its main branch.
func carryDeviceAndReceiver(l *carryLab) (string, string) {
	l.sh(l.root, "git init -q -b main device && cd device && "+
		"echo base > a.txt && echo keep > b.txt && git add -A && git commit -qm base && "+
		"git switch -qc feature && echo f > f.txt && git add f.txt && git commit -qm feature && "+
		"git switch -qc topic main && git commit -q --allow-empty -m topic && "+
		"git switch -q main && echo stashed > a.txt && git stash -q && "+
		"echo edited > a.txt && echo new > 'new file.txt' && rm b.txt")
	l.sh(l.root, "git init -q -b main receiver && cd receiver && git fetch -q ../device main && "+
		"git switch -qc main FETCH_HEAD")
	return l.path("device"), l.path("receiver")
}

func TestCarrySendPushesWhatTheOldPushDidAndLeavesTheDeviceAsItWas(t *testing.T) {
	l := newCarryLab(t)
	device, _ := carryDeviceAndReceiver(l)
	rev := func(name string) string { return l.sh(device, "git rev-parse "+name) }
	feature, topic, stash := rev("feature"), rev("topic"), rev("stash@{0}")
	before := l.state(device)
	old := l.copy("receiver", "old")
	fresh := l.copy("receiver", "new")
	prefix := "refs/carry/mini/op1"
	said := l.sh(device, carryReferenceSend(prefix, old, []string{feature, topic}, []string{stash}, "wip(mini): carried"))
	if said != "pushed to "+prefix {
		t.Fatal(said)
	}
	if l.state(device) != before {
		t.Fatal("the reference script wrote to the device")
	}
	ok, said := l.gitdcy(device, "carry", "send", fresh, "--from", "mini", "--operation", "op1",
		"--tip", feature, "--tip", topic, "--stash", stash, "--worktree", "--message", "wip(mini): carried")
	if !ok || said != "pushed to "+prefix {
		t.Fatalf("%v %s", ok, said)
	}
	if l.state(device) != before {
		t.Fatal("the device was written to")
	}
	refs := func(dir string) string {
		return l.sh(dir, "git for-each-ref --format='%(refname) %(objectname)' refs/carry/ | grep -v /wip")
	}
	if refs(old) != refs(fresh) {
		t.Fatalf("refs differ:\n%s\n%s", refs(old), refs(fresh))
	}
	for _, ref := range []string{"refs/carry/mini/op1/heads/feature", "refs/carry/mini/op1/stash/0"} {
		if !strings.Contains(refs(fresh), ref) {
			t.Fatalf("no %s in %s", ref, refs(fresh))
		}
	}
	// The working-tree commit: same tree, parent and message.
	wip := func(dir string) string { return l.sh(dir, "git log -1 --format='%T %P %B' refs/carry/mini/op1/wip") }
	if wip(old) != wip(fresh) {
		t.Fatalf("wip differs:\n%s\n%s", wip(old), wip(fresh))
	}
	if got := l.sh(fresh, "git show refs/carry/mini/op1/wip:'new file.txt'"); got != "new" {
		t.Fatal(got)
	}
}

func TestCarrySendRefusesWhatItCannotCarry(t *testing.T) {
	l := newCarryLab(t)
	device, receiver := carryDeviceAndReceiver(l)
	send := func(args ...string) (bool, string) {
		return l.gitdcy(device, append([]string{"carry", "send", receiver}, args...)...)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--from", "mini", "--operation", "op1"}, "nothing to carry"},
		{[]string{"--from", "../x", "--operation", "op1", "--worktree"}, "not a plain name"},
		{[]string{"--from", "mini", "--operation", "op1", "--tip", "nosuch"}, "not a commit"},
	} {
		if ok, said := send(c.args...); ok || !strings.Contains(said, c.want) {
			t.Fatalf("%q: %v %s", c.args, ok, said)
		}
	}
	if got := l.sh(receiver, "git for-each-ref refs/carry/"); got != "" {
		t.Fatal(got)
	}
}

func TestCarryPlaceDoesWhatTheOldPlacementDidInEveryCase(t *testing.T) {
	l := newCarryLab(t)
	device, receiver := carryDeviceAndReceiver(l)
	head := l.sh(device, "git rev-parse HEAD")
	rev := func(name string) string { return l.sh(device, "git rev-parse "+name) }
	feature, topic, stash := rev("feature"), rev("topic"), rev("stash@{0}")
	ok, said := l.gitdcy(device, "carry", "send", receiver, "--from", "mini", "--operation", "op1",
		"--tip", feature, "--tip", topic, "--stash", stash, "--worktree")
	if !ok {
		t.Fatal(said)
	}
	message := "carried from mini"
	reference := carryReferencePlace("refs/carry/mini/op1", "mini", "op1", head, message)
	place := []string{"carry", "place", "--from", "mini", "--operation", "op1", "--message", message}

	// Each case: what the receiving checkout looks like before the placement.
	cases := []struct {
		name, before string
		expected     []string
	}{
		{"free", "true", []string{"branch feature", "branch topic", "stash", "applied the working tree"}},
		{"names-taken", "git branch feature HEAD && git branch topic HEAD && git branch carry/mini/topic HEAD",
			[]string{"branch carry/mini/feature", "kept refs/carry/mini/op1/heads/topic"}},
		{"same-branch", "git branch feature " + feature, []string{"branch topic"}},
		{"dirty", "echo mine > a.txt", []string{"branch carry/mini/wip-op1 (successor checkout differs)"}},
		{"untracked-only", "echo stray > stray.txt", []string{"branch carry/mini/wip-op1 (successor checkout differs)"}},
		{"other-commit", "git commit -q --allow-empty -m other", []string{"branch carry/mini/wip-op1 (successor checkout differs)"}},
		{"wip-name-taken", "echo mine > a.txt && git branch carry/mini/wip-op1 HEAD", []string{"kept refs/carry/mini/op1/wip"}},
		{"stash-there", "git stash store -m earlier " + stash, []string{"branch feature", "applied the working tree"}},
	}
	for _, c := range cases {
		prepared := l.copy("receiver", c.name+"-prepared")
		l.sh(prepared, c.before)
		old := l.copy(c.name+"-prepared", c.name+"-old")
		fresh := l.copy(c.name+"-prepared", c.name+"-new")
		oldSaid := l.sh(old, reference)
		ok, newSaid := l.gitdcy(fresh, place...)
		if !ok {
			t.Fatalf("%s: %s", c.name, newSaid)
		}
		if newSaid != oldSaid {
			t.Fatalf("%s: the lines differ:\n%s\n%s", c.name, oldSaid, newSaid)
		}
		if l.state(fresh) != l.state(old) {
			t.Fatalf("%s: the checkouts differ:\n%s\n%s", c.name, l.state(old), l.state(fresh))
		}
		for _, line := range c.expected {
			found := false
			for _, s := range lines(newSaid) {
				found = found || strings.HasPrefix(s, line)
			}
			if !found {
				t.Fatalf("%s: no %q in %q", c.name, line, newSaid)
			}
		}
		// Twice: nothing is placed again, the same way.
		oldAgain := l.sh(old, reference)
		_, newAgain := l.gitdcy(fresh, place...)
		if newAgain != oldAgain {
			t.Fatalf("%s: the second run differs:\n%s\n%s", c.name, oldAgain, newAgain)
		}
		if l.state(fresh) != l.state(old) {
			t.Fatalf("%s: second run", c.name)
		}
	}
	// The clean case at the same commit has the device's work, unstaged.
	free := l.path("free-new")
	if data, _ := os.ReadFile(filepath.Join(free, "a.txt")); string(data) != "edited\n" {
		t.Fatalf("a.txt %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(free, "new file.txt")); string(data) != "new\n" {
		t.Fatalf("new file.txt %q", data)
	}
	if _, err := os.Stat(filepath.Join(free, "b.txt")); err == nil {
		t.Fatal("b.txt is still there")
	}
	if got := l.sh(free, "git stash list --format='%H %gs'"); got != stash+" "+message {
		t.Fatal(got)
	}
}

func TestCarryFastForwardDoesWhatTheOldOneDid(t *testing.T) {
	l := newCarryLab(t)
	l.sh(l.root, "git init -q -b main device && cd device && echo one > a.txt && git add a.txt && "+
		"git commit -qm one && cd .. && git init -q -b main receiver && cd receiver && "+
		"git fetch -q ../device main && git switch -qc main FETCH_HEAD && "+
		"cd ../device && echo two > a.txt && git commit -qam two")
	device := l.path("device")
	head := l.sh(device, "git rev-parse HEAD")
	ok, said := l.gitdcy(device, "carry", "send", l.path("receiver"), "--from", "mini", "--operation", "op2", "--head", "HEAD")
	if !ok {
		t.Fatal(said)
	}
	reference := carryReferenceFastForward("refs/carry/mini/op2", "main", head)
	forward := []string{"carry", "place", "--from", "mini", "--operation", "op2", "--fast-forward", "main"}
	cases := [][3]string{
		{"behind", "true", "fast-forwarded to"},
		{"ahead", "git merge -q --ff-only " + head + " && git commit -q --allow-empty -m three", "already ahead"},
		{"same", "git merge -q --ff-only " + head, "already ahead"},
		{"no-commits-there", "git switch -q --orphan main2 && git branch -D main && git branch -m main2 main", "diverged"},
		{"diverged", "echo other > b.txt && git add b.txt && git commit -qm other", "diverged"},
		{"elsewhere", "git switch -qc other", "not on main"},
		{"in-the-way", "echo mine > a.txt", "fast-forward refused"},
	}
	for _, c := range cases {
		name, before, expected := c[0], c[1], c[2]
		prepared := l.copy("receiver", name+"-prepared")
		l.sh(prepared, before)
		old := l.copy(name+"-prepared", name+"-old")
		fresh := l.copy(name+"-prepared", name+"-new")
		oldSaid := l.sh(old, reference)
		ok, newSaid := l.gitdcy(fresh, forward...)
		if !ok {
			t.Fatalf("%s: %s", name, newSaid)
		}
		if newSaid != oldSaid {
			t.Fatalf("%s:\n%s\n%s", name, oldSaid, newSaid)
		}
		if !strings.HasPrefix(newSaid, expected) {
			t.Fatalf("%s: %s", name, newSaid)
		}
		if l.state(fresh) != l.state(old) {
			t.Fatalf("%s: the checkouts differ", name)
		}
	}
	// Nothing carried: it says so and changes nothing.
	ok, said = l.gitdcy(l.path("behind-new"), "carry", "place", "--from", "mini", "--operation", "none", "--fast-forward", "main")
	if ok || !strings.Contains(said, "nothing was carried") {
		t.Fatalf("%v %s", ok, said)
	}
}

func TestCarryAsideKeepsTheWorkUnderItsRefLikeTheOldStashStep(t *testing.T) {
	l := newCarryLab(t)
	device, _ := carryDeviceAndReceiver(l)
	l.sh(device, "git stash drop -q")
	old := l.copy("device", "old")
	fresh := l.copy("device", "new")
	reference := "refs/x/switched/desk-1"
	ok, _ := l.trySh(old, fmt.Sprintf("set -eu\ngit stash push --include-untracked -m 'switch to desk' >/dev/null\n"+
		"git update-ref -m 'switch to desk' %s 'stash@{0}'\ngit stash drop -q 'stash@{0}'\necho saved\n", reference))
	if !ok {
		t.Fatal("the reference script failed")
	}
	ok, said := l.gitdcy(fresh, "carry", "aside", "--as", reference, "--message", "switch to desk")
	if !ok {
		t.Fatal(said)
	}
	kept := func(dir string) string {
		return l.sh(dir, fmt.Sprintf("git status --porcelain --untracked-files=all && git stash list && "+
			"git rev-parse %[1]s^{tree} %[1]s^2^{tree} %[1]s^3^{tree} && git log -1 --format=%%s %[1]s", reference))
	}
	if kept(fresh) != kept(old) {
		t.Fatalf("kept differs:\n%s\n%s", kept(old), kept(fresh))
	}
	if data, _ := os.ReadFile(filepath.Join(fresh, "a.txt")); string(data) != "base\n" {
		t.Fatalf("a.txt %q", data)
	}
	l.sh(fresh, "git stash apply -q "+reference)
	if data, _ := os.ReadFile(filepath.Join(fresh, "new file.txt")); string(data) != "new\n" {
		t.Fatalf("new file.txt %q", data)
	}

	// Nothing to set aside: an earlier stash is not taken in its place.
	clean := l.copy("receiver", "clean")
	l.sh(clean, "echo s > a.txt && git stash -q")
	stashes := l.sh(clean, "git stash list --format=%H")
	ok, said = l.gitdcy(clean, "carry", "aside", "--as", reference)
	if ok || !strings.Contains(said, "no local changes") {
		t.Fatalf("%v %s", ok, said)
	}
	if got := l.sh(clean, "git stash list --format=%H"); got != stashes {
		t.Fatal(got)
	}
	if got := l.sh(clean, "git for-each-ref refs/x/"); got != "" {
		t.Fatal(got)
	}
	ok, said = l.gitdcy(fresh, "carry", "aside", "--as", "refs/heads/main")
	if ok || !strings.Contains(said, "not a ref of its own") {
		t.Fatalf("%v %s", ok, said)
	}
}

func TestRelateAnswersInJSONForScripts(t *testing.T) {
	l := newCarryLab(t)
	l.sh(l.root, "git init -q -b main repo && cd repo && git commit -q --allow-empty -m one && "+
		"git commit -q --allow-empty -m two")
	repo := l.path("repo")
	ok, said := l.gitdcy(repo, "relate", "HEAD~1", "HEAD", "--format", "json")
	if !ok {
		t.Fatal(said)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(said), &value); err != nil {
		t.Fatal(err)
	}
	if value["schema_version"] != "gitdcy.response.v1" {
		t.Fatalf("%v", value)
	}
	relation := value["data"].(map[string]any)["relations"].([]any)[0].(map[string]any)
	if relation["relation"] != "behind" || relation["ahead"] != 0.0 || relation["behind"] != 1.0 {
		t.Fatalf("%v", relation)
	}
	cmd := l.command(gitdcyBinary(t), repo, "relate", "--stdin", "--list", "5", "--format", "json")
	cmd.Stdin = strings.NewReader(repo + "\tHEAD\tHEAD~1\n/nonexistent\tHEAD\tHEAD\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	value = nil
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatal(err)
	}
	relations := value["data"].(map[string]any)["relations"].([]any)
	first, second := relations[0].(map[string]any), relations[1].(map[string]any)
	if first["relation"] != "ahead" || first["only_a"].([]any)[0].(map[string]any)["subject"] != "two" {
		t.Fatalf("%v", first)
	}
	if second["relation"] != "missing" || value["outcome"] != "partial" {
		t.Fatalf("%v", value)
	}
	ok, said = l.gitdcy(repo, "relate", "HEAD")
	if ok || !strings.Contains(said, "name two commits") {
		t.Fatalf("%v %s", ok, said)
	}
}
