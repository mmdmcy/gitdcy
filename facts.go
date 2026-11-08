package main

// What a tool that moves or retires a checkout needs to know so that no work
// is lost (`gitdcy checkouts --facts`): the exact commit, branch and
// upstream, local changes counted file by file, stashes, commits that no
// remote holds, ignored files that usually cannot be rebuilt (by name only),
// content fingerprints of the local changes, and the id of the whole working
// tree.
//
// Read-only: every git call runs with hooks and fsmonitor off and without
// optional locks, and the working-tree id is computed in a throwaway index
// and object folder, so the repository is never written to. A git read that
// answers more than 1 MiB or takes longer than 8 seconds counts as failed;
// each fact then falls back as its field says. The facts are read on their
// own, so a failure in the rest of the checkout's report does not hide them.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	factsOutputLimit = 1024 * 1024
	factsTimeout     = 8 * time.Second
	// factsMaxFingerprints: past this many changed files the fingerprints
	// are left out...
	factsMaxFingerprints = 4096
	// factsMaxFingerprintBytes: ...and past this many bytes to hash.
	factsMaxFingerprintBytes = 1024 * 1024 * 1024
)

// factsReport is every fact about one checkout.
type factsReport struct {
	// Head is the full id of the commit checked out; null before the first
	// commit.
	Head *string `json:"head"`
	// Branch is `git symbolic-ref --short HEAD`; null when no branch is
	// checked out.
	Branch *string `json:"branch"`
	// Upstream is the branch it follows (origin/main); null when none.
	Upstream *string `json:"upstream"`
	// Ahead: commits on HEAD that the upstream lacks; 0 without an upstream.
	Ahead uint64 `json:"ahead"`
	// Behind: commits on the upstream that HEAD lacks; 0 without an upstream.
	Behind uint64 `json:"behind"`
	// Origin is origin's URL without user or password, the rest as
	// configured; null when there is no origin or its URL is not safe to
	// show.
	Origin *string `json:"origin"`
	// StatusError is why `git status` failed; changed and untracked are then
	// 0 and there are no fingerprints.
	StatusError *string `json:"status_error"`
	// Changed is the entries `git status --untracked-files=all` lists, so
	// each untracked file counts on its own.
	Changed uint64 `json:"changed"`
	// Untracked is, of those, the untracked files.
	Untracked uint64 `json:"untracked"`
	// Stashes are the commit ids of the stash entries, newest first.
	Stashes []string `json:"stashes"`
	// UnpushedCommits: commits on local branches that no remote-tracking
	// branch holds.
	UnpushedCommits uint64 `json:"unpushed_commits"`
	// UnpushedTips are the local branch tips among those commits, sorted.
	UnpushedTips []string `json:"unpushed_tips"`
	// IgnoredState is ignored files and folders that usually hold what cannot
	// be rebuilt (.env, local databases, keys), by name; never their
	// contents.
	IgnoredState []string `json:"ignored_state"`
	// Fingerprints is `DIGEST PATH` for every file that differs from HEAD
	// (changed, added or untracked; deletions lose no work and are left
	// out), with DIGEST `sha256:HEX` of its contents, `link:HEX` of a
	// symlink's target or `dir`. Null when there are no local changes, `git
	// status` failed, a path is unusual, or past 4096 files or 1 GiB.
	Fingerprints *[]string `json:"fingerprints"`
	// Tree is the tree id `git add -A && git write-tree` would give, so
	// identical working trees give identical ids on every device. Null when
	// there are no fingerprints.
	Tree *string `json:"tree"`
	// CarryTrees are the tree ids of the commits under refs/carry/ (working
	// trees another device's tool parked here), sorted.
	CarryTrees []string `json:"carry_trees"`
}

