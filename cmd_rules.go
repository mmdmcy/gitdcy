package main

// `gitdcy rules`: which rule applies to a repository, installing it into
// the checkout, the check the pre-push hook runs, and the scan of every
// checkout.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

func rulesCommand() *cliCmd {
	path := func(id, value string) *cliArg { return optArg(id, id, value).Path().Global() }
	return newCmd("rules").
		AfterHelp(`Examples:
  gitdcy rules                      which rule applies to this repository, and why
  gitdcy rules --format json        the same, for scripts
  gitdcy rules install              hook and a copy of the rule into .git/
  gitdcy rules check                what a push of this branch would be refused for
  gitdcy rules scan                 every checkout in the workspace against the rules
  gitdcy rules scan ~/code/app --since 2026-10-24

The rules file is the one given with --rules, else rules_file in GitDCY's
local.toml, else rules.toml in GitDCY's config folder. With a [hygiene] table
in it, scan and check also remind of work left lying around: a dirty tree or a
pull not done, commits on no remote from before today, a branch not merged
after branch_days (3), and open pull requests (GitHub's, through gh).`).
		Sub(
			newCmd("install").About("Write a pre-push hook and a copy of this repository's rule into its git folder, so the rule holds on a machine without the rules file"),
			newCmd("check").
				About("Refuse a push the rule does not allow, and read the commits it sends for personal data (what the pre-push hook runs)").
				Arg(
					flagArg("stdin", "stdin").Help("Read the refs being pushed from standard input, as git gives them to a pre-push hook"),
					posArg("remote", "REMOTE").Help("The remote's name (default: origin)"),
					posArg("url", "URL").Help("The remote's URL (default: its push URL)"),
				),
			newCmd("scan").
				About("Every checkout against the rules, read only: its rule, the remotes the rule forbids, personal data in what already reached a remote, and the reminders about work left lying around").
				Arg(
					posArg("paths", "PATH").Values(1, -1).Help("Checkouts to read (default: --repo, else every repository in the workspace)"),
					optArg("since", "since", "DATE").Help(`Read what reached a remote since then (git's --since: 2026-10-24, "3 days ago"; default 7 days)`),
				),
		).
		Arg(
			path("rules", "FILE").Help("The rules file"),
			path("repo", "PATH").Help("The repository (default: the current folder)"),
			formatArg().Global(),
		)
}

// runRules runs `gitdcy rules ...`; the exit code is 1 when a push is
// refused or the command failed (--format json prints the envelope in both
// cases).
func runRules(m *cliMatches) error {
	rulesPath, pathErr := rulesFilePath(m.optStr("rules"))
	given := m.optStr("repo")
	repo := "."
	if given != nil {
		repo = *given
	}
	gitdcy, _ := os.Executable()
	asJSON := isJSON(m)
	var failed bool
	var err error
	switch m.subName {
	case "":
		var report *RulesReport
		if err = pathErr; err == nil {
			report, err = rulesReport(rulesPath, repo, gitdcy)
		}
		failed, err = rulesFinish("rules", report, err, asJSON, func() []string { return report.Warnings },
			func() { rulesPrintReport(report) }, func() bool { return false })
	case "install":
		var report *InstallReport
		if err = pathErr; err == nil {
			report, err = rulesInstall(rulesPath, repo, gitdcy)
		}
		failed, err = rulesFinish("rules.install", report, err, asJSON, func() []string { return nil },
			func() { rulesPrintInstall(report) }, func() bool { return false })
	case "check":
		sub := m.sub
		hook := sub.flag("stdin")
		request, requestErr := rulesCheckRequest(hook, sub.optStr("remote"), sub.optStr("url"))
		var report *CheckReport
		if err = pathErr; err == nil {
			if err = requestErr; err == nil {
				report, err = rulesCheck(rulesPath, repo, request)
			}
		}
		failed, err = rulesFinish("rules.check", report, err, asJSON, func() []string { return nil },
			func() { rulesPrintCheck(report, hook) }, func() bool { return report.Refused })
	case "scan":
		paths := m.sub.strs("paths")
		if len(paths) == 0 && given != nil {
			paths = []string{*given}
		}
		var report *ScanReport
		if err = pathErr; err == nil {
			if paths, err = givenOrWorkspace(paths); err == nil {
				report, err = rulesScan(rulesPath, paths, m.sub.optStr("since"), gitdcy, "gh")
			}
		}
		failed, err = rulesFinish("rules.scan", report, err, asJSON, func() []string { return rulesScanWarnings(report) },
			func() { rulesPrintScan(report) }, func() bool { return false })
	}
	if err != nil {
		return err
	}
	if failed {
		os.Exit(1)
	}
	return nil
}

