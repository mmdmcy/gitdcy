package main

// Checkouts against their upstream: fetch each one, say whether it is up to
// date, behind, ahead, diverged, has local changes or is missing, and
// fast-forward the ones that can be. Nothing else ever happens to a
// checkout: no merge commit, no rebase, no force, and local changes or local
// commits are never touched.
//
// A path is used as given; a leading `~/` is the user's home (also on
// Windows, where the shell does not expand it), and the report shows the
// path the way it was given, so a caller can match its own list. Any
// checkout can be named, so reading its state runs git with hooks and
// fsmonitor off; only the fetch (skipped with `--no-fetch`) and `update`'s
// fast-forward run git as the user configured it.

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	checkoutsFetchTimeout     = 60 * time.Second
	checkoutsFetchOutputLimit = 256 * 1024
	// checkoutsWorkers is how many checkouts are read at the same time;
	// fetches are mostly waiting on the network.
	checkoutsWorkers = 8
)

// checkoutsOptions is what to do besides reading.
type checkoutsOptions struct {
	// fetch each checkout's upstream first (off: as of the last fetch).
	fetch bool
	// update fast-forwards a checkout that is behind and has no local
	// changes or commits.
	update bool
	// facts also reads what a tool moving the checkout needs; slower, since
	// it hashes local changes.
	facts bool
}

// checkoutsChangedFile is a file with local changes and git's two-letter
// status for it, as in `git status --porcelain` (" M", "A ", "??", "UU").
type checkoutsChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// checkoutsCheckout is one checkout's report.
type checkoutsCheckout struct {
	// Path as given (`~/code/app` stays `~/code/app`).
	Path string `json:"path"`
	// State is current, behind, ahead, diverged, changed (local changes
	// only), missing, no-upstream or error.
	State string `json:"state"`
	// Branch is empty when no branch is checked out.
	Branch string `json:"branch"`
	// TrackingBranch is the branch it follows (origin/main); empty when none.
	TrackingBranch string `json:"tracking_branch"`
	// Remote is that branch's remote (or origin), without user, password or
	// port.
	Remote  string `json:"remote"`
	Behind  uint32 `json:"behind"`
	Ahead   uint32 `json:"ahead"`
	Changed uint32 `json:"changed"`
	// Files with local changes, untracked ones included.
	Files []checkoutsChangedFile `json:"files"`
	// Head is the checkout's last commit: `hash date subject`.
	Head string `json:"head"`
	// HeadTime is its commit time in Unix seconds; 0 before the first commit.
	HeadTime int64 `json:"head_time"`
	// Upstream is the upstream's last commit, when behind.
	Upstream string `json:"upstream"`
	// Updated: fast-forwarded by this run.
	Updated bool `json:"updated"`
	// FetchError is why the fetch failed; the rest is then as of the last
	// fetch.
	FetchError string `json:"fetch_error"`
	Detail     string `json:"detail"`
	// Facts, with --facts, for every checkout that is not missing.
	Facts *factsReport `json:"facts,omitempty"`
}

// checkoutsRead reads every path, in the order given, with up to eight at a
// time.
func checkoutsRead(paths []string, options checkoutsOptions) []checkoutsCheckout {
	report := make([]checkoutsCheckout, len(paths))
	if len(paths) == 0 {
		return report
	}
	size := (len(paths) + checkoutsWorkers - 1) / checkoutsWorkers
	var wg sync.WaitGroup
	for start := 0; start < len(paths); start += size {
		end := min(start+size, len(paths))
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				report[i] = checkoutsOne(paths[i], options)
			}
		}(start, end)
	}
	wg.Wait()
	return report
}

// checkoutsOne is one checkout, with its facts last so they show any
// fast-forward.
func checkoutsOne(given string, options checkoutsOptions) checkoutsCheckout {
	dir := expandHome(given)
	found := checkoutsStateOf(given, dir, options)
	if options.facts && found.State != "missing" {
		f := factsRead(dir)
		found.Facts = &f
	}
	return found
}

