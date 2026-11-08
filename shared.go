package main

// Small parts several commands share: remote URLs as GitDCY shows them,
// today's date, keeping a file aside, and the gitdcy.response.v1 envelope.

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// isScpLike is git's own rule: `host:path` with no `/` before the `:` is an
// SSH address; a Windows drive (`C:\...`) is not.
func isScpLike(url string) bool {
	if strings.Contains(url, "://") {
		return false
	}
	colon := strings.Index(url, ":")
	if colon < 0 {
		return false
	}
	before := url[:colon]
	if strings.Contains(before, "/") || strings.Contains(before, "\\") {
		return false
	}
	if len(before) == 1 && ((before[0] >= 'a' && before[0] <= 'z') || (before[0] >= 'A' && before[0] <= 'Z')) {
		return false
	}
	return true
}

// isLocal reports whether a URL is a path on this device (`/srv/git/x.git`,
// `../x`, `file:///srv/x`).
func isLocal(url string) bool {
	url = trim(url)
	return strings.HasPrefix(url, "file://") || (!strings.Contains(url, "://") && !isScpLike(url))
}

func stripPort(host string) string {
	if rest, ok := strings.CutPrefix(host, "["); ok {
		before, _, _ := strings.Cut(rest, "]")
		return before
	}
	if name, port, ok := rsplitOnce(host, ":"); ok {
		digits := true
		for _, c := range port {
			if c < '0' || c > '9' {
				digits = false
			}
		}
		if digits {
			return name
		}
	}
	return host
}

// normalizeURL turns `git@host:org/repo.git`, `ssh://git@host:2222/org/repo`
// and `https://user:secret@host/org/repo` into `host/org/repo`: lowercase,
// without user, password, port or `.git`. A local path stays the path it
// is. What a remote is shown as, so a URL with a token in it is never
// printed whole.
func normalizeURL(url string) string {
	url = trim(url)
	if isLocal(url) {
		path := strings.TrimPrefix(url, "file://")
		return strings.TrimRight(path, "/")
	}
	var joined string
	if _, rest, ok := splitOnce(url, "://"); ok {
		authority, path, _ := splitOnce(rest, "/")
		host := authority
		if _, h, ok := rsplitOnce(authority, "@"); ok {
			host = h
		}
		joined = stripPort(host) + "/" + path
	} else if isScpLike(url) {
		authority, path, _ := splitOnce(url, ":")
		host := authority
		if _, h, ok := rsplitOnce(authority, "@"); ok {
			host = h
		}
		joined = host + "/" + strings.TrimLeft(path, "/")
	} else {
		joined = url
	}
	lower := strings.ToLower(joined)
	trimmed := strings.TrimRight(lower, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	return strings.TrimRight(trimmed, "/")
}

// today is YYYY-MM-DD in UTC.
func today() string {
	return civilDate(time.Now().Unix() / 86400)
}

// civilDate turns days since 1970-01-01 into YYYY-MM-DD.
func civilDate(days int64) string {
	return time.Unix(days*86400, 0).UTC().Format("2006-01-02")
}

// keepAside moves path aside as NAME.bak-YYYY-MM-DD (-2, -3, ... when
// taken) and returns where it went.
func keepAside(path string) (string, error) {
	name, _ := fileName(path)
	stamp := today()
	target := withFileName(path, name+".bak-"+stamp)
	for next := 2; ; next++ {
		if _, err := os.Lstat(target); err != nil {
			break
		}
		target = withFileName(path, fmt.Sprintf("%s.bak-%s-%d", name, stamp, next))
	}
	if err := os.Rename(path, target); err != nil {
		return "", wrapf(err, "moving %s aside to %s", path, target)
	}
	return target, nil
}

// envelope is the gitdcy.response.v1 envelope around a command's result;
// an error is in the envelope too, so a script always reads JSON.
type envelope struct {
	SchemaVersion string     `json:"schema_version"`
	Component     string     `json:"component"`
	Operation     string     `json:"operation"`
	Outcome       string     `json:"outcome"`
	Data          any        `json:"data"`
	Warnings      []string   `json:"warnings"`
	Error         *errorBody `json:"error"`
	CorrelationID string     `json:"correlation_id"`
}

type errorBody struct {
	Message string `json:"message"`
}

// printJSON prints the envelope: data when err is nil, else the error (its
// whole chain); partial when there are warnings.
func printJSON(operation string, data any, err error, warnings []string) {
	e := envelope{
		SchemaVersion: "gitdcy.response.v1",
		Component:     "katteke.gitdcy",
		Operation:     operation,
		Outcome:       "success",
		Data:          data,
		Warnings:      warnings,
		CorrelationID: correlationID(),
	}
	if err != nil {
		e.Outcome, e.Data = "error", nil
		e.Error = &errorBody{Message: chainText(err)}
	} else if len(warnings) > 0 {
		e.Outcome = "partial"
	}
	fmt.Println(jsonPretty(e))
}

// correlationID is GITDCY_CORRELATION_ID when set, else one made up.
func correlationID() string {
	if v := os.Getenv("GITDCY_CORRELATION_ID"); trim(v) != "" {
		return v
	}
	return fmt.Sprintf("gitdcy-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// formatArg is `--format human|json`, which every command with a report
// takes.
func formatArg() *cliArg {
	return optArg("format", "format", "FORMAT").Possible("human", "json").Default("human")
}

// isJSON reports whether --format json was asked for.
func isJSON(m *cliMatches) bool {
	v, _ := m.str("format")
	return v == "json"
}

// givenOrWorkspace is the paths given, else every enabled repository in the
// workspace.
func givenOrWorkspace(paths []string) ([]string, error) {
	if len(paths) > 0 {
		return paths, nil
	}
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return nil, err
	}
	list := []string{}
	for _, repo := range manifest.Repos {
		if repo.Enabled {
			list = append(list, repo.Path)
		}
	}
	return list, nil
}