// rulesCheckRequest is what check was asked; with stdin, the refs being
// pushed are read from standard input.
func rulesCheckRequest(stdin bool, remote, url *string) (CheckRequest, error) {
	request := CheckRequest{Remote: remote, URL: url, HasRefs: stdin}
	if !stdin {
		return request, nil
	}
	const context = "reading the refs being pushed from standard input"
	data, err := io.ReadAll(os.Stdin)
	if err != nil && !errors.Is(err, syscall.EBADF) {
		return request, wrap(err, context)
	}
	if !utf8.Valid(data) {
		return request, wrap(errors.New("stream did not contain valid UTF-8"), context)
	}
	refs, err := parsePushedRefs(string(data))
	request.Refs = refs
	return request, err
}

// rulesFinish prints the result: the envelope with --format json, else the
// report in words, or the error for main to print. It returns whether the
// command exits 1.
func rulesFinish[T any](operation string, report *T, err error, asJSON bool, warnings func() []string, human func(), refused func() bool) (bool, error) {
	if asJSON {
		if err != nil {
			printJSON(operation, nil, err, nil)
			return true, nil
		}
		printJSON(operation, report, nil, warnings())
		return refused(), nil
	}
	if err != nil {
		return false, err
	}
	human()
	return refused(), nil
}

func rulesSourceLabel(source *RuleSource) string {
	switch {
	case source == nil:
		return ""
	case *source == sourceRulesFile:
		return "the rules file"
	}
	return "the copy in the git folder"
}

func rulesPrintReport(r *RulesReport) {
	fmt.Println(r.Repository)
	switch {
	case r.Rule != nil:
		fmt.Printf("  rule: %s (from %s; matched by %s)\n", r.Rule.Name, rulesSourceLabel(r.Source), strings.Join(r.MatchedBy, ", "))
		push := "anywhere"
		if r.Push != nil {
			push = *r.Push
		}
		fmt.Printf("  push: %s\n", push)
		if len(r.Rule.Protected) > 0 {
			fmt.Printf("  only through a pull request: %s\n", strings.Join(r.Rule.Protected, ", "))
		}
		if note := trim(r.Rule.Note); note != "" {
			fmt.Printf("  note: %s\n", note)
		}
	case len(r.Conflicts) > 0:
		fmt.Printf("  rules that all match: %s\n", strings.Join(r.MatchedBy, "; "))
	default:
		fmt.Println("  no rule: pushes may go anywhere; the personal-data check still runs")
		if len(r.FirstCommits) > 0 {
			fmt.Printf("  first commit: %s (first_commit in a [[repo]] of %s gives it a rule)\n", r.FirstCommits[0], r.RulesFile)
		}
	}
	for _, remote := range r.Remotes {
		url, allowed := "(no URL)", "NOT allowed"
		if remote.URL != nil {
			url = *remote.URL
		}
		if remote.Allowed {
			allowed = "allowed"
		}
		fmt.Printf("  %s -> %s: %s\n", remote.Name, url, allowed)
	}
	if r.Rule != nil {
		hook := "not installed"
		if r.Hook.Installed && r.Hook.Current && r.Hook.Runs {
			hook = "installed"
		} else if r.Hook.Installed {
			hook = "installed, see below"
		}
		copy := "not installed"
		if r.Copy.Present {
			copy = "out of date"
			if r.Copy.Current != nil && *r.Copy.Current {
				copy = "current"
			}
		}
		fmt.Printf("  hook: %s; copy of the rule: %s\n", hook, copy)
	}
	words, allowed := "", ""
	if r.PersonalWords > 0 {
		words = fmt.Sprintf(" and %d personal word(s)", r.PersonalWords)
	}
	if r.AllowedAddresses > 0 {
		allowed = fmt.Sprintf("; %d address(es) allowed", r.AllowedAddresses)
	}
	fmt.Printf("  personal-data check: private mail addresses%s%s\n", words, allowed)
	for _, warning := range r.Warnings {
		fmt.Printf("  ! %s\n", warning)
	}
}

func rulesWrittenText(written bool) string {
	if written {
		return "written"
	}
	return "already current"
}

func rulesPrintInstall(r *InstallReport) {
	fmt.Printf("%s: rule %s\n", r.Repository, r.Rule)
	fmt.Printf("  copy %s: %s\n", r.Copy, rulesWrittenText(r.CopyWritten))
	fmt.Printf("  hook %s: %s\n", r.Hook, rulesWrittenText(r.HookWritten))
	for _, kept := range r.Kept {
		fmt.Printf("  kept the previous file as %s\n", kept)
	}
	if r.Chained != nil {
		fmt.Printf("  the hook that was there (%s) runs after the check\n", *r.Chained)
	}
}

