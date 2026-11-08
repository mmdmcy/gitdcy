package main

// Moving work between two checkouts of one repository (`gitdcy carry`),
// never overwriting or deleting anything on either side:
//
//   - send pushes this checkout's work into the other checkout under
//     refs/carry/FROM/OPERATION/: local branch tips (heads/BRANCH, or
//     tips/COMMIT where no branch points at one), stash entries (stash/N),
//     one commit (head), and the working tree as a commit on top of HEAD
//     (wip), made in a throwaway index and object folder so this checkout
//     is never written to;
//   - place turns what arrived into work in the receiving checkout: a
//     branch only where its name is free (else carry/FROM/BRANCH, else the
//     ref is kept), each stash entry once through `git stash store`, and the
//     working tree applied only when the checkout is clean and at the commit
//     the work was made on (else the branch carry/FROM/wip-OPERATION, else
//     the ref is kept). `--fast-forward BRANCH` instead moves that branch,
//     when it is checked out, to the commit `send --head` carried, only when
//     it is behind and only as a fast-forward;
//   - aside sets this checkout's uncommitted work aside once it continues
//     elsewhere: `git stash push --include-untracked`, kept under a ref of
//     its own and taken off the stash list, so `git stash apply REF` brings
//     it back.
//
// The refs under refs/carry/ stay as the record of what arrived. Git runs
// with hooks and fsmonitor off; nothing is forced, reset or deleted.

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	// carryReceivePack is the receiving side's git: no hooks of the
	// receiving checkout run.
	carryReceivePack = "git -c core.hooksPath=/dev/null -c core.fsmonitor=false receive-pack"
	// Who the working-tree commit is from where git has no identity
	// configured.
	carryFallbackName  = "gitdcy"
	carryFallbackEmail = "carry@gitdcy.invalid"
)

// carrySend is what send carries.
type carrySend struct {
	// to is the other checkout, the way `git push` names it (host:path, a
	// URL or a path); GIT_SSH_COMMAND in the environment reaches it.
	to        string
	from      string
	operation string
	// head is a commit for the other side's --fast-forward.
	head *string
	// tips are local branch tips.
	tips []string
	// stashes are stash entries, newest first (`git stash list` order).
	stashes []string
	// worktree: the working tree, uncommitted and untracked files included.
	worktree bool
	// message is the working-tree commit's message.
	message *string
}

type carrySent struct {
	Prefix string `json:"prefix"`
	// Pushed is COMMIT:REF for every ref pushed.
	Pushed []string `json:"pushed"`
}

// carryPlace is what place does.
type carryPlace struct {
	from      string
	operation string
	// fastForward moves this branch to the carried head instead.
	fastForward *string
	// message is the stash entries' message.
	message *string
}

// carryPlaced is one thing place did or left.
type carryPlaced struct {
	// Action is branch, stash, applied, kept, fast-forwarded or left.
	Action string `json:"action"`
	// Name is the branch, stash commit or ref.
	Name string `json:"name"`
	// Text is the line a person reads.
	Text string `json:"text"`
}

type carryPlacement struct {
	Prefix string        `json:"prefix"`
	Placed []carryPlaced `json:"placed"`
}

type carryAside struct {
	// Reference is the ref that keeps the work.
	Reference string `json:"ref"`
	Commit    string `json:"commit"`
}

// carryPrefix is refs/carry/FROM/OPERATION, once both are plain names.
func carryPrefix(from, operation string) (string, error) {
	for _, check := range [][2]string{{"--from", from}, {"--operation", operation}} {
		what, value := check[0], check[1]
		plain := value != "" &&
			!strings.HasPrefix(value, ".") && !strings.HasPrefix(value, "-") &&
			!strings.HasSuffix(value, ".lock") &&
			!strings.Contains(value, "..")
		for i := 0; plain && i < len(value); i++ {
			c := value[i]
			plain = isASCIIAlnum(rune(c)) || c == '.' || c == '_' || c == '-'
		}
		if !plain {
			return "", errf("%s %s is not a plain name (letters, digits, . _ -)", what, rustDebug(value))
		}
	}
	return "refs/carry/" + from + "/" + operation, nil
}

