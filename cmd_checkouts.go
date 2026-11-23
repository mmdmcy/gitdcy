package main

// `gitdcy checkouts`: checkouts against their upstream, and the safe
// fast-forward.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkoutsCommandReport is the JSON data of `gitdcy checkouts`.
type checkoutsCommandReport struct {
	Checkouts []checkoutsCheckout `json:"checkouts"`
}

func checkoutsCommand() *cliCmd {
	paths := func() *cliArg { return posArg("paths", "PATH").Values(1, -1) }
	flag := func(id, long string) *cliArg { return flagArg(id, long).Global() }
	return newCmd("checkouts").
		AfterHelp(`Examples:
  gitdcy checkouts                         every repository in the workspace
  gitdcy checkouts ~/code/app ~/code/lib   these checkouts
  gitdcy checkouts --no-fetch --format json
  gitdcy checkouts --no-fetch --facts --format json /abs/repo
  gitdcy checkouts update ~/code/app       fast-forward it if that is safe
  gitdcy checkouts update --rules          the checkout holding the rules file in use

States: current, behind, ahead, diverged, changed (local changes only),
missing, no-upstream, error. update fast-forwards only a checkout that is
behind with no local changes and no local commits; it never makes a merge
commit, rebases, forces or touches local changes. A leading ~/ is your home,
also on Windows. Reading a checkout's state never runs its hooks or
fsmonitor; only the fetch and update's fast-forward run git as configured.

--facts adds what a tool moving the checkout to another device needs, as
`+"`facts`"+` in the JSON: the full head id, stash ids, commits on no remote,
notable ignored files by name, fingerprints of the local changes and the
working tree's id (computed without writing to the repository).

--rules takes the checkout that holds the rules file in use (as `+"`gitdcy rules`"+`
finds it: rules_file in local.toml, else rules.toml in the config folder)
in place of PATH, so a daily job keeps the push check's rules current
wherever they live; with no rules file, or one in no git checkout, it says
so and succeeds.`).
		Sub(newCmd("update").
			About("Fast-forward the checkouts that are behind and have no local changes or commits").
			Arg(paths().Help("Checkouts to update (default: every repository in the workspace)"))).
		Arg(
			paths().Help("Checkouts to read (default: every repository in the workspace)"),
			flag("no_fetch", "no-fetch").Help("Do not fetch first: show what the last fetch knew"),
			flag("facts", "facts").Help("Also report what moving each checkout needs (slower: hashes local changes)"),
			flag("rules", "rules").Help("The checkout holding the rules file in use, in place of PATH"),
			formatArg().Global(),
		)
}

// runCheckouts runs `gitdcy checkouts ...`; exit 1 only when the command
// itself failed (--format json prints the envelope then too).
func runCheckouts(m *cliMatches) error {
	operation, update, paths := "checkouts", false, m.strs("paths")
	if m.sub != nil {
		operation, update, paths = "checkouts.update", true, m.sub.strs("paths")
	}
	options := checkoutsOptions{fetch: !m.flag("no_fetch"), update: update, facts: m.flag("facts")}
	var report checkoutsCommandReport
	if m.flag("rules") {
		if len(paths) > 0 {
			return errf("--rules takes no PATH: it is the checkout holding the rules file in use")
		}
		dir, none := rulesCheckout()
		if dir == "" {
			report.Checkouts = []checkoutsCheckout{}
			if isJSON(m) {
				printJSON(operation, report, nil, []string{none})
			} else {
				fmt.Println(none)
			}
			return nil
		}
		paths = []string{dir}
	}
	paths, err := givenOrWorkspace(paths)
	if err == nil {
		report.Checkouts = checkoutsRead(paths, options)
	}
	if isJSON(m) {
		var warnings []string
		if err == nil {
			warnings = checkoutsWarnings(report.Checkouts)
		}
		printJSON(operation, report, err, warnings)
		if err != nil {
			os.Exit(1)
		}
		return nil
	}
	if err != nil {
		return err
	}
	checkoutsPrintHuman(report.Checkouts)
	return nil
}

// rulesCheckout is the checkout that holds the rules file in use (a link
// followed), or why there is none: no rules file, or one in no git checkout.
func rulesCheckout() (string, string) {
	file, err := rulesFilePath(nil)
	if err != nil {
		return "", fmt.Sprintf("no rules file: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		return "", fmt.Sprintf("no rules file at %s: nothing to bring current", file)
	}
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	top, err := gitOutput(filepath.Dir(file), "rev-parse", "--show-toplevel")
	if top = strings.TrimSpace(top); err != nil || top == "" {
		return "", fmt.Sprintf("the rules file %s is in no git checkout: nothing to bring current", file)
	}
	return filepath.FromSlash(top), ""
}

// checkoutsWarnings is what could not be read as asked.
func checkoutsWarnings(list []checkoutsCheckout) []string {
	warnings := []string{}
	for _, c := range list {
		if c.State == "missing" || c.State == "error" || c.FetchError != "" {
			warnings = append(warnings, c.Path+": "+c.Detail)
		}
	}
	return warnings
}

func checkoutsPrintHuman(list []checkoutsCheckout) {
	fmt.Println(checkoutsSummary(list))
	width := 0
	for _, c := range list {
		width = max(width, len(c.Path))
	}
	width = min(width, 48)
	for _, c := range list {
		fmt.Printf("  %-*s  %-11s %s\n", width, c.Path, c.State, c.Detail)
		if c.Upstream != "" {
			fmt.Printf("  %-*s  %-11s newest: %s\n", width, "", "", c.Upstream)
		}
		if c.Facts != nil {
			fmt.Printf("  %-*s  %-11s %s\n", width, "", "", checkoutsFactsLine(c.Facts))
		}
	}
}

// checkoutsFactsLine is the facts in one line for a person; the JSON has
// them all.
func checkoutsFactsLine(f *factsReport) string {
	head := "no commit yet"
	if f.Head != nil {
		head = (*f.Head)[:min(len(*f.Head), 12)]
	}
	parts := []string{"head " + head}
	if f.StatusError != nil {
		parts = append(parts, *f.StatusError)
	} else if f.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed (%d untracked)", f.Changed, f.Untracked))
	}
	if len(f.Stashes) > 0 {
		parts = append(parts, fmt.Sprintf("%d stash(es)", len(f.Stashes)))
	}
	if f.UnpushedCommits > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) on no remote", f.UnpushedCommits))
	}
	if len(f.IgnoredState) > 0 {
		parts = append(parts, "ignored: "+strings.Join(f.IgnoredState, ", "))
	}
	if f.Tree != nil {
		parts = append(parts, "tree "+(*f.Tree)[:min(len(*f.Tree), 12)])
	}
	return strings.Join(parts, "; ")
}
