package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// pushedRepo is a repository with a bare origin, three pushed commits that
// hold private addresses in their files and message, and one more on no
// remote.
func pushedRepo(t *testing.T) string {
	t.Helper()
	factsNeedsSh(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	factsSh(t, root, "git init -q --bare forge.git && git init -q work")
	factsSh(t, work, "git remote add origin ../forge.git && "+
		"echo hello > README && git add README && git -c commit.gpgsign=false commit -qm first && "+
		"printf 'a "+personalAt("one", "gmail.com")+"\\nb\\n' > A && git add A && git -c commit.gpgsign=false commit -qm 'add A' && "+
		"printf 'x\\n"+personalAt("two", "hotmail.com")+"\\n' > B && git add B && git -c commit.gpgsign=false commit -qm 'mail "+personalAt("three", "outlook.com")+"' && "+
		"git push -q origin HEAD && "+
		"echo local > C && git add C && git -c commit.gpgsign=false commit -qm 'on no remote "+personalAt("four", "gmail.com")+"'")
	return work
}

func TestPushedACachedScanFindsWhatAFullReadFinds(t *testing.T) {
	work := pushedRepo(t)
	revs := []string{"--remotes", "--since=7 days ago"}
	config := personalConfig{words: []string{"hello"}}
	allowed := map[string]bool{}
	full, err := personalLog(work, revs, "", config, allowed)
	if err != nil || len(full) != 3 {
		t.Fatalf("%v %v", full, err)
	}
	var want []Finding
	for _, c := range full {
		want = append(want, c.findings...)
	}
	if len(want) != 4 {
		t.Fatalf("the test repository holds four findings: %v", want)
	}

	// The first scan reads everything, the second nothing new, the third
	// only a commit pushed since; each finds what a full read finds.
	n, got, kept, err := pushedScan(work, "7 days ago", config, allowed, "x", nil)
	if err != nil || n != 3 || !reflect.DeepEqual(got, want) || len(kept.Commits) != 3 {
		t.Fatalf("first: %d %v %v %v", n, got, kept, err)
	}
	n, got, kept, err = pushedScan(work, "7 days ago", config, allowed, "x", kept)
	if err != nil || n != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("second: %d %v %v", n, got, err)
	}
	factsSh(t, work, "git push -q origin HEAD")
	full, _ = personalLog(work, revs, "", config, allowed)
	want = nil
	for _, c := range full {
		want = append(want, c.findings...)
	}
	n, got, kept, err = pushedScan(work, "7 days ago", config, allowed, "x", kept)
	if err != nil || n != 4 || len(want) != 5 || !reflect.DeepEqual(got, want) || len(kept.Commits) != 4 {
		t.Fatalf("third: %d %v, want %v, %v", n, got, want, err)
	}
}

func TestPushedARecordSeparatorInADiffIsNoCommitOfItsOwn(t *testing.T) {
	factsNeedsSh(t)
	work := t.TempDir()
	factsSh(t, work, "git init -q && printf 'one\\036"+strings.Repeat("a", 40)+"\\n"+personalAt("one", "gmail.com")+"\\n' > A && "+
		"git add A && git -c commit.gpgsign=false commit -qm first")
	commits, err := personalLog(work, []string{"HEAD"}, "", personalConfig{}, map[string]bool{})
	if err != nil || len(commits) != 1 || len(commits[0].findings) != 1 || commits[0].findings[0].Place != "A:2" {
		t.Fatalf("%+v %v", commits, err)
	}
}

func TestPushedTheCacheIsReadOnlyAsItsOwnSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitdcy", pushedFile)
	cache := pushedLoad(path)
	cache.Checkouts["/work"] = &pushedRead{Against: "x", Commits: map[string][]Finding{"abc": nil}}
	pushedSave(path, cache)
	if got := pushedLoad(path); !reflect.DeepEqual(got, cache) {
		t.Fatalf("%+v", got)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("only the user reads it: %v %v", info, err)
	}
	for _, text := range []string{`{"schema":"gitdcy.pushed-read.v0","checkouts":{"/work":{}}}`, "not json"} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := pushedLoad(path); len(got.Checkouts) != 0 {
			t.Errorf("%s: %+v", text, got)
		}
	}
	if pushedAgainst("base", map[string]bool{"a": true}) == pushedAgainst("base", map[string]bool{"b": true}) ||
		pushedAgainst("base", nil) == pushedAgainst("other", nil) {
		t.Error("another identity or rules file is read again")
	}
}