// carrySendWork pushes this checkout's work into the other one under the
// prefix.
func carrySendWork(dir string, request carrySend) (carrySent, error) {
	prefix, err := carryPrefix(request.from, request.operation)
	if err != nil {
		return carrySent{}, err
	}
	if request.to == "" || strings.HasPrefix(request.to, "-") {
		return carrySent{}, errf("say where to send it: the other checkout as git push names it")
	}
	if dir, err = carryTop(dir); err != nil {
		return carrySent{}, err
	}
	pushed := []string{}
	for _, tip := range request.tips {
		tip, err := carryCommit(dir, tip)
		if err != nil {
			return carrySent{}, err
		}
		branches, err := relateGit(dir, "for-each-ref", "--points-at", tip, "--format=%(refname:lstrip=2)", "refs/heads")
		if err != nil {
			return carrySent{}, err
		}
		before := len(pushed)
		for _, branch := range lines(branches) {
			if branch != "" {
				pushed = append(pushed, tip+":"+prefix+"/heads/"+branch)
			}
		}
		if len(pushed) == before {
			pushed = append(pushed, tip+":"+prefix+"/tips/"+tip)
		}
	}
	for index, stash := range request.stashes {
		commit, err := carryCommit(dir, stash)
		if err != nil {
			return carrySent{}, err
		}
		pushed = append(pushed, fmt.Sprintf("%s:%s/stash/%d", commit, prefix, index))
	}
	if request.head != nil {
		commit, err := carryCommit(dir, *request.head)
		if err != nil {
			return carrySent{}, err
		}
		pushed = append(pushed, commit+":"+prefix+"/head")
	}
	if len(pushed) == 0 && !request.worktree {
		return carrySent{}, errf("nothing to carry: name a --head, --tip or --stash, or --worktree")
	}
	var environment []string
	if request.worktree {
		scratch, ok := factsScratchFolder()
		if !ok {
			return carrySent{}, errf("could not make a private temporary folder")
		}
		defer os.RemoveAll(scratch)
		if environment, err = carryScratchEnvironment(scratch, dir); err != nil {
			return carrySent{}, err
		}
		message := fmt.Sprintf("wip(%s): uncommitted work carried by gitdcy", request.from)
		if request.message != nil {
			message = *request.message
		}
		wip, err := carrySnapshot(dir, environment, message)
		if err != nil {
			return carrySent{}, err
		}
		pushed = append(pushed, wip+":"+prefix+"/wip")
	}
	args := append([]string{"push", "--no-verify", "--quiet", "--receive-pack=" + carryReceivePack, request.to}, pushed...)
	cmd := readingGitCommand(dir, args...)
	for i := 0; i+1 < len(environment); i += 2 {
		setEnv(cmd, environment[i], environment[i+1])
	}
	out, err := runOutput(cmd)
	if err != nil {
		return carrySent{}, wrap(err, "run git push")
	}
	if !out.success() {
		return carrySent{}, errf("%s", commandError("git push", out))
	}
	return carrySent{Prefix: prefix, Pushed: pushed}, nil
}

// carryPlaceWork places what arrived under the prefix, or fast-forwards the
// branch asked.
func carryPlaceWork(dir string, request carryPlace) (carryPlacement, error) {
	prefix, err := carryPrefix(request.from, request.operation)
	if err != nil {
		return carryPlacement{}, err
	}
	if dir, err = carryTop(dir); err != nil {
		return carryPlacement{}, err
	}
	var placed []carryPlaced
	if request.fastForward != nil {
		one, err := carryFastForward(dir, prefix, *request.fastForward)
		if err != nil {
			return carryPlacement{}, err
		}
		placed = []carryPlaced{one}
	} else if placed, err = carryPlaceArrived(dir, prefix, request); err != nil {
		return carryPlacement{}, err
	}
	return carryPlacement{Prefix: prefix, Placed: placed}, nil
}

