package main

// `gitdcy carry`: moving work between two checkouts of one repository.

import (
	"fmt"
	"os"
)

func carryCommand() *cliCmd {
	option := func(id, long, value string) *cliArg { return optArg(id, long, value) }
	from := func(help string) *cliArg { return option("from", "from", "NAME").Required().Help(help) }
	operation := func() *cliArg { return option("operation", "operation", "ID").Required() }
	return newCmd("carry").
		AfterHelp(`Examples:
  gitdcy carry send host:code/app --from laptop --operation 1 --tip feature --stash stash@{0} --worktree
  gitdcy carry place --from laptop --operation 1                (in the receiving checkout)
  gitdcy carry send host:code/app --from laptop --operation 2 --head HEAD
  gitdcy carry place --from laptop --operation 2 --fast-forward main
  gitdcy carry aside --as refs/carry-aside/desk-1 --message 'continues on desk'

send pushes into the other checkout under refs/carry/FROM/OPERATION/: each
branch at a --tip (heads/BRANCH, or tips/COMMIT), each --stash (stash/N,
newest first), --head (head) and, with --worktree, the working tree as a
commit on top of HEAD (wip), made in a throwaway index and object folder so
this checkout is not written to. GIT_SSH_COMMAND in the environment reaches
the other side; its hooks do not run.

place makes branches only where the name is free (else carry/FROM/BRANCH,
else the ref is kept), stores each stash entry once (git stash store) and
applies the working tree only when this checkout is clean and at the commit
it was made on (else the branch carry/FROM/wip-OPERATION, else the ref is
kept). --fast-forward BRANCH instead moves that branch, when it is checked
out, to the carried head, only when it is behind and only as a fast-forward.

aside stashes the uncommitted work, untracked files included, keeps it under
the ref given and takes it off the stash list: git stash apply REF brings it
back. Nothing is forced, reset or deleted; the refs/carry/ refs stay as the
record.`).
		SubRequired().
		ElseHelp().
		Sub(
			newCmd("send").
				About("Push this checkout's work into another checkout of the repository").
				Arg(
					posArg("to", "TO").Required().
						Help("The other checkout, the way git push names it (host:path, a URL, a path)"),
					from("This side's name; the work arrives under refs/carry/FROM/"),
					operation().Help("A name for this carry, so repeated carries stay apart"),
					option("head", "head", "COMMIT").Help("A commit for the other side's place --fast-forward"),
					option("tips", "tip", "COMMIT").Append().Help("A local branch tip; repeat"),
					option("stashes", "stash", "COMMIT").Append().Help("A stash entry, newest first; repeat"),
					flagArg("worktree", "worktree").Help("The working tree, uncommitted and untracked files included"),
					option("message", "message", "TEXT").Help("The working-tree commit's message"),
				),
			newCmd("place").
				About("Turn what another checkout sent into branches, stash entries and the working tree").
				Arg(
					from("The sending side's name, as it sent it"),
					operation(),
					option("fast_forward", "fast-forward", "BRANCH").Help("Fast-forward this branch to the carried head instead"),
					option("message", "message", "TEXT").Help("The stash entries' message (default: carried from FROM)"),
				),
			newCmd("aside").
				About("Stash the uncommitted work under a ref of its own, off the stash list").
				Arg(
					option("reference", "as", "REF").Required().Help("The ref that keeps it (refs/...)"),
					option("message", "message", "TEXT").Default("set aside by gitdcy carry"),
				),
		).
		Arg(
			option("repo", "repo", "PATH").Path().Global().Help("The checkout (default: the current folder)"),
			formatArg().Global(),
		)
}

// runCarry runs `gitdcy carry ...`; exit 1 when it could not be done
// (--format json prints the envelope then too).
func runCarry(m *cliMatches) error {
	repo := "."
	if v, ok := m.str("repo"); ok {
		repo = v
	}
	json := isJSON(m)
	sub := m.sub
	given := func(id string) string { v, _ := sub.str(id); return v }
	var failed bool
	switch m.subName {
	case "send":
		request := carrySend{
			to:        given("to"),
			from:      given("from"),
			operation: given("operation"),
			head:      sub.optStr("head"),
			tips:      sub.strs("tips"),
			stashes:   sub.strs("stashes"),
			worktree:  sub.flag("worktree"),
			message:   sub.optStr("message"),
		}
		sent, err := carrySendWork(repo, request)
		failed = carryFinish(json, "carry.send", sent, err, func() []string {
			return []string{"pushed to " + sent.Prefix}
		})
	case "place":
		request := carryPlace{
			from:        given("from"),
			operation:   given("operation"),
			fastForward: sub.optStr("fast_forward"),
			message:     sub.optStr("message"),
		}
		placement, err := carryPlaceWork(repo, request)
		failed = carryFinish(json, "carry.place", placement, err, func() []string {
			var texts []string
			for _, p := range placement.Placed {
				texts = append(texts, p.Text)
			}
			return texts
		})
	default:
		aside, err := carrySetAside(repo, given("reference"), given("message"))
		failed = carryFinish(json, "carry.aside", aside, err, func() []string {
			return []string{fmt.Sprintf("saved as %s; git stash apply %s brings it back", aside.Reference, aside.Reference)}
		})
	}
	if failed {
		os.Exit(1)
	}
	return nil
}

// carryFinish prints the result; true when the command should exit 1.
func carryFinish(json bool, operation string, value any, err error, texts func() []string) bool {
	if json {
		printJSON(operation, value, err, []string{})
		return err != nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gitdcy carry: %s\n", chainText(err))
		return true
	}
	for _, line := range texts() {
		fmt.Println(line)
	}
	return false
}