// hygOld is what hygRead read before `git status --porcelain=v2`: the
// changed files, the upstream and how far behind it.
func hygOld(dir string) (uint64, *string, uint64) {
	var changed, behind uint64
	if out, ok := hygGit(dir, "status", "--porcelain=v1", "--untracked-files=normal"); ok {
		for _, line := range lines(out) {
			if line != "" {
				changed++
			}
		}
	}
	upstream, ok := hygGit(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if !ok || upstream == "" {
		return changed, nil, 0
	}
	behind = hygCount(dir, "rev-list", "--count", "HEAD..@{upstream}")
	return changed, &upstream, behind
}

func TestHygieneOneStatusSaysWhatThreeReadsSaid(t *testing.T) {
	work := pushedRepo(t)
	check := func(state string, settled bool) Hygiene {
		t.Helper()
		var h Hygiene
		ok := hygStatus(work, &h)
		if ok != settled {
			t.Fatalf("%s: settled %v", state, ok)
		}
		if !ok {
			return h
		}
		changed, upstream, behind := hygOld(work)
		if h.Changed != changed || !reflect.DeepEqual(h.Upstream, upstream) || h.Behind != behind {
			t.Errorf("%s: %d %v %d, before %d %v %d", state, h.Changed, h.Upstream, h.Behind, changed, upstream, behind)
		}
		return h
	}
	check("no upstream", true)
	factsSh(t, work, "git branch -q -u origin/$(git symbolic-ref --short HEAD)")
	if h := check("level with its upstream", true); h.Upstream == nil || h.Behind != 0 {
		t.Errorf("%+v", h)
	}
	factsSh(t, work, "git reset -q --hard HEAD~2 && echo changed > README && echo new > D && mkdir E && echo e > E/e")
	if h := check("behind, with changes", true); h.Upstream == nil || h.Behind != 1 || h.Changed != 3 {
		t.Errorf("%+v", h)
	}
	factsSh(t, work, "git update-ref -d refs/remotes/origin/$(git symbolic-ref --short HEAD)")
	if h := check("its upstream gone", true); h.Upstream != nil || h.Changed != 3 {
		t.Errorf("%+v", h)
	}
	factsSh(t, work, "git checkout -q --detach")
	check("detached", true)
	factsSh(t, work, "git push -q origin HEAD:refs/heads/side && git fetch -q origin && git checkout -q --orphan fresh && "+
		"git config branch.fresh.remote origin && git config branch.fresh.merge refs/heads/side")
	check("no commit yet, with an upstream", false)
}

// rulesOpenOld is what rulesOpen read before one question: the top folder,
// the git folder and where git runs hooks from.
func rulesOpenOld(t *testing.T, dir string) (root, gitDir, hooks string) {
	t.Helper()
	out, err := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	gitDir = rulesAbsolute(dir, out)
	root = gitDir
	if top, err := gitOutput(dir, "rev-parse", "--show-toplevel"); err == nil && trim(top) != "" {
		root = trim(top)
	}
	out, err = gitOutput(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	return root, gitDir, rulesAbsolute(root, out)
}

func TestRulesOneQuestionOpensARepositoryAsSeveralDid(t *testing.T) {
	work := pushedRepo(t)
	forge := filepath.Join(filepath.Dir(work), "forge.git")
	linked := filepath.Join(filepath.Dir(work), "linked")
	factsSh(t, work, "mkdir -p sub && git worktree add -q ../linked && git config core.hooksPath .githooks")
	for _, dir := range []string{work, filepath.Join(work, "sub"), linked, forge, filepath.Join(work, ".git")} {
		repo, err := rulesOpen(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		root, gitDir, hooks := rulesOpenOld(t, dir)
		if repo.root != root || repo.gitDir != gitDir {
			t.Errorf("%s: %s %s, before %s %s", dir, repo.root, repo.gitDir, root, gitDir)
		}
		if got, _, _ := rulesHooksElsewhere(repo); got != hooks && !(got == "" && hooks == joinPath(gitDir, "hooks")) {
			t.Errorf("%s: hooks %q, before %q", dir, got, hooks)
		}
		if len(repo.remotes) > 0 && repo.remotes[0].pushURL != "../forge.git" {
			t.Errorf("%s: %+v", dir, repo.remotes)
		}
		// A checkout is opened with one question; a bare repository and a
		// git folder in parts.
		if inWorkTree := dir != forge && !strings.HasSuffix(dir, ".git"); inWorkTree != (repo.hooksDir != "" && repo.fetchHead != "") {
			t.Errorf("%s: %+v", dir, repo)
		}
	}
}