// factsRead is every fact about the checkout at dir (its top folder).
func factsRead(dir string) factsReport {
	text := func(args ...string) (string, error) { return factsGitText(dir, args, nil) }
	optional := func(args ...string) *string {
		if v, err := text(args...); err == nil {
			return &v
		}
		return nil
	}
	status, statusErr := text("status", "--porcelain=v1", "--untracked-files=all")
	upstream := optional("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	var ahead, behind uint64
	if upstream != nil {
		if counts, err := text("rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
			values := strings.Fields(counts)
			if len(values) >= 2 {
				b, okB := checkoutsParseUint(values[0], 64)
				a, okA := checkoutsParseUint(values[1], 64)
				if okB && okA {
					ahead, behind = a, b
				}
			}
		}
	}
	var changed, untracked uint64
	var statusError *string
	if statusErr == nil {
		for _, line := range lines(status) {
			if line == "" {
				continue
			}
			changed++
			if strings.HasPrefix(line, "??") {
				untracked++
			}
		}
	} else {
		message := statusErr.Error()
		statusError = &message
	}
	unpushedCommits, unpushedTips := factsUnpushed(dir)
	var fingerprints *[]string
	if changed > 0 && statusError == nil {
		fingerprints = factsFingerprints(dir)
	}
	var tree *string
	if fingerprints != nil {
		tree = factsWorkingTreeID(dir)
	}
	head := optional("rev-parse", "HEAD")
	branch := optional("symbolic-ref", "--short", "HEAD")
	var origin *string
	if url, err := text("remote", "get-url", "origin"); err == nil {
		origin = factsWithoutCredentials(url)
	}
	stashText, _ := text("stash", "list", "--format=%H")
	stashes := factsLines(stashText)
	ignored := factsNotableIgnoredState(dir)
	var carryTrees []string
	if trees, err := text("for-each-ref", "--format=%(tree)", "refs/carry/"); err == nil {
		for _, t := range lines(trees) {
			if factsIsObjectID(t) {
				carryTrees = append(carryTrees, t)
			}
		}
		carryTrees = sortDedup(carryTrees)
	}
	return factsReport{
		Head:            head,
		Branch:          branch,
		Upstream:        upstream,
		Ahead:           ahead,
		Behind:          behind,
		Origin:          origin,
		StatusError:     statusError,
		Changed:         changed,
		Untracked:       untracked,
		Stashes:         stashes,
		UnpushedCommits: unpushedCommits,
		UnpushedTips:    unpushedTips,
		IgnoredState:    ignored,
		Fingerprints:    fingerprints,
		Tree:            tree,
		CarryTrees:      carryTrees,
	}
}

// factsUnpushed is the commits reachable from local branches but from no
// remote-tracking ref, and the branch tips that hold them: these exist
// nowhere but here.
func factsUnpushed(dir string) (uint64, []string) {
	listing, err := factsGitText(dir, []string{"rev-list", "--branches", "--not", "--remotes"}, nil)
	if err != nil {
		return 0, nil
	}
	commits := map[string]bool{}
	for _, line := range lines(listing) {
		if line != "" {
			commits[line] = true
		}
	}
	if len(commits) == 0 {
		return 0, nil
	}
	var tips []string
	if heads, err := factsGitText(dir, []string{"for-each-ref", "--format=%(objectname)", "refs/heads"}, nil); err == nil {
		for _, tip := range lines(heads) {
			if commits[tip] {
				tips = append(tips, tip)
			}
		}
		tips = sortDedup(tips)
	}
	return uint64(len(commits)), tips
}

// factsNotableIgnoredState is the ignored files that usually hold
// irreplaceable local state (secrets, local databases, private notes)
// rather than rebuildable output, in git's order.
func factsNotableIgnoredState(dir string) []string {
	args := []string{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory"}
	listing, err := factsGitText(dir, args, nil)
	if err != nil {
		return nil
	}
	var found []string
	for _, line := range lines(listing) {
		path := strings.TrimRight(line, "/")
		if path != "" && factsIsNotableIgnoredPath(path) {
			found = append(found, path)
		}
	}
	return found
}

// factsRebuildable are folders whose contents can be rebuilt.
var factsRebuildable = []string{
	".git", ".cache", ".dart_tool", ".gradle", ".venv", "__pycache__", "build", "dist",
	"node_modules", "target", "vendor", "coverage", "ephemeral", ".idea", ".next", ".nuxt",
	".astro", ".turbo", ".svelte-kit", ".terraform", ".expo", "Pods", "DerivedData",
}

// factsIsNotableIgnoredPath reports whether an ignored path is likely state
// that cannot be rebuilt: .env files (not examples), databases, keys,
// *.private.* notes and a few well-known data folders, outside build and
// dependency folders.
func factsIsNotableIgnoredPath(path string) bool {
	// A folder ending in `-partial` is a copy still being written (rsync's
	// `--partial-dir`), never state of its own.
	for _, part := range strings.Split(path, "/") {
		if contains(factsRebuildable, part) || strings.HasSuffix(part, "-partial") {
			return false
		}
	}
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	lower := asciiLower(name)
	if strings.HasPrefix(lower, ".env") && !strings.HasSuffix(lower, ".example") && !strings.HasSuffix(lower, ".sample") {
		return true
	}
	for _, suffix := range []string{".db", ".sqlite", ".sqlite3", ".pem", ".key", ".p12", ".jks", ".keystore"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	if strings.Contains(lower, ".private.") {
		return true
	}
	switch lower {
	case "pb_data", "secrets", ".secrets", "credentials.json":
		return true
	}
	return false
}

// factsFingerprints is the content digests of the files that differ from
// HEAD; see factsReport.Fingerprints.
func factsFingerprints(dir string) *[]string {
	changed, err := factsGitText(dir, []string{"diff", "--name-only", "-z", "HEAD"}, nil)
	if err != nil {
		return nil
	}
	untracked, err := factsGitText(dir, []string{"ls-files", "--others", "--exclude-standard", "-z"}, nil)
	if err != nil {
		return nil
	}
	var paths []string
	for _, path := range append(strings.Split(changed, "\x00"), strings.Split(untracked, "\x00")...) {
		if path != "" {
			paths = append(paths, path)
		}
	}
	paths = sortDedup(paths)
	if len(paths) > factsMaxFingerprints {
		return nil
	}
	var hashed uint64
	fingerprints := make([]string, 0, len(paths))
	for _, path := range paths {
		if !factsPlainRelativePath(strings.TrimRight(path, "/")) {
			return nil
		}
		full := joinPath(dir, path)
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		var digest string
		switch {
		case info.IsDir():
			digest = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return nil
			}
			digest = "link:" + factsSHA256Hex([]byte(target))
		default:
			size := uint64(max(info.Size(), 0))
			if hashed+size < hashed {
				hashed = ^uint64(0)
			} else {
				hashed += size
			}
			if hashed > factsMaxFingerprintBytes {
				return nil
			}
			sum, err := factsSHA256File(full)
			if err != nil {
				return nil
			}
			digest = "sha256:" + sum
		}
		fingerprints = append(fingerprints, digest+" "+path)
	}
	return &fingerprints
}

// factsWorkingTreeID is the tree id `git add -A && git write-tree` would
// produce, computed with a throwaway index and object folder so the
// repository itself is never written to.
func factsWorkingTreeID(dir string) *string {
	path := func(name string) (string, bool) {
		v, err := factsGitText(dir, []string{"rev-parse", "--path-format=absolute", "--git-path", name}, nil)
		return v, err == nil
	}
	index, ok := path("index")
	if !ok {
		return nil
	}
	objects, ok := path("objects")
	if !ok {
		return nil
	}
	scratch, ok := factsScratchFolder()
	if !ok {
		return nil
	}
	defer os.RemoveAll(scratch)
	environment, err := factsScratchEnvironment(scratch, index, objects)
	if err != nil {
		return nil
	}
	if _, err := factsGitText(dir, []string{"add", "-A"}, environment); err != nil {
		return nil
	}
	tree, err := factsGitText(dir, []string{"write-tree"}, environment)
	if err != nil || !factsIsObjectID(tree) {
		return nil
	}
	return &tree
}

// factsScratchEnvironment copies the index (when there is one) into scratch
// and makes its object folder: git's environment (name, value pairs) for
// writing into the scratch only, reading the checkout's own objects
// alongside.
func factsScratchEnvironment(scratch, index, objects string) ([]string, error) {
	scratchIndex := joinPath(scratch, "index")
	if info, err := os.Stat(index); err == nil && info.Mode().IsRegular() {
		if err := factsCopyFile(index, scratchIndex); err != nil {
			return nil, wrap(err, "copy the index")
		}
	}
	scratchObjects := joinPath(scratch, "objects")
	if err := os.Mkdir(scratchObjects, 0o777); err != nil {
		return nil, wrap(err, "make the scratch object folder")
	}
	return []string{
		"GIT_INDEX_FILE", scratchIndex,
		"GIT_OBJECT_DIRECTORY", scratchObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", objects,
	}, nil
}

// factsCopyFile copies a file's contents and permissions (Rust's fs::copy).
func factsCopyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return ioError(err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return ioError(err)
	}
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return ioError(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return ioError(err)
	}
	if err := target.Close(); err != nil {
		return ioError(err)
	}
	return nil
}

var factsScratchCounter atomic.Uint64

// factsScratchFolder is a new private folder under the temporary directory.
func factsScratchFolder() (string, bool) {
	folder := joinPath(os.TempDir(), fmt.Sprintf("gitdcy-tree-%d-%d-%d",
		os.Getpid(), time.Now().UnixNano(), factsScratchCounter.Add(1)-1))
	if err := os.Mkdir(folder, 0o700); err != nil {
		return "", false
	}
	return folder, true
}

// factsWithoutCredentials is origin's URL with any user and password removed
// and the rest exactly as written; nil when it is empty, too long, has
// control characters or spaces, or is a `user:password@host:path` form that
// cannot be cleaned.
func factsWithoutCredentials(url string) *string {
	if url == "" || len(url) > 2048 {
		return nil
	}
	for _, c := range url {
		if unicode.IsControl(c) || unicode.IsSpace(c) {
			return nil
		}
	}
	// scheme://[user[:password]@]host/path: drop the user part.
	if scheme, rest, ok := splitOnce(url, "://"); ok && scheme != "" && factsSchemeName(scheme) {
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		authority, path := rest[:end], rest[end:]
		host := authority
		if _, h, ok := rsplitOnce(authority, "@"); ok {
			host = h
		}
		cleaned := scheme + "://" + host + path
		return &cleaned
	}
	if user, _, ok := splitOnce(url, "@"); ok && (user == "" || strings.Contains(user, ":")) {
		return nil
	}
	return &url
}

func factsSchemeName(scheme string) bool {
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		if !isASCIIAlnum(rune(c)) && c != '+' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// factsGitText is git's trimmed answer, or why there is none; env is name,
// value pairs added to git's environment.
func factsGitText(dir string, args []string, env []string) (string, error) {
	cmd := readingGitCommand(dir, args...)
	for i := 0; i+1 < len(env); i += 2 {
		setEnv(cmd, env[i], env[i+1])
	}
	what := "git"
	if len(args) > 0 {
		what = "git " + args[0]
	}
	out, err := runBounded(cmd, factsOutputLimit, factsTimeout)
	if err != nil {
		return "", errf("%s answered more than 1 MiB or could not start", what)
	}
	if !out.success() {
		for _, line := range lines(lossy(out.Stderr)) {
			if said := trim(line); said != "" {
				return "", errf("%s failed: %s", what, said)
			}
		}
		return "", errf("%s failed or took longer than %d s", what, int(factsTimeout.Seconds()))
	}
	if !utf8.Valid(out.Stdout) {
		return "", errf("%s answered something that is not UTF-8", what)
	}
	return trim(string(out.Stdout)), nil
}

// factsLines is the lines of text that are not empty.
func factsLines(text string) []string {
	var list []string
	for _, line := range lines(text) {
		if line != "" {
			list = append(list, line)
		}
	}
	return list
}

func factsIsObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// factsPlainRelativePath reports a relative path without empty, `.` or
// `..` parts or control characters.
func factsPlainRelativePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	for _, c := range path {
		if unicode.IsControl(c) {
			return false
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func factsSHA256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", ioError(err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", ioError(err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func factsSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
