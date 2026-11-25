package main

// `gitdcy rules scan`: every checkout on the device against the rules, read
// only. Per checkout: which rule applies and where it lets the repository
// go, the remotes that rule forbids (a self-hosted-only repository with a
// public remote), whether git runs the push check there, personal data in
// what already reached a remote (only the commits not read before:
// pushed.go), and the reminders about work left lying around (hygiene.go).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// scanSince is how far back pushed commits are read for personal data
	// without --since.
	scanSince = "7 days ago"
	// scanWorkers is how many checkouts are read at the same time.
	scanWorkers   = 8
	scanGHTimeout = 20 * time.Second
)

// CheckoutScan is one checkout against the rules.
type CheckoutScan struct {
	// As given (~/code/app stays ~/code/app).
	Path string `json:"path"`
	// ok, missing or error.
	State  string `json:"state"`
	Detail string `json:"detail"`
	// Its top folder.
	Repository string      `json:"repository"`
	Rule       *string     `json:"rule"`
	Source     *RuleSource `json:"source"`
	// Where the rule lets it go, in words.
	Push *string `json:"push"`
	// Rules that all match it: its pushes are refused until one is left.
	Conflicts []string      `json:"conflicts"`
	Remotes   []RemoteState `json:"remotes"`
	// The remotes its rule does not allow: a push there is refused.
	Forbidden []RemoteState `json:"forbidden"`
	// The pre-push hook, for a checkout with a rule.
	Hook *HookState `json:"hook"`
	// Commits that reached a remote since `since`, read for personal data.
	PushedCommits  int       `json:"pushed_commits"`
	PushedFindings []Finding `json:"pushed_findings"`
	// Why its commits on a remote were not read: nothing was ever pushed
	// from this checkout, so they are someone else's, fetched (a clone of
	// another person's project); null when they were read.
	PushedOff *string `json:"pushed_off"`
	// The reminders; null when they are off for it (hygiene_off says why).
	Hygiene    *Hygiene `json:"hygiene"`
	HygieneOff *string  `json:"hygiene_off"`
}

// ScanReport is every checkout against the rules file.
type ScanReport struct {
	RulesFile      string `json:"rules_file"`
	RulesFileFound bool   `json:"rules_file_found"`
	// What already reached a remote is read from this on (git's --since).
	Since string `json:"since"`
	// The [hygiene] settings; null when the rules file has none (no
	// reminders).
	Hygiene *HygieneRules `json:"hygiene"`
	// GitHub's open pull requests for the scan as a whole: known, unknown
	// (gh could not be asked, pull_requests_detail says why, and each
	// checkout on GitHub has a pull-requests-unknown reminder) or not-asked
	// (no reminders, or no checkout on GitHub).
	PullRequests       string `json:"pull_requests"`
	PullRequestsDetail string `json:"pull_requests_detail,omitempty"`
	// Hosts whose fetches were refused for want of credentials (by
	// `gitdcy checkouts`), each said once; empty when none.
	FetchRefused []FetchRefused `json:"fetch_refused"`
	// How many personal words and allowed addresses are read for (the
	// words are not shown).
	PersonalWords    int `json:"personal_words"`
	AllowedAddresses int `json:"allowed_addresses"`
	// How many words commit messages are read for.
	MessageWords int `json:"message_words"`
	// Who may author and commit a push (the rules file's identities; a
	// [[repo]] may hold its own); empty: no identity check. Read on a
	// push only, never on what already reached a remote.
	AllowedIdentities []string       `json:"allowed_identities"`
	Checkouts         []CheckoutScan `json:"checkouts"`
}

// scanSinceText is the --since git reads: git reads a bare date as that day
// at the current time; a day means from its start.
func scanSinceText(since *string) string {
	if since == nil || trim(*since) == "" {
		return scanSince
	}
	day := trim(*since)
	if len(day) == 10 && strings.Trim(day, "0123456789-") == "" {
		return day + " 00:00"
	}
	return day
}

// scanParallel is items through f, with up to eight at a time, in order.
func scanParallel[T, U any](items []T, f func(T) U) []U {
	out := make([]U, len(items))
	if len(items) == 0 {
		return out
	}
	size := (len(items) + scanWorkers - 1) / scanWorkers
	var wg sync.WaitGroup
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := start; i < end; i++ {
				out[i] = f(items[i])
			}
		}()
	}
	wg.Wait()
	return out
}

func scanFirstLine(text string) string {
	if first := lines(text); len(first) > 0 {
		return trim(first[0])
	}
	return trim(text)
}

// scanRead is one checkout's rules report, or why it could not be read;
// nil when it is no checkout.
type scanRead struct {
	report *RulesReport
	err    string
}

