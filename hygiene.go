package main

// No dirty dishes: reminders about work left lying around in a checkout, so
// it is picked up before work starts and before a push.
//
// Four kinds of reminder, each with what clears it:
//
//   - a dirty tree (local changes not committed), or a pull not done
//     (commits on the upstream, as of the last fetch);
//   - commits on no remote from before today (they stayed unpushed past a
//     day's end), or, before a push, the ones that push leaves behind;
//   - a branch not merged into the default branch after some days;
//   - an open pull request, read from GitHub through `gh` when it is
//     installed and signed in; otherwise their state is unknown, and the
//     reminder says so.
//
// The reminders are on when the rules file has a [hygiene] table; a
// repository whose remote starts with one of its `skip` prefixes keeps its
// own rules and gets none. Read-only: git runs with hooks and fsmonitor off
// and without optional locks, and nothing is fetched.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hygOutputLimit        = 1024 * 1024
	hygGitTimeout         = 8 * time.Second
	hygDay                = 86400
	hygDefaultBranchDays  = 3
	hygPullRequestsFields = "number,title,url,createdAt,author,repository,isDraft"
)

// HygieneRules is the rules file's [hygiene] table.
type HygieneRules struct {
	// A branch not merged after this many days is a reminder (default 3).
	BranchDays uint64 `json:"branch_days"`
	// Remote URL prefixes (`github.com/example-org/`) of repositories that
	// keep their own rules, such as an employer's: no reminders for them.
	Skip []string `json:"skip,omitempty"`
}

func (*HygieneRules) denyUnknownTOML() {}

func (h *HygieneRules) validate() error {
	if h.BranchDays == 0 {
		return errf("[hygiene] branch_days = 0 would remind of every branch at once; say 1 or more")
	}
	for _, prefix := range h.Skip {
		if trim(prefix) == "" {
			return errf("[hygiene] an empty prefix in skip")
		}
	}
	return nil
}

// skippedBy is the skip prefix one of these remote URLs starts with, if any.
func (h *HygieneRules) skippedBy(urls []string) (string, bool) {
	for _, prefix := range h.Skip {
		for _, url := range urls {
			if hygHasPrefix(url, prefix) {
				return prefix, true
			}
		}
	}
	return "", false
}

// hygHasPrefix reports whether prefix matches the URL or what lies under
// it, ignoring case (hosts and GitHub owners are case-insensitive).
func hygHasPrefix(url, prefix string) bool {
	url = strings.ToLower(normalizeURL(url))
	prefix = strings.TrimRight(strings.ToLower(normalizeURL(prefix)), "/")
	return prefix != "" && (url == prefix || strings.HasPrefix(url, prefix) && strings.HasPrefix(url[len(prefix):], "/"))
}

// Branch is a branch with commits the default branch does not have.
type Branch struct {
	Branch string `json:"branch"`
	// What it is compared with (origin/main).
	Base string `json:"base"`
	// Its commits that Base lacks.
	Commits uint64 `json:"commits"`
	// Days since the oldest of them.
	Days uint64 `json:"days"`
}

