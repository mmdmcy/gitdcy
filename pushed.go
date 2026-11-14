package main

// What `gitdcy rules scan` already read of the commits that reached a
// remote. A commit id names the commit's content, so what was found in it
// holds as long as what it was read against: the rules file, the
// repository's own identities and gitdcy itself. A scan lists the commits
// pushed in its window (one git call without diffs) and reads only the
// ones it has not read before; a change to any of those three reads every
// commit in the window again. A rewritten history has new ids, so it is
// read again too.
//
// Kept per checkout in the user's cache folder (gitdcy/pushed-read.json
// there), only for the commits the checkout's last scan listed. The file
// holds commit ids and what was found, masked as a scan shows it; losing it
// costs one full read.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// pushedSchema names the cache file's layout; a file with another is
	// read as empty.
	pushedSchema = "gitdcy.pushed-read.v1"
	pushedFile   = "pushed-read.json"
)

// pushedCache is the cache file.
type pushedCache struct {
	Schema string `json:"schema"`
	// By the checkout's top folder.
	Checkouts map[string]*pushedRead `json:"checkouts"`
}

// pushedRead is one checkout's: what its commits were read against, and
// what was found in each, by full commit id (null when nothing).
type pushedRead struct {
	Against string               `json:"against"`
	Commits map[string][]Finding `json:"commits"`
}

// pushedPath is the cache file; "" when the user has no cache folder.
func pushedPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gitdcy", pushedFile)
}

// pushedLoad is the cache file at path; empty when it is not there or not
// readable as this schema.
func pushedLoad(path string) *pushedCache {
	cache := &pushedCache{Schema: pushedSchema, Checkouts: map[string]*pushedRead{}}
	if path == "" {
		return cache
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cache
	}
	var file pushedCache
	if json.Unmarshal(data, &file) != nil || file.Schema != pushedSchema || file.Checkouts == nil {
		return cache
	}
	return &file
}

// pushedSave writes the cache file whole (a new file renamed into place),
// readable by the user only. A cache that cannot be written is read again
// next time; that is no error.
func pushedSave(path string, cache *pushedCache) {
	if path == "" {
		return
	}
	data, err := json.Marshal(cache)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	part := path + "." + strconv.Itoa(os.Getpid()) + ".part"
	if os.WriteFile(part, data, 0o600) != nil {
		os.Remove(part)
		return
	}
	if os.Rename(part, path) != nil {
		os.Remove(part)
	}
}

// pushedProgram names this gitdcy build (its path, size and time), so a new
// build reads every commit again.
func pushedProgram() string {
	program, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(program)
	if err != nil {
		return program
	}
	return program + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// pushedAgainst is what one checkout's commits are read against: the rules
// file's text and the program (base, the same for the whole scan), and the
// addresses that are no finding there (the allow list and its own
// identities).
func pushedAgainst(base string, allowed map[string]bool) string {
	addresses := make([]string, 0, len(allowed))
	for address := range allowed {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	sum := sha256.Sum256([]byte(pushedSchema + "\x00" + base + "\x00" + strings.Join(addresses, "\n")))
	return hex.EncodeToString(sum[:])
}

// pushedBase is the part of what commits are read against that is the
// same for every checkout: the rules file's text and this program.
func pushedBase(rulesPath string) string {
	text, _ := os.ReadFile(rulesPath)
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:]) + "\x00" + pushedProgram()
}

// pushedList is the commits git log selects with revs, in its order, with
// their full and abbreviated ids: one walk, no diffs.
func pushedList(repo string, revs []string) ([]personalCommit, error) {
	out, err := gitOutput(repo, append([]string{"rev-list", "--format=%h"}, revs...)...)
	if err != nil {
		return nil, err
	}
	var list []personalCommit
	for _, line := range lines(out) {
		if id, ok := strings.CutPrefix(line, "commit "); ok {
			list = append(list, personalCommit{id: id})
		} else if len(list) > 0 {
			list[len(list)-1].short = line
		}
	}
	return list, nil
}

// pushedScan reads what reached a remote since `since` in the checkout at
// root for personal data. With kept read against the same (against), only
// the commits kept lacks are read; otherwise every commit in the window.
// It returns the commits in the window, what was found in them in git
// log's order, and what to keep for the next scan.
func pushedScan(root, since string, config personalConfig, allowed map[string]bool, against string, kept *pushedRead) (int, []Finding, *pushedRead, error) {
	revs := []string{"--remotes", "--since=" + since}
	next := &pushedRead{Against: against, Commits: map[string][]Finding{}}
	var commits []personalCommit
	if kept == nil || kept.Against != against {
		all, err := personalLog(root, revs, "", config, allowed)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, c := range all {
			if c.id != "" {
				next.Commits[c.id] = c.findings
			}
		}
		commits = all
	} else {
		listed, err := pushedList(root, revs)
		if err != nil {
			return 0, nil, nil, err
		}
		var missing []string
		for _, c := range listed {
			if found, ok := kept.Commits[c.id]; ok {
				next.Commits[c.id] = found
			} else {
				missing = append(missing, c.id)
			}
		}
		if len(missing) > 0 {
			read, err := personalLog(root, []string{"--no-walk=unsorted", "--stdin"}, strings.Join(missing, "\n")+"\n", config, allowed)
			if err != nil {
				return 0, nil, nil, err
			}
			for _, c := range read {
				if c.id != "" {
					next.Commits[c.id] = c.findings
				}
			}
		}
		for i := range listed {
			listed[i].findings = next.Commits[listed[i].id]
		}
		commits = listed
	}
	var findings []Finding
	for _, c := range commits {
		for _, f := range c.findings {
			// Named as git abbreviates the commit now.
			f.Commit = c.short
			findings = append(findings, f)
		}
	}
	return len(commits), findings, next, nil
}

// pushedFromHere says whether anything was ever pushed from the checkout
// whose git folder this is: git logs "update by push" on a remote-tracking
// ref at each push. A clone nobody pushes from (another person's project)
// has only fetches there, so the commits on its remotes are not ours to
// read: their authors' addresses are theirs (2026-11-14, a fetch of a cloned
// project raised "personal data reached a remote").
func pushedFromHere(gitDir string) bool {
	if gitDir == "" {
		return true
	}
	found := false
	_ = filepath.WalkDir(filepath.Join(gitDir, "logs", "refs", "remotes"), func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "\tupdate by push") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