func checkoutsStateOf(given, dir string, options checkoutsOptions) checkoutsCheckout {
	found := checkoutsCheckout{Path: given}
	if _, err := os.Stat(joinPath(dir, ".git")); err != nil {
		found.State = "missing"
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			found.Detail = "not a git checkout at that path"
		} else {
			found.Detail = "no folder at that path"
		}
		return found
	}
	if err := checkoutsReadInto(dir, &found, options.fetch); err != nil {
		found.State = "error"
		found.Detail = checkoutsFirstLine(chainText(err))
		return found
	}
	if options.update && found.State == "behind" && found.Changed == 0 {
		behind := found.Behind
		if _, err := gitOutput(dir, "merge", "--ff-only", "--quiet", "@{u}"); err == nil {
			fetchError := found.FetchError
			found.FetchError = ""
			reread := checkoutsReadInto(dir, &found, false)
			found.FetchError = fetchError
			found.Updated = true
			if reread != nil {
				found.State = "error"
				found.Detail = fmt.Sprintf("fast-forwarded %d commit(s), then could not read it: %s",
					behind, checkoutsFirstLine(chainText(reread)))
			} else if found.State == "current" {
				found.Detail = fmt.Sprintf("fast-forwarded %d commit(s); up to date", behind)
			}
		} else {
			found.Detail = fmt.Sprintf("%d commit(s) behind; the fast-forward failed: %s",
				behind, checkoutsFirstLine(chainText(err)))
		}
	}
	if found.FetchError != "" {
		found.Detail = fmt.Sprintf("%s; the fetch failed (%s), so this is as of the last fetch",
			found.Detail, found.FetchError)
	}
	return found
}

// checkoutsReadInto reads branch, upstream, changes and commits into found.
func checkoutsReadInto(dir string, found *checkoutsCheckout, fetch bool) error {
	found.Behind, found.Ahead = 0, 0
	branch, err := checkoutsReadText(dir, "branch", "--show-current")
	if err != nil {
		return err
	}
	found.Branch = trim(branch)
	found.TrackingBranch = ""
	if tracking, err := checkoutsReadText(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		found.TrackingBranch = trim(tracking)
	}
	if fetch && found.TrackingBranch != "" {
		if err := checkoutsFetchUpstream(dir); err != nil {
			found.FetchError = checkoutsFirstLine(chainText(err))
		}
	}
	found.Remote = checkoutsRemoteOf(dir, found.Branch)
	status, err := checkoutsReadGit(dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return err
	}
	found.Files = checkoutsParseStatus(status)
	found.Changed = uint32(min(len(found.Files), math.MaxUint32))
	found.HeadTime, found.Head = checkoutsLastCommit(dir, "HEAD")
	found.Upstream = ""
	if found.TrackingBranch == "" {
		found.State = "no-upstream"
		if found.Branch == "" {
			found.Detail = "not on a branch (detached HEAD)"
		} else {
			found.Detail = "the branch follows no remote branch"
		}
		return nil
	}
	counts, err := checkoutsReadText(dir, "rev-list", "--left-right", "--count", "@{u}...HEAD")
	if err != nil {
		return err
	}
	numbers := strings.Fields(counts)
	number := func(i int) uint32 {
		if i >= len(numbers) {
			return 0
		}
		n, _ := checkoutsParseUint(numbers[i], 32)
		return uint32(n)
	}
	found.Behind, found.Ahead = number(0), number(1)
	if found.Behind > 0 {
		_, found.Upstream = checkoutsLastCommit(dir, "@{u}")
	}
	found.State, found.Detail = checkoutsVerdict(found.Behind, found.Ahead, found.Changed)
	return nil
}

// checkoutsVerdict is the state and its sentence, from the counts.
func checkoutsVerdict(behind, ahead, changed uint32) (string, string) {
	switch {
	case behind == 0 && ahead == 0 && changed > 0:
		return "changed", fmt.Sprintf("%d local change(s), not committed", changed)
	case behind == 0 && ahead == 0:
		return "current", "up to date"
	case ahead == 0 && changed > 0:
		return "behind", fmt.Sprintf("%d commit(s) behind, but %d local change(s): commit or stash them first", behind, changed)
	case ahead == 0:
		return "behind", fmt.Sprintf("%d commit(s) behind; checkouts update brings it up", behind)
	case behind == 0:
		return "ahead", fmt.Sprintf("%d local commit(s) not pushed", ahead)
	}
	return "diverged", fmt.Sprintf("%d behind and %d ahead: merge or rebase by hand", behind, ahead)
}

