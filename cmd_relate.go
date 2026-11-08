package main

// `gitdcy relate`: how two commits of a repository relate.

import (
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// relateCommandReport is the JSON data of `gitdcy relate`.
type relateCommandReport struct {
	Relations []relateRelation `json:"relations"`
}

func relateCommand() *cliCmd {
	return newCmd("relate").
		AfterHelp(`Examples:
  gitdcy relate main origin/main            this repository's main against its upstream
  gitdcy relate --repo ~/code/app 1a2b3c HEAD --format json
  printf '%s\t%s\t%s\n' ~/code/app 1a2b3c HEAD | gitdcy relate --stdin --format json

Relations: equal, ahead (A holds all of B and more), behind (B holds all of A
and more), diverged (each has commits the other lacks), unrelated (no common
history) and missing (no repository there, or A or B is not in it). ahead
and behind count the commits only in A and only in B. Read-only: nothing is
fetched, and git runs without the repository's hooks or fsmonitor.`).
		Arg(
			posArg("commits", "COMMIT").Values(0, 2).Help("A, then B: commits or refs"),
			optArg("repo", "repo", "PATH").Help("The repository (default: the current folder)"),
			flagArg("stdin", "stdin").ConflictsWith("commits", "repo").
				Help("Read PATH<TAB>A<TAB>B lines from standard input instead, answered in order"),
			optArg("list", "list", "N").Usize().Help("Also list up to N commits only in A and only in B, newest first"),
			formatArg(),
		)
}

// runRelate runs `gitdcy relate ...`; exit 1 only when the command itself
// failed (--format json prints the envelope then too).
func runRelate(m *cliMatches) error {
	var list *uint64
	if v, ok := m.str("list"); ok {
		n, _ := parseUsize(v)
		list = &n
	}
	var report relateCommandReport
	requests, err := relateRequests(m)
	if err == nil {
		report.Relations = relateAll(requests, list)
	}
	if isJSON(m) {
		var warnings []string
		if err == nil {
			warnings = []string{}
			for _, r := range report.Relations {
				if r.Relation == "missing" {
					warnings = append(warnings, r.Repo+": "+r.Detail)
				}
			}
		}
		printJSON("relate", report, err, warnings)
		if err != nil {
			os.Exit(1)
		}
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range report.Relations {
		fmt.Printf("%s: %s %s\n", r.Repo, r.Relation, r.Detail)
		for _, side := range []struct {
			name    string
			commits *[]relateCommit
		}{{r.A, r.OnlyA}, {r.B, r.OnlyB}} {
			if side.commits == nil {
				continue
			}
			for _, c := range *side.commits {
				fmt.Printf("  only in %s: %s %s\n", side.name, c.ID[:min(len(c.ID), 12)], c.Subject)
			}
		}
	}
	return nil
}

// relateRequests is the questions: standard input's lines with --stdin,
// else A and B in --repo (default: the current folder).
func relateRequests(m *cliMatches) ([]relateRequest, error) {
	if m.flag("stdin") {
		data, err := io.ReadAll(os.Stdin)
		if err == nil && !utf8.Valid(data) {
			err = errf("stream did not contain valid UTF-8")
		}
		if err != nil {
			return nil, wrap(err, "reading the questions from standard input")
		}
		return relateParseRequests(string(data))
	}
	commits := m.strs("commits")
	if len(commits) != 2 {
		return nil, errf("name two commits or refs (A and B), or pass --stdin")
	}
	repo := "."
	if v, ok := m.str("repo"); ok {
		repo = v
	}
	return []relateRequest{{repo, commits[0], commits[1]}}, nil
}