// carrySetAside sets the uncommitted work aside under reference, off the
// stash list.
func carrySetAside(dir, reference, message string) (carryAside, error) {
	dir, err := carryTop(dir)
	if err != nil {
		return carryAside{}, err
	}
	if !strings.HasPrefix(reference, "refs/") || carryFails(dir, "check-ref-format", reference) ||
		strings.HasPrefix(reference, "refs/heads/") || reference == "refs/stash" {
		return carryAside{}, errf("%s is not a ref of its own (refs/..., not a branch or refs/stash)", rustDebug(reference))
	}
	stash := func() (string, bool) {
		v, err := relateGit(dir, "rev-parse", "-q", "--verify", "refs/stash")
		return v, err == nil
	}
	before, hadBefore := stash()
	if _, err := relateGit(dir, "stash", "push", "--include-untracked", "-q", "-m", message); err != nil {
		return carryAside{}, err
	}
	commit, ok := stash()
	if !ok || (hadBefore && commit == before) {
		return carryAside{}, errf("no local changes to set aside")
	}
	// Kept first; it leaves the stash list only once the ref holds it.
	if _, err := relateGit(dir, "update-ref", "-m", message, reference, commit, ""); err != nil {
		return carryAside{}, wrapf(err, "%s could not keep it; it stays as stash@{0}", reference)
	}
	if now, ok := stash(); ok && now == commit {
		if _, err := relateGit(dir, "stash", "drop", "-q", "stash@{0}"); err != nil {
			return carryAside{}, err
		}
	}
	return carryAside{Reference: reference, Commit: commit}, nil
}

