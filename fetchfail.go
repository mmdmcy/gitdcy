package main

// Hosts this device cannot fetch from for want of credentials. `gitdcy
// checkouts` fetches; when a fetch is refused for want of a login (git
// could not ask for a user name, a credential helper wanted a prompt, the
// server answered 401 or 403, an SSH key was refused), the remote's host is
// kept here, and the next good fetch from that host clears it. A scan reads
// it: a checkout with a remote on such a host has remote-tracking refs as of
// its last good fetch, so what it says about pulls, pushes and merges is
// not judged, and the scan says once per host that this device cannot
// fetch there. Any host and any transport: only git's own words are read.
//
// Kept in the user's cache folder (gitdcy/fetch-failures.json there);
// losing it costs nothing but the notice until the next fetch.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	fetchFailSchema = "gitdcy.fetch-failures.v1"
	fetchFailFile   = "fetch-failures.json"
)

// fetchFailure is one host's last refused fetch.
type fetchFailure struct {
	// When it was refused (Unix seconds).
	At int64 `json:"at"`
	// What git said, its first line.
	Error string `json:"error"`
}

type fetchFailures struct {
	Schema string `json:"schema"`
	// By host, lowercase.
	Hosts map[string]fetchFailure `json:"hosts"`
}

// fetchCredentialWords are what git, its credential helpers and its
// transports say when a fetch is refused for want of a login.
var fetchCredentialWords = []string{
	"could not read username",
	"could not read password",
	"terminal prompts disabled",
	"user interactivity has been disabled",
	"authentication failed",
	"invalid username or password",
	"invalid username or token",
	"permission denied (publickey",
	"returned error: 401",
	"returned error: 403",
	"http 401",
	"http 403",
}

// fetchForWantOfCredentials reports whether git's words say the fetch was
// refused for want of a login.
func fetchForWantOfCredentials(said string) bool {
	said = strings.ToLower(said)
	for _, words := range fetchCredentialWords {
		if strings.Contains(said, words) {
			return true
		}
	}
	return false
}

func fetchFailPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "gitdcy", fetchFailFile)
}

func fetchFailLoad(path string) *fetchFailures {
	empty := &fetchFailures{Schema: fetchFailSchema, Hosts: map[string]fetchFailure{}}
	if path == "" {
		return empty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var file fetchFailures
	if json.Unmarshal(data, &file) != nil || file.Schema != fetchFailSchema || file.Hosts == nil {
		return empty
	}
	return &file
}

// fetchFailSave writes the file whole (a new file renamed into place),
// readable by the user only; one that cannot be written is no error.
func fetchFailSave(path string, file *fetchFailures) {
	if path == "" {
		return
	}
	data, err := json.Marshal(file)
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

// fetchFailNote keeps what the fetches of one run said: a host refused for
// want of credentials is kept, one that fetched (and was refused by none)
// is cleared. It reports whether anything changed.
func (f *fetchFailures) note(checkouts []checkoutsCheckout, at int64) bool {
	refused, fetched := map[string]string{}, map[string]bool{}
	for _, c := range checkouts {
		host, ok := remoteHost(c.Remote)
		if !ok || !c.fetchTried {
			continue
		}
		switch {
		case c.FetchError == "":
			fetched[host] = true
		case c.fetchForCredentials:
			refused[host] = c.FetchError
		}
	}
	changed := false
	for host := range fetched {
		if _, ok := refused[host]; !ok {
			if _, kept := f.Hosts[host]; kept {
				delete(f.Hosts, host)
				changed = true
			}
		}
	}
	for host, said := range refused {
		f.Hosts[host] = fetchFailure{At: at, Error: said}
		changed = true
	}
	return changed
}

// refusing is the host of one of these remote URLs that refused a fetch,
// if any.
func (f *fetchFailures) refusing(urls []string) (string, fetchFailure, bool) {
	for _, url := range urls {
		if host, ok := remoteHost(normalizeURL(url)); ok {
			if failure, kept := f.Hosts[host]; kept {
				return host, failure, true
			}
		}
	}
	return "", fetchFailure{}, false
}