// scanURLs is the checkout's remote URLs, push and fetch, as configured
// (as the push check reads them).
func scanURLs(report *RulesReport) []string {
	var urls []string
	if report.repo != nil {
		for _, remote := range report.repo.remotes {
			urls = append(urls, remote.urls...)
		}
		return urls
	}
	for _, remote := range report.Remotes {
		if remote.URL != nil {
			urls = append(urls, *remote.URL)
		}
	}
	return urls
}

// scanOwnURLs is the URLs of the remotes something was pushed to from this
// checkout: its own repositories, the ones whose open pull requests are
// asked for (a fork's upstream, or a clone of another person's project,
// is not).
func scanOwnURLs(report *RulesReport) []string {
	if report.repo == nil || report.repo.gitDir == "" {
		return scanURLs(report)
	}
	pushed := pushedRemotes(report.repo.gitDir)
	var urls []string
	for _, remote := range report.repo.remotes {
		if pushed[remote.name] {
			urls = append(urls, remote.urls...)
		}
	}
	return urls
}

// FetchRefused is a host this device's fetches are refused by for want of
// credentials: its checkouts' remote-tracking refs are as old as their last
// good fetch.
type FetchRefused struct {
	Host string `json:"host"`
	// When the last fetch was refused (Unix seconds), and what git said.
	At    int64  `json:"at"`
	Error string `json:"error"`
	// The oldest last good fetch among its checkouts (Unix seconds); null
	// when one never fetched.
	Since *int64 `json:"since"`
	// How many checkouts' remote state was not judged for it.
	Checkouts int `json:"checkouts"`
}