// carryPlaceArrived makes branches where the name is free, stores each stash
// entry once, and applies the working tree only where the checkout is clean
// at the commit it was made on.
func carryPlaceArrived(dir, prefix string, request carryPlace) ([]carryPlaced, error) {
	from := request.from
	placed := []carryPlaced{}
	heads, err := relateGit(dir, "for-each-ref", "--format=%(refname)", prefix+"/heads/")
	if err != nil {
		return nil, err
	}
	for _, reference := range lines(heads) {
		if reference == "" {
			continue
		}
		branch := reference[len(prefix)+len("/heads/"):]
		commit, err := relateGit(dir, "rev-parse", reference)
		if err != nil {
			return nil, err
		}
		carried := "carry/" + from + "/" + branch
		if carryCreate(dir, "refs/heads/"+branch, commit) {
			placed = append(placed, carryLine("branch", branch, "branch "+branch))
		} else if there, err := relateGit(dir, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil && there == commit {
			// Already there under its own name.
			continue
		} else if carryCreate(dir, "refs/heads/"+carried, commit) {
			placed = append(placed, carryLine("branch", carried, "branch "+carried))
		} else {
			placed = append(placed, carryLine("kept", reference, "kept "+reference))
		}
	}
	listing, err := relateGit(dir, "for-each-ref", "--format=%(refname:lstrip=-1)", prefix+"/stash/")
	if err != nil {
		return nil, err
	}
	var entries []uint64
	for _, index := range lines(listing) {
		if n, ok := checkoutsParseUint(index, 64); ok {
			entries = append(entries, n)
		}
	}
	// Oldest first, so the newest ends on top as it was.
	sort.Slice(entries, func(i, j int) bool { return entries[i] > entries[j] })
	message := "carried from " + from
	if request.message != nil {
		message = *request.message
	}
	for _, index := range entries {
		reference := fmt.Sprintf("%s/stash/%d", prefix, index)
		commit, err := relateGit(dir, "rev-parse", reference)
		if err != nil {
			return nil, err
		}
		listed, _ := relateGit(dir, "stash", "list", "--format=%H")
		if contains(lines(listed), commit) {
			continue
		}
		if _, err := relateGit(dir, "stash", "store", "-m", message, commit); err == nil {
			placed = append(placed, carryLine("stash", commit, "stash "+commit))
		} else {
			placed = append(placed, carryLine("kept", reference, "kept "+reference))
		}
	}
	wipRef := prefix + "/wip"
	if wip, err := relateGit(dir, "rev-parse", "-q", "--verify", wipRef+"^{commit}"); err == nil {
		madeOn, madeErr := relateGit(dir, "rev-parse", "-q", "--verify", wip+"^")
		here, hereErr := relateGit(dir, "rev-parse", "-q", "--verify", "HEAD")
		sameCommit := (madeErr == nil) == (hereErr == nil) && (madeErr != nil || madeOn == here)
		status, statusErr := relateGit(dir, "status", "--porcelain")
		clean := statusErr == nil && status == ""
		branch := "carry/" + from + "/wip-" + request.operation
		if sameCommit && clean && carryApply(dir, wip) {
			placed = append(placed, carryLine("applied", "", "applied the working tree"))
		} else if carryCreate(dir, "refs/heads/"+branch, wip) {
			placed = append(placed, carryLine("branch", branch, "branch "+branch+" (successor checkout differs)"))
		} else {
			placed = append(placed, carryLine("kept", wipRef, "kept "+wipRef))
		}
	}
	return placed, nil
}

// carryFastForward moves the branch, when checked out, to the carried head
// when it is behind it.
func carryFastForward(dir, prefix, branch string) (carryPlaced, error) {
	carried := prefix + "/head"
	head, err := relateGit(dir, "rev-parse", "-q", "--verify", carried+"^{commit}")
	if err != nil {
		return carryPlaced{}, errf("nothing was carried as %s", carried)
	}
	left := func(text string) carryPlaced { return carryLine("left", branch, text) }
	on, _ := relateGit(dir, "symbolic-ref", "-q", "--short", "HEAD")
	if on != branch {
		return left("not on " + branch + "; left as is"), nil
	}
	switch relateOne(dir, head, "HEAD", nil).Relation {
	case "equal", "behind":
		return left("already ahead; left as is"), nil
	case "ahead":
		if _, err := relateGit(dir, "merge", "--ff-only", "--quiet", head); err == nil {
			return carryLine("fast-forwarded", branch, "fast-forwarded to "+head[:8]), nil
		}
		return left("fast-forward refused (local changes in the way); " + carried + " keeps the commit"), nil
	}
	return left("diverged; left as is, " + carried + " keeps the device commit"), nil
}

func carryLine(action, name, text string) carryPlaced {
	return carryPlaced{Action: action, Name: name, Text: text}
}

// carryCreate creates reference at commit only when it does not exist yet.
func carryCreate(dir, reference, commit string) bool {
	_, err := relateGit(dir, "update-ref", reference, commit, "")
	return err == nil
}

// carryFails reports whether `git ARGS` fails.
func carryFails(dir string, args ...string) bool {
	_, err := relateGit(dir, args...)
	return err != nil
}

// carryApply is `git diff --binary HEAD WIP | git apply`: the working tree
// only, all or nothing.
func carryApply(dir, wip string) bool {
	diff, err := runOutput(readingGitCommand(dir, "diff", "--binary", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", wip))
	if err != nil || !diff.success() {
		return false
	}
	cmd := readingGitCommand(dir, "apply", "--whitespace=nowarn")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	_, writeErr := stdin.Write(diff.Stdout)
	stdin.Close()
	return cmd.Wait() == nil && writeErr == nil
}

// carryTop is the checkout's top folder.
func carryTop(dir string) (string, error) {
	top, err := relateGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", wrapf(err, "%s is not a git checkout", dir)
	}
	return top, nil
}

// carryCommit is the full id of the commit rev names.
func carryCommit(dir, rev string) (string, error) {
	if strings.HasPrefix(rev, "-") {
		return "", errf("%s is not a commit", rustDebug(rev))
	}
	id, err := relateGit(dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || id == "" {
		return "", errf("%s is not a commit here", rev)
	}
	return id, nil
}

// carryScratchEnvironment is git's environment (name, value pairs) for
// writing into the scratch folder only, reading the checkout's own objects
// alongside.
func carryScratchEnvironment(scratch, dir string) ([]string, error) {
	index, err := relateGit(dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	objects, err := relateGit(dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	return factsScratchEnvironment(scratch, index, objects)
}

// carrySnapshot is the working tree as a commit on top of HEAD (none before
// the first commit), written into the scratch only.
func carrySnapshot(dir string, environment []string, message string) (string, error) {
	run := func(identity []string, args ...string) (string, error) {
		cmd := readingGitCommand(dir, args...)
		for _, env := range [][]string{environment, identity} {
			for i := 0; i+1 < len(env); i += 2 {
				setEnv(cmd, env[i], env[i+1])
			}
		}
		out, err := runOutput(cmd)
		if err != nil {
			return "", err
		}
		if !out.success() {
			return "", errf("%s", commandError("git", out))
		}
		return trim(lossy(out.Stdout)), nil
	}
	if _, err := run(nil, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := run(nil, "write-tree")
	if err != nil {
		return "", err
	}
	var identity []string
	if _, err := run(nil, "var", "GIT_AUTHOR_IDENT"); err != nil {
		identity = []string{
			"GIT_AUTHOR_NAME", carryFallbackName,
			"GIT_AUTHOR_EMAIL", carryFallbackEmail,
			"GIT_COMMITTER_NAME", carryFallbackName,
			"GIT_COMMITTER_EMAIL", carryFallbackEmail,
		}
	}
	if _, err := run(nil, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		return run(identity, "commit-tree", tree, "-p", "HEAD", "-m", message)
	}
	return run(identity, "commit-tree", tree, "-m", message)
}