// checkoutsSummary is `N checkout(s): N up to date, N behind, N need
// attention`.
func checkoutsSummary(list []checkoutsCheckout) string {
	count := func(state string) int {
		n := 0
		for _, c := range list {
			if c.State == state {
				n++
			}
		}
		return n
	}
	return fmt.Sprintf("%d checkout(s): %d up to date, %d behind, %d need attention",
		len(list), count("current"), count("behind"), len(list)-count("current")-count("behind"))
}

// checkoutsFetchUpstream is `git fetch` from the branch's upstream remote,
// never asking for a password.
func checkoutsFetchUpstream(dir string) error {
	cmd := gitCommand(dir, "fetch", "--quiet")
	setEnv(cmd, "GIT_TERMINAL_PROMPT", "0")
	setEnv(cmd, "GCM_INTERACTIVE", "never")
	started := time.Now()
	out, err := runBounded(cmd, checkoutsFetchOutputLimit, checkoutsFetchTimeout)
	if err != nil {
		return err
	}
	if out.success() {
		return nil
	}
	if time.Since(started) >= checkoutsFetchTimeout {
		return errf("git fetch took longer than %d s and was stopped", int(checkoutsFetchTimeout.Seconds()))
	}
	return errf("%s", commandError("git fetch", out))
}

// checkoutsRemoteOf is the remote the branch follows, else origin, as
// host/path.
func checkoutsRemoteOf(dir, branch string) string {
	name := "origin"
	if branch != "" {
		if configured, err := checkoutsReadText(dir, "config", "--get", "branch."+branch+".remote"); err == nil {
			if configured = trim(configured); configured != "" && configured != "." {
				name = configured
			}
		}
	}
	url, err := checkoutsReadText(dir, "remote", "get-url", name)
	if err != nil {
		return ""
	}
	return normalizeURL(trim(url))
}

// checkoutsLastCommit is (unix seconds, "hash date subject") of a commit;
// empty before the first one.
func checkoutsLastCommit(dir, rev string) (int64, string) {
	text, _ := checkoutsReadText(dir, "log", "-1", "--format=%ct %h %cs %s", rev, "--")
	line := strings.TrimRight(text, "\r\n")
	when, rest, ok := splitOnce(line, " ")
	if !ok {
		return 0, ""
	}
	seconds, err := strconv.ParseInt(when, 10, 64)
	if err != nil {
		seconds = 0
	}
	return seconds, rest
}

// checkoutsReadGit is what `git ARGS` printed, read with hooks and fsmonitor
// off.
func checkoutsReadGit(dir string, args ...string) ([]byte, error) {
	out, err := gitCommandOutput(readingGitCommand(dir, args...))
	if err != nil {
		return nil, wrap(err, "run git")
	}
	if !out.success() {
		return nil, errf("%s", commandError("git", out))
	}
	return out.Stdout, nil
}

func checkoutsReadText(dir string, args ...string) (string, error) {
	out, err := checkoutsReadGit(dir, args...)
	if err != nil {
		return "", err
	}
	return lossy(out), nil
}

// checkoutsParseStatus reads `git status --porcelain=v1 -z`: `XY path`, and
// after a rename or copy the old path as a field of its own.
func checkoutsParseStatus(output []byte) []checkoutsChangedFile {
	files := []checkoutsChangedFile{}
	fields := bytes.Split(output, []byte{0})
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if len(field) < 4 {
			continue
		}
		status := lossy(field[:2])
		if strings.ContainsAny(status, "RC") {
			i++
		}
		files = append(files, checkoutsChangedFile{Path: lossy(field[3:]), Status: status})
	}
	return files
}

// checkoutsFirstLine is the first line of text that is not blank, trimmed.
func checkoutsFirstLine(text string) string {
	for _, line := range lines(text) {
		if line = trim(line); line != "" {
			return line
		}
	}
	return ""
}

// checkoutsParseUint is Rust's str::parse for an unsigned number of that
// many bits: an optional "+", then decimal digits only.
func checkoutsParseUint(s string, bits int) (uint64, bool) {
	digits := strings.TrimPrefix(s, "+")
	if digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(digits, 10, bits)
	return n, err == nil
}