// PullRequest is an open pull request.
type PullRequest struct {
	Number uint64 `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author string `json:"author"`
	// Days since it was opened.
	Days  uint64 `json:"days"`
	Draft bool   `json:"draft"`
}

// PullRequests is the open pull requests of a checkout's repository.
type PullRequests struct {
	// known (read from GitHub), unknown (it is on GitHub, and gh could not
	// be asked) or not-asked (not on GitHub).
	State string `json:"state"`
	// Why they are unknown or not asked.
	Detail string `json:"detail,omitempty"`
	// The repository on GitHub (owner/name).
	Repository string        `json:"repository,omitempty"`
	Open       []PullRequest `json:"open"`
}

// Reminder is one reminder: what it is about, in words, and what clears it.
type Reminder struct {
	// dirty, behind, unpushed, branch, pull-request or
	// pull-requests-unknown.
	Kind string `json:"kind"`
	Text string `json:"text"`
	Fix  string `json:"fix"`
}

// Hygiene is a checkout's dishes.
type Hygiene struct {
	// Files with local changes, untracked ones included.
	Changed uint64 `json:"changed"`
	// The upstream (origin/main), when the checked-out branch has one.
	Upstream *string `json:"upstream"`
	// Commits on the upstream that were not pulled, as of the last fetch.
	Behind uint64 `json:"behind"`
	// When the repository last fetched (Unix seconds); null when never.
	FetchedAt *int64 `json:"fetched_at"`
	// Commits on local branches that no remote-tracking branch holds.
	Unpushed uint64 `json:"unpushed"`
	// Of those, the ones made before today (local midnight).
	UnpushedBeforeToday uint64 `json:"unpushed_before_today"`
	// The default branch branches are compared with; null when none is
	// found.
	Base *string `json:"base"`
	// Branches not merged into Base for branch_days or more.
	Branches     []Branch     `json:"branches"`
	PullRequests PullRequests `json:"pull_requests"`
	Reminders    []Reminder   `json:"reminders"`
}

func hygNow() int64 {
	if now := time.Now().Unix(); now > 0 {
		return now
	}
	return 0
}

// hygGit is git's trimmed output, false when it fails or takes too long.
func hygGit(dir string, args ...string) (string, bool) {
	out, err := runBounded(readingGitCommand(dir, args...), hygOutputLimit, hygGitTimeout)
	if err != nil || !out.success() {
		return "", false
	}
	return trim(lossy(out.Stdout)), true
}

func hygCount(dir string, args ...string) uint64 {
	out, ok := hygGit(dir, args...)
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(out, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// hygBaseBranch is the branch others are compared with: origin/HEAD, else
// origin/main or origin/master, else a local main or master.
func hygBaseBranch(dir string) (string, bool) {
	if head, ok := hygGit(dir, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); ok && head != "" {
		return head, true
	}
	for _, name := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, ok := hygGit(dir, "rev-parse", "--verify", "-q", name+"^{commit}"); ok {
			return name, true
		}
	}
	return "", false
}

// hygUnmerged is the local branches with commits base lacks, whose oldest
// such commit is days or more old.
func hygUnmerged(dir, base string, days uint64, at int64) []Branch {
	short := base
	if _, name, ok := rsplitOnce(base, "/"); ok {
		short = name
	}
	list, ok := hygGit(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if !ok {
		return nil
	}
	var found []Branch
	for _, branch := range lines(list) {
		branch = trim(branch)
		if branch == "" || branch == short || branch == base {
			continue
		}
		out, ok := hygGit(dir, "log", "--no-merges", "--format=%ct", base+"..refs/heads/"+branch)
		if !ok {
			continue
		}
		var times []int64
		for _, t := range lines(out) {
			if n, err := strconv.ParseInt(trim(t), 10, 64); err == nil {
				times = append(times, n)
			}
		}
		if len(times) == 0 {
			continue
		}
		oldest := times[0]
		for _, t := range times {
			oldest = min(oldest, t)
		}
		age := uint64(max(at-oldest, 0) / hygDay)
		if age >= days {
			found = append(found, Branch{Branch: branch, Base: base, Commits: uint64(len(times)), Days: age})
		}
	}
	return found
}

// githubRepository is the repository's place on GitHub (owner/name), from
// its remotes.
func githubRepository(urls []string) (string, bool) {
	for _, url := range urls {
		rest, ok := strings.CutPrefix(normalizeURL(url), "github.com/")
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0] + "/" + parts[1], true
		}
	}
	return "", false
}

func hygDivEuclid(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// hygDaysFromCivil is the days since 1970-01-01 of a civil date.
func hygDaysFromCivil(year, month, day int64) int64 {
	if month <= 2 {
		year--
	}
	era := hygDivEuclid(year, 400)
	yoe := year - era*400
	mp := (month + 9) % 12
	doy := (153*mp+2)/5 + day - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// hygUnixTime reads `2026-11-01T10:00:00Z` as Unix seconds (UTC; the time
// zone is ignored, a day is all that is counted).
func hygUnixTime(stamp string) (int64, bool) {
	date, clock, ok := splitOnce(stamp, "T")
	if !ok {
		return 0, false
	}
	number := func(parts []string, i int) (int64, bool) {
		if i >= len(parts) {
			return 0, false
		}
		n, err := strconv.ParseInt(parts[i], 10, 64)
		return n, err == nil
	}
	dateParts := strings.Split(date, "-")
	year, ok1 := number(dateParts, 0)
	month, ok2 := number(dateParts, 1)
	day, ok3 := number(dateParts, 2)
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	clock = strings.TrimRight(clock, "Z")
	if i := strings.IndexAny(clock, "+-"); i >= 0 {
		clock = clock[:i]
	}
	var timeParts []string
	for _, part := range strings.Split(clock, ":") {
		whole, _, _ := strings.Cut(part, ".")
		timeParts = append(timeParts, whole)
	}
	hour, ok1 := number(timeParts, 0)
	minute, ok2 := number(timeParts, 1)
	if !ok1 || !ok2 {
		return 0, false
	}
	second, _ := number(timeParts, 2)
	return hygDaysFromCivil(year, month, day)*hygDay + hour*3600 + minute*60 + second, true
}

// hygGitHub is the open pull requests on GitHub, by repository
// (owner/name, lowercase), or why they could not be read.
type hygGitHub struct {
	found map[string][]PullRequest
	// Why they could not be read; "" when they were.
	why string
}

// hygUnknown is nothing asked: every repository on GitHub is unknown, for
// this reason.
func hygUnknown(why string) *hygGitHub { return &hygGitHub{why: why} }

// failure is why GitHub's pull requests could not be read, if they could
// not.
func (g *hygGitHub) failure() (string, bool) { return g.why, g.found == nil }

// hygAsk asks for the open pull requests of these owners' repositories, or
// of these repositories (owner/name), each question stopped after timeout;
// program is gh. One gh search per owner, or for one repository.
func hygAsk(program string, owners, repositories []string, timeout time.Duration) *hygGitHub {
	found := map[string][]PullRequest{}
	var asks [][2]string
	for _, owner := range owners {
		asks = append(asks, [2]string{"--owner", owner})
	}
	for _, repo := range repositories {
		asks = append(asks, [2]string{"--repo", repo})
	}
	for _, ask := range asks {
		cmd := exec.Command(program, "search", "prs", ask[0], ask[1], "--state", "open",
			"--json", hygPullRequestsFields, "--limit", "200")
		setEnv(cmd, "GH_PROMPT_DISABLED", "1")
		setEnv(cmd, "GH_NO_UPDATE_NOTIFIER", "1")
		out, err := runBounded(cmd, hygOutputLimit, timeout)
		if err != nil {
			return hygUnknown(program + " is not installed here")
		}
		if !out.success() {
			said := fmt.Sprintf("no answer within %d s", int64(timeout/time.Second))
			for _, line := range lines(lossy(out.Stderr)) {
				if line = trim(line); line != "" {
					said = line
					break
				}
			}
			return hygUnknown(program + " could not be asked: " + said)
		}
		list, ok := hygParsePullRequests(out.Stdout)
		if !ok {
			return hygUnknown(program + " answered something unexpected")
		}
		at := hygNow()
		for _, pr := range list {
			if !pr.hasRepository {
				continue
			}
			repo := strings.ToLower(pr.repository)
			opened, ok := hygUnixTime(pr.createdAt)
			if !ok {
				opened = at
			}
			known := false
			for _, p := range found[repo] {
				known = known || p.Number == pr.number
			}
			if known {
				continue
			}
			found[repo] = append(found[repo], PullRequest{
				Number: pr.number, Title: pr.title, URL: pr.url, Author: pr.author,
				Days: uint64(max(at-opened, 0) / hygDay), Draft: pr.isDraft,
			})
		}
	}
	for _, list := range found {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	}
	return &hygGitHub{found: found}
}

// hygFound is one pull request as gh answered it.
type hygFound struct {
	number                uint64
	title, url, createdAt string
	isDraft               bool
	author, repository    string
	hasRepository         bool
}

// hygParsePullRequests reads gh's answer as serde read it: field names as
// written, `number` required, the others default when missing, null only
// for author and repository.
func hygParsePullRequests(data []byte) ([]hygFound, bool) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil || items == nil {
		return nil, false
	}
	isNull := func(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
	value := func(fields map[string]json.RawMessage, name string, v any) bool {
		raw, ok := fields[name]
		if !ok {
			return true
		}
		return !isNull(raw) && json.Unmarshal(raw, v) == nil
	}
	object := func(raw json.RawMessage) (map[string]json.RawMessage, bool) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, false
		}
		return fields, true
	}
	var list []hygFound
	for _, item := range items {
		fields, ok := object(item)
		if !ok {
			return nil, false
		}
		var pr hygFound
		if _, has := fields["number"]; !has || !value(fields, "number", &pr.number) ||
			!value(fields, "title", &pr.title) || !value(fields, "url", &pr.url) ||
			!value(fields, "createdAt", &pr.createdAt) || !value(fields, "isDraft", &pr.isDraft) {
			return nil, false
		}
		if raw, has := fields["author"]; has && !isNull(raw) {
			login, ok := object(raw)
			if !ok || !value(login, "login", &pr.author) {
				return nil, false
			}
		}
		if raw, has := fields["repository"]; has && !isNull(raw) {
			named, ok := object(raw)
			if !ok || !value(named, "nameWithOwner", &pr.repository) {
				return nil, false
			}
			pr.hasRepository = true
		}
		list = append(list, pr)
	}
	return list, true
}

// of is the pull requests of repository (owner/name); onGitHub false for a
// repository that is not on GitHub.
func (g *hygGitHub) of(repository string, onGitHub bool) PullRequests {
	if !onGitHub {
		return PullRequests{State: "not-asked", Detail: "not on GitHub: only GitHub's pull requests are read, through gh"}
	}
	if g.found == nil {
		return PullRequests{State: "unknown", Detail: g.why, Repository: repository}
	}
	return PullRequests{State: "known", Repository: repository, Open: append([]PullRequest{}, g.found[strings.ToLower(repository)]...)}
}

// hygStatus reads the changed files, the upstream and how far behind it
// the checkout is from one `git status`; false when that did not settle
// them (git failed, or the branch has no commit yet and an upstream, which
// status does not say exists), and nothing is set.
func hygStatus(dir string, h *Hygiene) bool {
	out, ok := hygGit(dir, "status", "--porcelain=v2", "--branch", "--ahead-behind", "--untracked-files=normal")
	if !ok {
		return false
	}
	var changed, behind uint64
	var upstream string
	initial, counted := false, false
	for _, line := range lines(out) {
		header, ok := strings.CutPrefix(line, "# ")
		if !ok {
			if line != "" {
				changed++
			}
			continue
		}
		switch name, value, _ := strings.Cut(header, " "); name {
		case "branch.oid":
			initial = value == "(initial)"
		case "branch.upstream":
			upstream = value
		case "branch.ab":
			// "+AHEAD -BEHIND"; without it the upstream is gone.
			_, minus, _ := strings.Cut(value, " -")
			n, err := strconv.ParseUint(minus, 10, 64)
			if err != nil {
				return false
			}
			behind, counted = n, true
		}
	}
	if initial && upstream != "" {
		return false
	}
	h.Changed = changed
	if counted && upstream != "" {
		h.Upstream, h.Behind = &upstream, behind
	}
	return true
}

// hygRead is the checkout's dishes. urls are its remote URLs; pushing the
// commits a push sends when beforePush (before a push: what it leaves
// behind is reminded of, whatever its age), else a scan (commits from
// before today).
func hygRead(repo *rulesRepo, rules HygieneRules, urls []string, github *hygGitHub, pushing []string, beforePush bool) Hygiene {
	at := hygNow()
	dir := repo.root
	shown := dir
	h := Hygiene{}
	if !hygStatus(dir, &h) {
		if out, ok := hygGit(dir, "status", "--porcelain=v1", "--untracked-files=normal"); ok {
			for _, line := range lines(out) {
				if line != "" {
					h.Changed++
				}
			}
		}
		if upstream, ok := hygGit(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); ok && upstream != "" {
			h.Upstream = &upstream
			h.Behind = hygCount(dir, "rev-list", "--count", "HEAD..@{upstream}")
		}
	}
	fetchHead, ok := repo.fetchHead, repo.fetchHead != ""
	if !ok {
		fetchHead, ok = hygGit(dir, "rev-parse", "--path-format=absolute", "--git-path", "FETCH_HEAD")
	}
	if ok {
		if info, err := os.Stat(fetchHead); err == nil && info.ModTime().Unix() >= 0 {
			fetched := info.ModTime().Unix()
			h.FetchedAt = &fetched
		}
	}
	// rulesOpen read the remotes.
	hasRemote := len(repo.remotes) > 0
	onNoRemote := append([]string{"rev-list", "--count", "--branches", "--not", "--remotes"}, pushing...)
	if hasRemote {
		h.Unpushed = hygCount(dir, onNoRemote...)
	}
	if h.Unpushed > 0 {
		before := append([]string{"rev-list", "--count", "--before=midnight", "--branches", "--not", "--remotes"}, pushing...)
		h.UnpushedBeforeToday = hygCount(dir, before...)
	}
	if base, ok := hygBaseBranch(dir); ok {
		h.Base = &base
		h.Branches = hygUnmerged(dir, base, rules.BranchDays, at)
	}
	h.PullRequests = github.of(githubRepository(urls))

	if h.Changed > 0 {
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "dirty",
			Text: fmt.Sprintf("%d file(s) changed and not committed", h.Changed),
			Fix:  fmt.Sprintf("commit or stash them (git -C %s status)", shown),
		})
	}
	if h.Behind > 0 {
		upstream := "the upstream"
		if h.Upstream != nil {
			upstream = *h.Upstream
		}
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "behind",
			Text: fmt.Sprintf("%d commit(s) on %s not pulled (as of the last fetch)", h.Behind, upstream),
			Fix:  fmt.Sprintf("git -C %s pull --ff-only", shown),
		})
	}
	left, text := h.UnpushedBeforeToday, "%d commit(s) from before today are on no remote"
	if beforePush {
		left, text = h.Unpushed, "%d commit(s) on other branches are on no remote"
	}
	if left > 0 {
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "unpushed",
			Text: fmt.Sprintf(text, left),
			Fix:  fmt.Sprintf("push them (git -C %s log --branches --not --remotes --oneline lists them)", shown),
		})
	}
	for _, b := range h.Branches {
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "branch",
			Text: fmt.Sprintf("branch %s not merged into %s for %d day(s) (%d commit(s))", b.Branch, b.Base, b.Days, b.Commits),
			Fix:  fmt.Sprintf("merge it, or delete it when it was merged another way (git -C %s branch -d %s)", shown, b.Branch),
		})
	}
	for _, pr := range h.PullRequests.Open {
		draft := ""
		if pr.Draft {
			draft = " (draft)"
		}
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "pull-request",
			Text: fmt.Sprintf("pull request #%d open for %d day(s)%s: %s", pr.Number, pr.Days, draft, pr.Title),
			Fix:  "merge or close it: " + pr.URL,
		})
	}
	if h.PullRequests.State == "unknown" {
		h.Reminders = append(h.Reminders, Reminder{
			Kind: "pull-requests-unknown",
			Text: fmt.Sprintf("open pull requests unknown (%s)", h.PullRequests.Detail),
			Fix:  "gh auth login, then ask again",
		})
	}
	return h
}