// rulesPrintCheck: in the hook a clean push prints only its reminders; by
// hand, it says so. The reminders go to standard error and never refuse a
// push.
func rulesPrintCheck(r *CheckReport, hook bool) {
	if !r.Refused {
		if !hook {
			rule := ""
			if r.Rule != nil {
				rule = ", rule " + *r.Rule
			}
			fmt.Printf("%s: %d commit(s) in %s%s: clean\n", r.Repository, r.Commits, r.Range, rule)
		}
	} else {
		var b strings.Builder
		target := "(no remote)"
		if r.URL != nil {
			target = *r.URL
		} else if r.Remote != nil {
			target = *r.Remote
		}
		fmt.Fprintf(&b, "gitdcy rules: push from %s to %s REFUSED\n", r.Repository, target)
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "  commit %s %s: %s (%s)\n", f.Commit, f.Place, f.Kind.label(), f.Shown)
		}
		for _, reason := range r.Reasons {
			fmt.Fprintf(&b, "  %s\n", reason)
		}
		fmt.Fprint(os.Stderr, b.String())
	}
	if len(r.Reminders) > 0 {
		texts := make([]string, len(r.Reminders))
		for i, reminder := range r.Reminders {
			texts[i] = reminder.Text
		}
		fmt.Fprintf(os.Stderr, "gitdcy rules: left lying around in %s: %s\n", r.Repository, strings.Join(texts, "; "))
	}
}

// rulesScanWarnings is what could not be read as asked.
func rulesScanWarnings(r *ScanReport) []string {
	var warnings []string
	for _, c := range r.Checkouts {
		if c.State != "ok" || c.Detail != "" {
			warnings = append(warnings, c.Path+": "+c.Detail)
		}
	}
	if !r.RulesFileFound {
		warnings = append(warnings, fmt.Sprintf("no rules file at %s: no repository has a rule, and there are no reminders", r.RulesFile))
	}
	return warnings
}

// rulesDishes is a checkout's reminders, without the unknown pull requests
// the scan's header names once.
func rulesDishes(c CheckoutScan) []Reminder {
	if c.Hygiene == nil {
		return nil
	}
	var list []Reminder
	for _, r := range c.Hygiene.Reminders {
		if r.Kind != "pull-requests-unknown" {
			list = append(list, r)
		}
	}
	return list
}

// rulesNotable reports whether a checkout has anything to say.
func rulesNotable(c CheckoutScan) bool {
	return c.State != "ok" || c.Rule != nil || len(c.Conflicts) > 0 || len(c.Forbidden) > 0 ||
		len(c.PushedFindings) > 0 || len(rulesDishes(c)) > 0
}

// rulesPrintScan prints the checkouts with something to say, then a count
// of the rest.
func rulesPrintScan(r *ScanReport) {
	pushed := 0
	for _, c := range r.Checkouts {
		pushed += c.PushedCommits
	}
	reminders := "; no reminders (the rules file has no [hygiene])"
	if r.Hygiene != nil {
		reminders = ""
	}
	fmt.Printf("%d checkout(s) against %s; %d commit(s) that reached a remote since %s read for personal data%s\n",
		len(r.Checkouts), r.RulesFile, pushed, r.Since, reminders)
	// Said once here, not for every checkout on GitHub.
	if r.PullRequests == "unknown" {
		fmt.Printf("  open pull requests are unknown: %s (gh auth login, then scan again)\n", r.PullRequestsDetail)
	}
	quiet := 0
	for _, c := range r.Checkouts {
		if !rulesNotable(c) {
			quiet++
			continue
		}
		rule := ""
		if c.Rule != nil {
			rule = ": rule " + *c.Rule
			if c.Push != nil {
				rule += " (push " + *c.Push + ")"
			}
		}
		fmt.Printf("  %s%s\n", c.Path, rule)
		if c.State != "ok" {
			fmt.Printf("    %s: %s\n", c.State, c.Detail)
			continue
		}
		if len(c.Conflicts) > 0 {
			fmt.Printf("    ! rules %s all match it: its pushes are refused until one is left\n", strings.Join(c.Conflicts, ", "))
		}
		for _, remote := range c.Forbidden {
			url := "(no URL)"
			if remote.URL != nil {
				url = *remote.URL
			}
			fmt.Printf("    ! remote %s -> %s: its rule does not allow it, a push there is refused\n", remote.Name, url)
		}
		if c.Hook != nil && !c.Hook.Runs {
			where := "elsewhere"
			if c.Hook.HooksPath != nil {
				where = *c.Hook.HooksPath
			}
			fmt.Printf("    ! git runs hooks from %s and the pre-push there does not run the check\n", where)
		}
		for _, f := range c.PushedFindings {
			fmt.Printf("    ! pushed commit %s %s: %s (%s)\n", f.Commit, f.Place, f.Kind.label(), f.Shown)
		}
		for _, reminder := range rulesDishes(c) {
			fmt.Printf("    - %s: %s\n", reminder.Text, reminder.Fix)
		}
	}
	if quiet > 0 {
		fmt.Printf("  %d more checkout(s): no rule and nothing left lying around\n", quiet)
	}
}
