package main

// How two commits of one repository relate (`gitdcy relate`): the same
// commit, one ahead of or behind the other, diverged from a common
// ancestor, or unrelated, with the commits only on each side counted and,
// when asked, listed.
//
// Read-only: git runs with hooks and fsmonitor off, without optional locks
// and never prompts; nothing is fetched, so a commit the repository does not
// hold is `missing`, never looked up elsewhere.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// relateWorkers is how many repositories are read at the same time.
const relateWorkers = 8

// relateRelation is one answer: A against B.
type relateRelation struct {
	// Repo is the repository, as given (`~/code/app` stays `~/code/app`).
	Repo string `json:"repo"`
	// A and B are the two commits or refs, as given.
	A string `json:"a"`
	B string `json:"b"`
	// ACommit and BCommit are their full commit ids; null where it is not in
	// the repository.
	ACommit *string `json:"a_commit"`
	BCommit *string `json:"b_commit"`
	// Relation is equal, ahead (A holds all of B and more), behind (B holds
	// all of A and more), diverged (each has commits the other lacks, from a
	// common ancestor), unrelated (no common ancestor) or missing (no
	// repository there, or A or B is not in it).
	Relation string `json:"relation"`
	// Ahead is the commits in A that B lacks.
	Ahead uint64 `json:"ahead"`
	// Behind is the commits in B that A lacks.
	Behind uint64 `json:"behind"`
	Detail string `json:"detail"`
	// OnlyA, with a list asked for: up to that many commits only in A,
	// newest first.
	OnlyA *[]relateCommit `json:"only_a,omitempty"`
	// OnlyB is the same for B.
	OnlyB *[]relateCommit `json:"only_b,omitempty"`
}

type relateCommit struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
}

// relateRequest is one question: the repository, then A and B.
type relateRequest struct{ repo, a, b string }

// relateParseRequests reads `PATH<TAB>A<TAB>B` lines, as `gitdcy relate
// --stdin` reads them; empty lines are skipped.
func relateParseRequests(text string) ([]relateRequest, error) {
	requests := []relateRequest{}
	for number, line := range lines(text) {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, errf("line %d: expected PATH, A and B separated by tabs", number+1)
		}
		requests = append(requests, relateRequest{fields[0], fields[1], fields[2]})
	}
	return requests, nil
}

// relateAll answers every request, in the order given, with up to eight
// repositories read at the same time; list, when not nil, is how many
// commits only on each side to list.
func relateAll(requests []relateRequest, list *uint64) []relateRelation {
	answers := make([]relateRelation, len(requests))
	if len(requests) == 0 {
		return answers
	}
	size := (len(requests) + relateWorkers - 1) / relateWorkers
	var wg sync.WaitGroup
	for start := 0; start < len(requests); start += size {
		end := min(start+size, len(requests))
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				r := requests[i]
				answers[i] = relateOne(r.repo, r.a, r.b, list)
			}
		}(start, end)
	}
	wg.Wait()
	return answers
}

// relateOne is A against B in the repository at repo; with list, up to that
// many commits only on each side.
func relateOne(repo, a, b string, list *uint64) relateRelation {
	found := relateRelation{Repo: repo, A: a, B: b}
	dir := expandHome(repo)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return relateMissing(found, "no folder at that path")
	}
	found.ACommit = relateCommitID(dir, a)
	found.BCommit = relateCommitID(dir, b)
	if found.ACommit == nil || found.BCommit == nil {
		var absent []string
		if found.ACommit == nil {
			absent = append(absent, a)
		}
		if found.BCommit == nil {
			absent = append(absent, b)
		}
		return relateMissing(found, strings.Join(absent, " and ")+": no such commit in this repository")
	}
	aID, bID := *found.ACommit, *found.BCommit
	if aID == bID {
		found.Relation = "equal"
		found.Detail = "the same commit"
		if list != nil {
			found.OnlyA, found.OnlyB = &[]relateCommit{}, &[]relateCommit{}
		}
		return found
	}
	counts, err := relateGit(dir, "rev-list", "--left-right", "--count", aID+"..."+bID)
	if err != nil {
		return relateMissing(found, chainText(err))
	}
	numbers := strings.Fields(counts)
	var ahead, behind uint64
	ok := len(numbers) >= 2
	if ok {
		var okA, okB bool
		ahead, okA = checkoutsParseUint(numbers[0], 64)
		behind, okB = checkoutsParseUint(numbers[1], 64)
		ok = okA && okB
	}
	if !ok {
		return relateMissing(found, "git rev-list answered "+rustDebug(counts))
	}
	found.Ahead, found.Behind = ahead, behind
	switch {
	case behind == 0:
		found.Relation, found.Detail = "ahead", fmt.Sprintf("%s is %d commit(s) ahead of %s", a, ahead, b)
	case ahead == 0:
		found.Relation, found.Detail = "behind", fmt.Sprintf("%s is %d commit(s) behind %s", a, behind, b)
	default:
		if _, err := relateGit(dir, "merge-base", aID, bID); err == nil {
			found.Relation = "diverged"
			found.Detail = fmt.Sprintf("diverged: %d commit(s) only in %s, %d only in %s", ahead, a, behind, b)
		} else {
			found.Relation = "unrelated"
			found.Detail = fmt.Sprintf("no common history: %d commit(s) only in %s, %d only in %s", ahead, a, behind, b)
		}
	}
	if list != nil {
		onlyA := relateOnlyIn(dir, aID, bID, *list)
		onlyB := relateOnlyIn(dir, bID, aID, *list)
		found.OnlyA, found.OnlyB = &onlyA, &onlyB
	}
	return found
}

func relateMissing(found relateRelation, detail string) relateRelation {
	found.Relation = "missing"
	found.Detail = detail
	return found
}

// relateCommitID is the full id of the commit rev names here, if it is one.
func relateCommitID(dir, rev string) *string {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return nil
	}
	id, err := relateGit(dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil || id == "" {
		return nil
	}
	return &id
}

// relateOnlyIn is up to limit commits in tip that other lacks, newest
// first.
func relateOnlyIn(dir, tip, other string, limit uint64) []relateCommit {
	commits := []relateCommit{}
	if limit == 0 {
		return commits
	}
	listing, _ := relateGit(dir, "log", "--no-decorate", "--no-show-signature", "--format=%H %s",
		"-n", strconv.FormatUint(limit, 10), tip, "--not", other, "--")
	for _, line := range lines(listing) {
		id, subject, ok := splitOnce(line, " ")
		if !ok {
			id, subject = line, ""
		}
		if id != "" {
			commits = append(commits, relateCommit{ID: id, Subject: subject})
		}
	}
	return commits
}

// relateGit is git's trimmed answer, read with hooks and fsmonitor off.
func relateGit(dir string, args ...string) (string, error) {
	out, err := runOutput(readingGitCommand(dir, args...))
	if err != nil {
		return "", err
	}
	if !out.success() {
		return "", errf("%s", commandError("git", out))
	}
	return trim(lossy(out.Stdout)), nil
}