// rulesScan reads every checkout in paths against the rules file at
// rulesPath. gh is the program asked for open pull requests (gh on the
// PATH).
func rulesScan(rulesPath string, paths []string, since *string, gitdcy, gh string) (*ScanReport, error) {
	file, err := readRules(rulesPath)
	if err != nil {
		return nil, err
	}
	sinceText := scanSinceText(since)
	var rules *HygieneRules
	config := personalConfig{}
	identities := []string{}
	if file != nil {
		rules = file.Hygiene
		config = personalConfig{words: file.Personal, allow: file.Allow, messageWords: file.MessageWords}
		identities = append(identities, file.Identities...)
	}
	reads := scanParallel(paths, func(given string) *scanRead {
		dir := expandHome(given)
		if _, err := os.Stat(joinPath(dir, ".git")); err != nil {
			return nil
		}
		report, err := rulesReport(rulesPath, dir, gitdcy)
		if err != nil {
			return &scanRead{err: scanFirstLine(chainText(err))}
		}
		return &scanRead{report: report}
	})
	// One question to GitHub per owner, for every checkout the reminders
	// are for.
	var owners []string
	if rules != nil {
		set := map[string]bool{}
		for _, read := range reads {
			if read == nil || read.report == nil {
				continue
			}
			if _, skipped := rules.skippedBy(scanURLs(read.report)); skipped {
				continue
			}
			if _, skipped := rules.skippedFolder(read.report.Repository); skipped {
				continue
			}
			if repo, ok := githubRepository(scanOwnURLs(read.report)); ok {
				if owner, _, ok := splitOnce(repo, "/"); ok {
					set[owner] = true
				}
			}
		}
		owners = sortedKeys(set)
	}
	github := hygUnknown("not asked")
	if len(owners) > 0 {
		github = hygAsk(gh, owners, nil, scanGHTimeout)
	}
	pullRequests, detail := "not-asked", ""
	if len(owners) > 0 {
		pullRequests = "known"
		if why, failed := github.failure(); failed {
			pullRequests, detail = "unknown", why
		}
	}
	type job struct {
		given string
		read  *scanRead
	}
	jobs := make([]job, len(paths))
	for i := range paths {
		jobs[i] = job{paths[i], reads[i]}
	}
	refusals := fetchFailLoad(fetchFailPath())
	cachePath := pushedPath()
	cache := pushedLoad(cachePath)
	base := pushedBase(rulesPath)
	type done struct {
		found CheckoutScan
		// What to keep of its pushed commits; nil when they were not read.
		kept *pushedRead
	}
	results := scanParallel(jobs, func(j job) done {
		found := CheckoutScan{Path: j.given}
		switch {
		case j.read == nil:
			found.State = "missing"
			found.Detail = "no folder at that path"
			if rulesIsDir(expandHome(j.given)) {
				found.Detail = "not a git checkout at that path"
			}
			return done{found, nil}
		case j.read.report == nil:
			found.State, found.Detail = "error", j.read.err
			return done{found, nil}
		}
		report := j.read.report
		found.State = "ok"
		found.Repository = report.Repository
		if report.Rule != nil {
			name := report.Rule.Name
			found.Rule = &name
			hook := report.Hook
			found.Hook = &hook
		}
		found.Source, found.Push, found.Conflicts = report.Source, report.Push, report.Conflicts
		for _, remote := range report.Remotes {
			if !remote.Allowed && remote.URL != nil {
				found.Forbidden = append(found.Forbidden, remote)
			}
		}
		found.Remotes = report.Remotes
		root := report.Repository
		config := config
		config.rulesFile = personalRulesFile(rulesPath, root)
		allowed := personalOwnIdentities(root, config.allow)
		var kept *pushedRead
		if !pushedFromHere(report.repo.gitDir) {
			off := "nothing was pushed from this checkout, so what is on its remotes came by fetch, someone else's"
			found.PushedOff = &off
		} else {
			commits, findings, read, err := pushedScan(root, sinceText, config, allowed, pushedAgainst(base, allowed), cache.Checkouts[root])
			if err != nil {
				found.Detail = "what reached a remote could not be read: " + scanFirstLine(chainText(err))
			} else {
				found.PushedCommits, found.PushedFindings, kept = commits, findings, read
			}
		}
		urls := scanURLs(report)
		if rules == nil {
			off := "the rules file has no [hygiene] table"
			found.HygieneOff = &off
		} else if prefix, skipped := rules.skippedBy(urls); skipped {
			off := "keeps its own rules: its remote starts with " + prefix + " ([hygiene] skip)"
			found.HygieneOff = &off
		} else if name, skipped := rules.skippedFolder(root); skipped {
			off := "a copy kept as it was: it lies under a folder named " + name + " ([hygiene] skip_folders)"
			found.HygieneOff = &off
		} else if hygEmpty(root) {
			off := "an empty checkout: no commit and no file"
			found.HygieneOff = &off
		} else {
			own := scanOwnURLs(report)
			h := hygRead(report.repo, *rules, own, github, nil, false)
			if _, onGitHub := githubRepository(urls); onGitHub && h.PullRequests.State == "not-asked" {
				h.PullRequests.Detail = "nothing was pushed to its GitHub remote from here: someone else's repository, not asked"
			}
			if host, _, refused := refusals.refusing(urls); refused {
				hygStale(&h, host)
			}
			found.Hygiene = &h
		}
		return done{found, kept}
	})
	checkouts := make([]CheckoutScan, len(results))
	for i, r := range results {
		checkouts[i] = r.found
		if r.kept != nil {
			cache.Checkouts[r.found.Repository] = r.kept
		}
	}
	// A checkout that is gone keeps nothing.
	for root := range cache.Checkouts {
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			delete(cache.Checkouts, root)
		}
	}
	pushedSave(cachePath, cache)
	return &ScanReport{
		FetchRefused:       scanFetchRefused(refusals, checkouts),
		RulesFile:          rulesPath,
		RulesFileFound:     file != nil,
		Since:              sinceText,
		Hygiene:            rules,
		PullRequests:       pullRequests,
		PullRequestsDetail: detail,
		PersonalWords:      len(config.words),
		AllowedAddresses:   len(config.allow),
		MessageWords:       len(config.messageWords),
		AllowedIdentities:  identities,
		Checkouts:          checkouts,
	}, nil
}

// scanFetchRefused is each refusing host with the checkouts it left stale,
// by host.
func scanFetchRefused(refusals *fetchFailures, checkouts []CheckoutScan) []FetchRefused {
	byHost := map[string]*FetchRefused{}
	for _, c := range checkouts {
		if c.Hygiene == nil || c.Hygiene.StaleRemote == nil {
			continue
		}
		host := *c.Hygiene.StaleRemote
		r, ok := byHost[host]
		if !ok {
			failure := refusals.Hosts[host]
			r = &FetchRefused{Host: host, At: failure.At, Error: failure.Error}
			byHost[host] = r
		}
		r.Checkouts++
		if at := c.Hygiene.FetchedAt; at != nil && (r.Since == nil || *at < *r.Since) {
			since := *at
			r.Since = &since
		}
	}
	list := []FetchRefused{}
	for _, host := range sortedKeys(byHost) {
		list = append(list, *byHost[host])
	}
	return list
}

// scanStaleText is what a refusing host leaves stale, in words.
func scanStaleText(f FetchRefused) string {
	since := "never fetched"
	if f.Since != nil {
		since = "as of " + time.Unix(*f.Since, 0).UTC().Format("2006-01-02")
	}
	return fmt.Sprintf("the remote refs of %d checkout(s) are %s, so pulls, pushes and merges against them are not judged (give this device a login for it, then gitdcy checkouts)", f.Checkouts, since)
}
