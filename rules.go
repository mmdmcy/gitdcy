package main

// Repository rules: where a repository may be pushed, which branches take
// only a pull request, and the personal-data check on what a push sends.
//
// Rules are declared once per repository, in the owner's rules file (TOML),
// and found by the repository's identity: its first (root) commit, with its
// remote URLs as a hint. They do not depend on the folder a checkout is in,
// so they hold wherever the repository goes. `install` writes a pre-push
// hook and a copy of that one repository's rule into its git folder, so the
// rule also holds on a machine without the rules file.
//
//	# Hosts that count as self-hosted (names or addresses; an SSH alias that
//	# resolves to one of them counts too). Local paths and localhost always do.
//	selfhosted = ["forge.example.com", "192.0.2.10"]
//
//	# The personal-data check: words that never leave in a pushed commit, and
//	# private addresses that do belong in commits.
//	personal = ["Jane Doe"]
//	allow = []
//
//	# Reminders about work left lying around (scan and the push check); a
//	# repository whose remote starts with a `skip` prefix keeps its own rules.
//	[hygiene]
//	branch_days = 3
//	skip = ["github.com/example-employer/"]
//
//	[[repo]]
//	name = "example-app"
//	first_commit = "0123456789abcdef0123456789abcdef01234567"
//	remotes = ["ssh://git@forge.example.com:2222/team/example-app.git"]
//	push = "selfhosted"     # or "nowhere", "anywhere", or ["github.com/example-org/"]
//	protected = ["main"]    # only through a pull request
//	note = "Customer data: it stays on our own forge."

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// rulesFileName is the rules file's name in GitDCY's config folder,
	// when local.toml names no other.
	rulesFileName = "rules.toml"
	// rulesCopyName is one repository's rule, copied into its git folder by
	// `install`.
	rulesCopyName = "gitdcy-rules.toml"
	// rulesHookMark is the line that makes a pre-push hook gitdcy's own.
	rulesHookMark = "# gitdcy rules check: before every push"
	rulesQuestion = "This refusal is a question for the repository's owner: ask them before trying another way."
)

var rulesLocalHosts = []string{"localhost", "127.0.0.1", "::1"}

type rulesPushKind int

const (
	pushAnywhere rulesPushKind = iota
	// pushSelfhosted: only remotes on hosts listed under `selfhosted`,
	// localhost or a local path.
	pushSelfhosted
	// pushNowhere: no push at all.
	pushNowhere
	// pushRemotes: only remote URLs that start with one of Remotes
	// (`github.com/example-org/`).
	pushRemotes
)

// Push is where a repository may be pushed: a word in the rules file, or a
// list of allowed remote URL prefixes.
type Push struct {
	Kind    rulesPushKind
	Remotes []string
}

func (p Push) isEmpty() bool { return p.Kind == pushAnywhere }

func (p Push) word() any {
	switch p.Kind {
	case pushSelfhosted:
		return "selfhosted"
	case pushNowhere:
		return "nowhere"
	case pushRemotes:
		return append([]string{}, p.Remotes...)
	}
	return "anywhere"
}

func (p Push) jsonValue() any  { return p.word() }
func (p Push) encodeTOML() any { return p.word() }

// decodeTOML reads push as serde read its untagged string-or-list, with the
// same refusals.
func (p *Push) decodeTOML(v any) error {
	const noVariant = "data did not match any variant of untagged enum PushText"
	switch v := v.(type) {
	case string:
		switch word := trim(v); word {
		case "anywhere":
			*p = Push{}
		case "selfhosted":
			*p = Push{Kind: pushSelfhosted}
		case "nowhere":
			*p = Push{Kind: pushNowhere}
		default:
			return errf("push = \"%s\" is not a rule: use \"selfhosted\", \"nowhere\", \"anywhere\" or a list of allowed remote URL prefixes", word)
		}
	case []any:
		list := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return errf(noVariant)
			}
			list = append(list, s)
		}
		empty := len(list) == 0
		for _, prefix := range list {
			empty = empty || trim(prefix) == ""
		}
		if empty {
			return errf("push = [] allows nothing; say push = \"nowhere\" for that")
		}
		*p = Push{Kind: pushRemotes, Remotes: list}
	default:
		return errf(noVariant)
	}
	return nil
}

func (p Push) describe(selfhosted []string) string {
	switch p.Kind {
	case pushNowhere:
		return "nowhere"
	case pushSelfhosted:
		if len(selfhosted) == 0 {
			return "self-hosted only (no hosts listed: local paths only)"
		}
		return "self-hosted hosts only: " + strings.Join(selfhosted, ", ")
	case pushRemotes:
		return "only " + strings.Join(p.Remotes, ", ")
	}
	return "anywhere"
}

func (p Push) equal(o Push) bool {
	return p.Kind == o.Kind && rulesSameList(p.Remotes, o.Remotes)
}

// RepoRule is one repository's rule.
type RepoRule struct {
	// A name for people; matching does not use it.
	Name string `json:"name" toml:",required"`
	// The repository's root commit: its identity.
	FirstCommit *string `json:"first_commit,omitempty"`
	// Remote URLs the repository is known by: a hint when the first commit
	// is not known, or the history was rewritten.
	Remotes []string `json:"remotes,omitempty"`
	Push    Push     `json:"push,omitempty"`
	// Branches that take no direct push: they are reached only through a
	// pull request.
	Protected []string `json:"protected,omitempty"`
	// The owner's reason, shown with a refusal.
	Note string `json:"note,omitempty"`
}

func (*RepoRule) denyUnknownTOML() {}

func (r RepoRule) equal(o RepoRule) bool {
	sameFirst := (r.FirstCommit == nil) == (o.FirstCommit == nil) && (r.FirstCommit == nil || *r.FirstCommit == *o.FirstCommit)
	return r.Name == o.Name && sameFirst && rulesSameList(r.Remotes, o.Remotes) && r.Push.equal(o.Push) &&
		rulesSameList(r.Protected, o.Protected) && r.Note == o.Note
}

// RulesFile is the owner's rules file.
type RulesFile struct {
	// Hosts that count as self-hosted.
	Selfhosted []string `json:"selfhosted,omitempty"`
	// Words that never go in a pushed commit (names, phone numbers).
	Personal []string `json:"personal,omitempty"`
	// Private mail addresses that do belong in commits.
	Allow []string `json:"allow,omitempty"`
	// Reminders about work left lying around (`gitdcy rules scan`, the push
	// check): on when this table is there.
	Hygiene *HygieneRules `json:"hygiene,omitempty"`
	Repos   []RepoRule    `json:"repo,omitempty"`
}

func (*RulesFile) denyUnknownTOML() {}

func (f *RulesFile) equal(o *RulesFile) bool {
	if f == nil || o == nil {
		return f == o
	}
	sameHygiene := (f.Hygiene == nil) == (o.Hygiene == nil) &&
		(f.Hygiene == nil || f.Hygiene.BranchDays == o.Hygiene.BranchDays && rulesSameList(f.Hygiene.Skip, o.Hygiene.Skip))
	if !sameHygiene || len(f.Repos) != len(o.Repos) || !rulesSameList(f.Selfhosted, o.Selfhosted) ||
		!rulesSameList(f.Personal, o.Personal) || !rulesSameList(f.Allow, o.Allow) {
		return false
	}
	for i := range f.Repos {
		if !f.Repos[i].equal(o.Repos[i]) {
			return false
		}
	}
	return true
}

// rulesSameList compares two lists as Rust's Vec did: an empty one equals
// an empty one.
func rulesSameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rulesParse reads a rules file's text. A [hygiene] table without
// branch_days gets its default, 3 (serde's `default = fn` for the field).
func rulesParse(text string) (RulesFile, error) {
	var file RulesFile
	if err := tomlUnmarshal(text, &file); err != nil {
		return file, err
	}
	if file.Hygiene != nil {
		var given struct {
			Hygiene *struct {
				BranchDays *uint64 `json:"branch_days"`
			} `json:"hygiene"`
		}
		if tomlUnmarshal(text, &given) == nil && given.Hygiene != nil && given.Hygiene.BranchDays == nil {
			file.Hygiene.BranchDays = hygDefaultBranchDays
		}
	}
	return file, nil
}

func (f *RulesFile) validate() error {
	if f.Hygiene != nil {
		if err := f.Hygiene.validate(); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, rule := range f.Repos {
		name := trim(rule.Name)
		if name == "" {
			return errf("a [[repo]] has no name")
		}
		if names[name] {
			return errf("two rules are named %s; each repository has one rule", name)
		}
		names[name] = true
		if rule.FirstCommit != nil {
			first := *rule.FirstCommit
			if !rulesAllHex(first) || !(len(first) == 40 || len(first) == 64) {
				return errf("rule %s: first_commit = \"%s\" is not a full commit hash (git rev-list --max-parents=0 HEAD shows it)", name, first)
			}
		}
		if rule.FirstCommit == nil && len(rule.Remotes) == 0 {
			return errf("rule %s has neither first_commit nor remotes, so it matches no repository", name)
		}
		for _, branch := range rule.Protected {
			if trim(branch) == "" {
				return errf("rule %s: an empty name in protected", name)
			}
		}
	}
	return nil
}

func rulesAllHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// readRules reads a rules file; nil when it is not there.
func readRules(path string) (*RulesFile, error) {
	text, err := readText(path)
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, wrapf(err, "reading the rules file %s", path)
	}
	file, err := rulesParse(text)
	if err == nil {
		err = file.validate()
	}
	if err != nil {
		return nil, wrapf(err, "the rules file %s is not valid", path)
	}
	return &file, nil
}

// rulesFilePath is the rules file: the one given, else `rules_file` in this
// device's own local.toml, else rules.toml in GitDCY's config folder. The
// setting is read from the device's config only, never from a file in the
// current folder: a repository could carry one.
func rulesFilePath(explicit *string) (string, error) {
	if explicit != nil {
		return expandHome(*explicit), nil
	}
	config := loadSavedLocalConfig()
	if config.ConfigError != nil {
		return "", errf("%s", *config.ConfigError)
	}
	if config.RulesFile != nil {
		return expandHome(*config.RulesFile), nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return joinPath(dir, rulesFileName), nil
}

// ---------------------------------------------------------------------------
// Remote URLs

// rulesHostOf is the host of a remote URL, lowercase, without user and
// port; false for a local path.
func rulesHostOf(url string) (string, bool) {
	if isLocal(url) {
		return "", false
	}
	host, _, _ := strings.Cut(normalizeURL(url), "/")
	return host, host != ""
}

// urlHasPrefix reports whether prefix matches the URL itself or what lies
// under it (`github.com/org` matches `github.com/org/repo`, not
// `github.com/org-other/repo`).
func urlHasPrefix(url, prefix string) bool {
	// A prefix is written without a scheme (`github.com/Org/`), which on its
	// own reads as a local path and would keep its capitals: read it as the
	// remote it names, unless it is one (`/`, `.`, `~`).
	prefix = trim(prefix)
	if isLocal(prefix) && !strings.HasPrefix(prefix, "/") && !strings.HasPrefix(prefix, ".") && !strings.HasPrefix(prefix, "~") {
		prefix = normalizeURL("https://" + prefix)
	} else {
		prefix = normalizeURL(prefix)
	}
	url = normalizeURL(url)
	return prefix != "" && (url == prefix || strings.HasPrefix(url, prefix) && strings.HasPrefix(url[len(prefix):], "/"))
}

// sshHostname is what an SSH alias stands for, from the user's own ssh
// config (`ssh -G`).
func sshHostname(alias string) (string, bool) {
	safe := alias != "" && !strings.HasPrefix(alias, "-")
	for _, c := range alias {
		safe = safe && (isASCIIAlnum(c) || strings.ContainsRune(".-_", c))
	}
	if !safe {
		return "", false
	}
	out, err := runOutput(exec.Command("ssh", "-G", alias))
	if err != nil {
		return "", false
	}
	for _, line := range lines(lossy(out.Stdout)) {
		if host, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.ToLower(trim(host)), true
		}
	}
	return "", false
}

// remoteAllowed reports whether push lets a push go to url. resolve turns
// an SSH alias into the host it stands for.
func remoteAllowed(push Push, url string, selfhosted []string, resolve func(string) (string, bool)) bool {
	switch push.Kind {
	case pushNowhere:
		return false
	case pushRemotes:
		for _, prefix := range push.Remotes {
			if urlHasPrefix(url, prefix) {
				return true
			}
		}
		return false
	case pushSelfhosted:
		host, ok := rulesHostOf(url)
		if !ok {
			return true // a path on this device
		}
		listed := map[string]bool{}
		for _, h := range selfhosted {
			if name, ok := rulesHostOf(h); ok {
				listed[name] = true
			} else {
				listed[strings.ToLower(trim(h))] = true
			}
		}
		for _, h := range rulesLocalHosts {
			listed[h] = true
		}
		if listed[host] {
			return true
		}
		lower := strings.ToLower(trim(url))
		ssh := !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "git://")
		if ssh {
			real, ok := resolve(host)
			return ok && listed[real]
		}
		return false
	}
	return true
}

// protectedIn is the protected branches among branches, sorted, each once.
func protectedIn(rule RepoRule, branches []string) []string {
	var out []string
	for _, b := range branches {
		for _, p := range rule.Protected {
			if trim(p) == b {
				out = append(out, b)
				break
			}
		}
	}
	return sortDedup(out)
}

// ---------------------------------------------------------------------------
// The repository

type rulesRemote struct {
	name string
	// Its URLs (push and fetch), as configured.
	urls []string
}

type rulesRepo struct {
	root string
	// The git folder shared by all worktrees: where hooks and the copy live.
	gitDir       string
	firstCommits []string
	remotes      []rulesRemote
}

func rulesAbsolute(base, path string) string {
	path = trim(path)
	if filepath.IsAbs(path) {
		return path
	}
	return joinPath(base, path)
}

// rulesParent is Rust's Path::parent, "" when there is none.
func rulesParent(p string) string {
	end := len(p)
	for end > 1 && isSep(p[end-1]) {
		end--
	}
	i := end - 1
	for i >= 0 && !isSep(p[i]) {
		i--
	}
	if i < 0 {
		return ""
	}
	dir := p[:i]
	for len(dir) > 1 && isSep(dir[len(dir)-1]) {
		dir = dir[:len(dir)-1]
	}
	if dir == "" {
		if end == 1 {
			return "" // the root has no parent
		}
		return p[:1]
	}
	return dir
}

func rulesIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func rulesOpen(path string) (*rulesRepo, error) {
	dir := path
	if !rulesIsDir(path) {
		dir = rulesParent(path)
	}
	out, err := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		out, err = gitOutput(dir, "rev-parse", "--absolute-git-dir")
	}
	if err != nil {
		return nil, wrapf(err, "%s is not inside a git repository", path)
	}
	repo := &rulesRepo{gitDir: rulesAbsolute(dir, out)}
	// A bare repository (a mirror) has no work tree; its git folder is the
	// repository.
	repo.root = repo.gitDir
	if top, err := gitOutput(dir, "rev-parse", "--show-toplevel"); err == nil && trim(top) != "" {
		repo.root = trim(top)
	}
	commits, _ := gitOutput(repo.root, "rev-list", "--max-parents=0", "--all")
	for _, line := range lines(commits) {
		if c := strings.ToLower(trim(line)); c != "" {
			repo.firstCommits = append(repo.firstCommits, c)
		}
	}
	repo.firstCommits = sortDedup(repo.firstCommits)
	names, _ := gitOutput(repo.root, "remote")
	for _, name := range lines(names) {
		name = trim(name)
		if name == "" {
			continue
		}
		remote := rulesRemote{name: name}
		for _, args := range [][]string{
			{"remote", "get-url", "--push", "--all", name},
			{"remote", "get-url", "--all", name},
		} {
			out, err := gitOutput(repo.root, args...)
			if err != nil {
				continue
			}
			for _, url := range lines(out) {
				if url = trim(url); url != "" && !contains(remote.urls, url) {
					remote.urls = append(remote.urls, url)
				}
			}
		}
		repo.remotes = append(repo.remotes, remote)
	}
	return repo, nil
}

func rulesPushURL(repo *rulesRepo, remote string) (string, bool) {
	out, err := gitOutput(repo.root, "remote", "get-url", "--push", remote)
	if err != nil {
		return "", false
	}
	first := lines(out)
	if len(first) == 0 || trim(first[0]) == "" {
		return "", false
	}
	return trim(first[0]), true
}

func rulesShort(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func rulesHasFirst(repo *rulesRepo, first string) bool {
	for _, c := range repo.firstCommits {
		if asciiLower(c) == asciiLower(first) {
			return true
		}
	}
	return false
}

// rulesMatches is why rule is this repository's rule; empty when it is not.
func rulesMatches(rule RepoRule, repo *rulesRepo) []string {
	var why []string
	if rule.FirstCommit != nil && rulesHasFirst(repo, *rule.FirstCommit) {
		why = append(why, "first commit "+rulesShort(*rule.FirstCommit))
	}
	for _, hint := range rule.Remotes {
		hint = normalizeURL(hint)
		for _, remote := range repo.remotes {
			for _, u := range remote.urls {
				if normalizeURL(u) == hint {
					why = append(why, fmt.Sprintf("remote %s (%s)", remote.name, hint))
					break
				}
			}
		}
	}
	return why
}

// ---------------------------------------------------------------------------
// Which rule applies

// RuleSource is where a repository's rule was found.
type RuleSource string

const (
	sourceRulesFile     RuleSource = "rules-file"
	sourceInstalledCopy RuleSource = "installed-copy"
)

type rulesFound struct {
	repo      *rulesRepo
	rulesPath string
	file      *RulesFile
	copyPath  string
	copy      *RulesFile
	rule      *RepoRule
	source    *RuleSource
	matchedBy []string
	// Two or more rules in the rules file claim this repository.
	conflicts  []string
	selfhosted []string
}

func (f *rulesFound) from(source RuleSource) bool { return f.source != nil && *f.source == source }

func (f *rulesFound) sourceText() string {
	if f.from(sourceInstalledCopy) {
		return f.copyPath + " (a copy of the owner's rules file)"
	}
	return "the rules file " + f.rulesPath
}

func (f *rulesFound) scanConfig() personalConfig {
	if f.file == nil {
		return personalConfig{}
	}
	return personalConfig{words: f.file.Personal, allow: f.file.Allow}
}

// expectedCopy is what `install` writes as the copy, when there is a rule.
func (f *rulesFound) expectedCopy() *RulesFile {
	if f.rule == nil {
		return nil
	}
	return &RulesFile{Selfhosted: f.selfhosted, Repos: []RepoRule{*f.rule}}
}

func rulesFind(rulesPath, repoPath string) (*rulesFound, error) {
	repo, err := rulesOpen(repoPath)
	if err != nil {
		return nil, err
	}
	file, err := readRules(rulesPath)
	if err != nil {
		return nil, err
	}
	copyPath := joinPath(repo.gitDir, rulesCopyName)
	copy, err := readRules(copyPath)
	if err != nil {
		return nil, err
	}
	if copy != nil && len(copy.Repos) != 1 {
		return nil, errf("%s holds %d rules; it is one repository's copy and holds one (gitdcy rules install writes it)", copyPath, len(copy.Repos))
	}
	type match struct {
		rule RepoRule
		why  []string
	}
	var matching []match
	if file != nil {
		for _, rule := range file.Repos {
			if why := rulesMatches(rule, repo); len(why) > 0 {
				matching = append(matching, match{rule, why})
			}
		}
	}
	found := &rulesFound{repo: repo, rulesPath: rulesPath, copyPath: copyPath}
	switch {
	case len(matching) > 1:
		for _, m := range matching {
			found.conflicts = append(found.conflicts, m.rule.Name)
			found.matchedBy = append(found.matchedBy, m.rule.Name+": "+strings.Join(m.why, ", "))
		}
	case len(matching) == 1:
		rule, source := matching[0].rule, sourceRulesFile
		found.rule, found.source, found.matchedBy = &rule, &source, matching[0].why
		found.selfhosted = file.Selfhosted
	case copy != nil:
		rule, source := copy.Repos[0], sourceInstalledCopy
		found.rule, found.source = &rule, &source
		found.matchedBy = []string{"the copy in this repository's git folder"}
		found.selfhosted = copy.Selfhosted
	}
	found.file, found.copy = file, copy
	return found, nil
}

// ---------------------------------------------------------------------------
// The hook

func rulesShQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// rulesHookText is the pre-push hook: the check, then the hook that was
// there before ("" for none). gitdcy is this program's path ("" when not
// known), the fallback when gitdcy is not on the PATH.
func rulesHookText(chained, gitdcy string) string {
	fallback := ""
	if gitdcy != "" {
		fallback = "command -v gitdcy >/dev/null 2>&1 || gitdcy=" + rulesShQuote(gitdcy) + "\n"
	}
	return "#!/bin/sh\n" +
		rulesHookMark + `, this repository's rule (the owner's
# rules file, or the copy gitdcy-rules.toml in the git folder) and the
# personal-data check. Written by gitdcy rules install; run that again to update.
gitdcy=gitdcy
` + fallback + "chained=" + rulesShQuote(chained) + `
input=$(cat)
if ! command -v "$gitdcy" >/dev/null 2>&1; then
  echo "gitdcy rules: this repository has a push rule, and gitdcy is not installed here to check it; install gitdcy, then push again" >&2
  exit 1
fi
printf '%s\n' "$input" | "$gitdcy" rules check --stdin "$1" "$2" || exit 1
if [ -n "$chained" ] && [ -x "$(dirname "$0")/$chained" ]; then
  printf '%s\n' "$input" | "$(dirname "$0")/$chained" "$@" || exit 1
fi
exit 0
`
}

// rulesChainedOf is the hook a gitdcy hook chains, as written in it; ""
// for none.
func rulesChainedOf(text string) string {
	for _, line := range lines(text) {
		if rest, ok := strings.CutPrefix(line, "chained='"); ok {
			name, ok := strings.CutSuffix(rest, "'")
			if !ok {
				return ""
			}
			return name
		}
	}
	return ""
}

// HookState is a repository's pre-push hook.
type HookState struct {
	// .git/hooks/pre-push.
	Path string `json:"path"`
	// It is gitdcy's hook.
	Installed bool `json:"installed"`
	// It is the hook this gitdcy writes.
	Current bool `json:"current"`
	// git runs it on a push: no core.hooksPath points elsewhere, or the
	// hook there calls this one.
	Runs bool `json:"runs"`
	// Where git runs hooks from, when core.hooksPath moves them.
	HooksPath *string `json:"hooks_path,omitempty"`
}

// rulesSameDir compares two folders as Rust compared canonical paths.
func rulesSameDir(a, b string) bool {
	if c, err := canonicalize(a); err == nil {
		a = c
	}
	if c, err := canonicalize(b); err == nil {
		b = c
	}
	return filepath.IsAbs(a) == filepath.IsAbs(b) && rulesSameList(components(a), components(b))
}

// rulesHooksElsewhere is the other folder git runs hooks from
// (core.hooksPath), if any, and whether its pre-push calls the
// repository's own.
func rulesHooksElsewhere(repo *rulesRepo) (dir string, chains, elsewhere bool) {
	def := joinPath(repo.gitDir, "hooks")
	effective := def
	if out, err := gitOutput(repo.root, "rev-parse", "--path-format=absolute", "--git-path", "hooks"); err == nil {
		effective = rulesAbsolute(repo.root, out)
	}
	if rulesSameDir(effective, def) {
		return "", false, false
	}
	text, err := readText(joinPath(effective, "pre-push"))
	return effective, err == nil && strings.Contains(text, "/hooks/pre-push"), true
}

func rulesHookState(repo *rulesRepo, gitdcy string) HookState {
	path := joinPath(joinPath(repo.gitDir, "hooks"), "pre-push")
	text, _ := readText(path)
	installed := strings.Contains(text, rulesHookMark)
	dir, chains, elsewhere := rulesHooksElsewhere(repo)
	state := HookState{
		Path:      path,
		Installed: installed,
		Current:   installed && text == rulesHookText(rulesChainedOf(text), gitdcy),
		Runs:      !elsewhere || chains,
	}
	if elsewhere {
		state.HooksPath = &dir
	}
	return state
}

// ---------------------------------------------------------------------------
// `gitdcy rules`: which rule applies and why

// RemoteState is a remote and whether the rule lets a push go there.
type RemoteState struct {
	Name string `json:"name"`
	// Shown without user, password or port.
	URL     *string `json:"url"`
	Allowed bool    `json:"allowed"`
}

// CopyState is the copy of the rule in the git folder.
type CopyState struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	// The copy holds what the rules file says now; null without a rule.
	Current *bool `json:"current"`
}

// RulesReport is which rule applies to a repository, and why.
type RulesReport struct {
	Repository     string      `json:"repository"`
	RulesFile      string      `json:"rules_file"`
	RulesFileFound bool        `json:"rules_file_found"`
	FirstCommits   []string    `json:"first_commits"`
	Rule           *RepoRule   `json:"rule"`
	Source         *RuleSource `json:"source"`
	MatchedBy      []string    `json:"matched_by"`
	// Rules that all match this repository: its pushes are refused until
	// one is left.
	Conflicts []string `json:"conflicts"`
	// Where the rule lets it go, in words.
	Push       *string       `json:"push"`
	Selfhosted []string      `json:"selfhosted"`
	Remotes    []RemoteState `json:"remotes"`
	Copy       CopyState     `json:"copy"`
	Hook       HookState     `json:"hook"`
	// How many personal words and allowed addresses the check reads (the
	// words themselves are not shown).
	PersonalWords    int      `json:"personal_words"`
	AllowedAddresses int      `json:"allowed_addresses"`
	Warnings         []string `json:"warnings"`
}

func rulesReport(rulesPath, repoPath, gitdcy string) (*RulesReport, error) {
	found, err := rulesFind(rulesPath, repoPath)
	if err != nil {
		return nil, err
	}
	hook := rulesHookState(found.repo, gitdcy)
	scan := found.scanConfig()
	var copyCurrent *bool
	if expected := found.expectedCopy(); expected != nil {
		current := found.copy != nil && found.copy.equal(expected)
		copyCurrent = &current
	}
	var warnings []string
	if found.file == nil {
		if found.from(sourceInstalledCopy) {
			warnings = append(warnings, fmt.Sprintf("the rules file %s is not on this device; the copy in the git folder holds", found.rulesPath))
		} else {
			warnings = append(warnings, fmt.Sprintf("no rules file at %s (rules_file in GitDCY's local.toml names another place)", found.rulesPath))
		}
	} else if found.from(sourceInstalledCopy) {
		warnings = append(warnings, fmt.Sprintf("the rules file %s has no rule for this repository; the copy in the git folder still holds", found.rulesPath))
	}
	if len(found.conflicts) > 0 {
		warnings = append(warnings, fmt.Sprintf("rules %s all match this repository; keep one in the rules file", strings.Join(found.conflicts, ", ")))
	}
	if rule := found.rule; rule != nil {
		if rule.FirstCommit != nil && found.from(sourceRulesFile) && !rulesHasFirst(found.repo, *rule.FirstCommit) {
			warnings = append(warnings, fmt.Sprintf("%s matched by its remote only: its first_commit %s is not in this history (rewritten, or a shallow clone?)", rule.Name, rulesShort(*rule.FirstCommit)))
		}
		if rule.Push.Kind == pushSelfhosted && len(found.selfhosted) == 0 {
			warnings = append(warnings, "push = \"selfhosted\" and no self-hosted hosts are listed: only local paths are allowed")
		}
		if copyCurrent == nil || !*copyCurrent {
			if found.copy == nil {
				warnings = append(warnings, "the rule is not installed in this checkout: gitdcy rules install")
			} else {
				warnings = append(warnings, "the copy in the git folder differs from the rules file: gitdcy rules install")
			}
		}
		if !hook.Installed {
			warnings = append(warnings, "no gitdcy pre-push hook here: gitdcy rules install")
		} else if !hook.Current {
			warnings = append(warnings, "the pre-push hook is an older one: gitdcy rules install")
		}
		if !hook.Runs {
			hooksPath := ""
			if hook.HooksPath != nil {
				hooksPath = *hook.HooksPath
			}
			warnings = append(warnings, fmt.Sprintf("git runs hooks from %s (core.hooksPath), and the pre-push there does not call this repository's own, so the rule is not checked on a push", hooksPath))
		}
	}
	var remotes []RemoteState
	for _, remote := range found.repo.remotes {
		state := RemoteState{Name: remote.name, Allowed: len(found.conflicts) == 0}
		if url, ok := rulesPushURL(found.repo, remote.name); ok {
			shown := normalizeURL(url)
			state.URL = &shown
			if found.rule != nil {
				state.Allowed = remoteAllowed(found.rule.Push, url, found.selfhosted, sshHostname)
			}
		}
		remotes = append(remotes, state)
	}
	report := &RulesReport{
		Repository:       found.repo.root,
		RulesFile:        found.rulesPath,
		RulesFileFound:   found.file != nil,
		FirstCommits:     found.repo.firstCommits,
		Rule:             found.rule,
		Source:           found.source,
		MatchedBy:        found.matchedBy,
		Conflicts:        found.conflicts,
		Selfhosted:       found.selfhosted,
		Remotes:          remotes,
		Copy:             CopyState{Path: found.copyPath, Present: found.copy != nil, Current: copyCurrent},
		Hook:             hook,
		PersonalWords:    len(scan.words),
		AllowedAddresses: len(scan.allow),
		Warnings:         warnings,
	}
	if found.rule != nil {
		push := found.rule.Push.describe(found.selfhosted)
		report.Push = &push
	}
	return report, nil
}

// ---------------------------------------------------------------------------
// `gitdcy rules install`

// InstallReport is what `install` wrote.
type InstallReport struct {
	Repository  string `json:"repository"`
	Rule        string `json:"rule"`
	Copy        string `json:"copy"`
	CopyWritten bool   `json:"copy_written"`
	Hook        string `json:"hook"`
	HookWritten bool   `json:"hook_written"`
	// Files moved aside before they were replaced.
	Kept []string `json:"kept"`
	// The hook that was there before; it now runs after the check.
	Chained *string `json:"chained"`
}

func rulesCopyText(copy *RulesFile, rulesPath string) (string, error) {
	text, err := tomlMarshal(copy)
	if err != nil {
		return "", err
	}
	return "# This repository's rule, copied from " + rulesPath + " by gitdcy rules install.\n" +
		"# The rule is the owner's: change it in the rules file, then run\n" +
		"# gitdcy rules install again.\n\n" + text, nil
}

func rulesThere(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// rulesInstall writes the copy of this repository's rule and the pre-push
// hook into its git folder. A hook that is there already is moved aside
// and chained; when git would not run the hook (a core.hooksPath
// elsewhere), nothing is written. Safe to run twice.
func rulesInstall(rulesPath, repoPath, gitdcy string) (*InstallReport, error) {
	found, err := rulesFind(rulesPath, repoPath)
	if err != nil {
		return nil, err
	}
	if len(found.conflicts) > 0 {
		return nil, errf("rules %s in %s all match this repository (%s); keep one for it, then run gitdcy rules install again",
			strings.Join(found.conflicts, ", "), found.rulesPath, strings.Join(found.matchedBy, "; "))
	}
	rule, expected := found.rule, found.expectedCopy()
	if rule == nil {
		first := "(no commits yet)"
		if len(found.repo.firstCommits) > 0 {
			first = found.repo.firstCommits[0]
		}
		return nil, errf("%s has no rule in %s, so there is nothing to install. To give it one, add to that file:\n\n[[repo]]\nname = \"NAME\"\nfirst_commit = \"%s\"\npush = \"selfhosted\"   # or \"nowhere\", or [\"host/org/\"]\n\nthen run gitdcy rules install again",
			found.repo.root, found.rulesPath, first)
	}
	hooks := joinPath(found.repo.gitDir, "hooks")
	if dir, chains, elsewhere := rulesHooksElsewhere(found.repo); elsewhere && !chains {
		return nil, errf("git runs this repository's hooks from %s (core.hooksPath), so a hook in %s would not run, and nothing was written. Make %s/pre-push call .git/hooks/pre-push with the same arguments and input, or remove core.hooksPath from the git config that sets it (git config --show-origin core.hooksPath shows which); then run gitdcy rules install again",
			dir, hooks, dir)
	}
	hook := joinPath(hooks, "pre-push")
	if rulesIsDir(hook) {
		return nil, errf("%s is a folder, not a hook; nothing was written", hook)
	}
	var kept []string

	copyWritten := !found.copy.equal(expected)
	if copyWritten {
		if rulesThere(found.copyPath) {
			aside, err := keepAside(found.copyPath)
			if err != nil {
				return nil, err
			}
			kept = append(kept, aside)
		}
		text, err := rulesCopyText(expected, found.rulesPath)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(found.copyPath, []byte(text), 0o666); err != nil {
			return nil, wrapf(err, "writing %s", found.copyPath)
		}
	}

	if err := os.MkdirAll(hooks, 0o777); err != nil {
		return nil, wrapf(err, "creating %s", hooks)
	}
	var existing *string
	if rulesThere(hook) {
		data, _ := os.ReadFile(hook)
		text := lossy(data)
		existing = &text
	}
	chained := ""
	if existing != nil && strings.Contains(*existing, rulesHookMark) {
		chained = rulesChainedOf(*existing)
	}
	hookWritten := existing == nil || *existing != rulesHookText(chained, gitdcy)
	if hookWritten {
		if existing != nil {
			aside, err := keepAside(hook)
			if err != nil {
				return nil, err
			}
			if !strings.Contains(*existing, rulesHookMark) {
				chained, _ = fileName(aside)
			}
			kept = append(kept, aside)
		}
		if err := os.WriteFile(hook, []byte(rulesHookText(chained, gitdcy)), 0o666); err != nil {
			return nil, wrapf(err, "writing %s", hook)
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(hook, 0o755); err != nil {
				return nil, ioError(err)
			}
		}
	}
	report := &InstallReport{
		Repository:  found.repo.root,
		Rule:        rule.Name,
		Copy:        found.copyPath,
		CopyWritten: copyWritten,
		Hook:        hook,
		HookWritten: hookWritten,
		Kept:        kept,
	}
	if chained != "" {
		report.Chained = &chained
	}
	return report, nil
}

// ---------------------------------------------------------------------------
// `gitdcy rules check`: what the hook runs

// PushedRef is one line of what git gives a pre-push hook on its input.
type PushedRef struct {
	LocalRef, LocalSha, RemoteRef, RemoteSha string
}

func rulesIsSha(text string) bool {
	return (len(text) == 40 || len(text) == 64) && rulesAllHex(text)
}

// parsePushedRefs reads `<local ref> <local sha> <remote ref> <remote sha>`
// per line. A line that is not that is an error, not skipped: what is not
// read is not checked.
func parsePushedRefs(text string) ([]PushedRef, error) {
	var refs []PushedRef
	for _, line := range lines(text) {
		if trim(line) == "" {
			continue
		}
		words := strings.Fields(line)
		if len(words) != 4 || !rulesIsSha(words[1]) || !rulesIsSha(words[3]) {
			return nil, errf("not a line git gives a pre-push hook: %s", line)
		}
		refs = append(refs, PushedRef{LocalRef: words[0], LocalSha: words[1], RemoteRef: words[2], RemoteSha: words[3]})
	}
	return refs, nil
}

// CheckRequest is what `rules check` was asked.
type CheckRequest struct {
	// The remote's name (as git passes it, which is the URL for a push to a
	// URL); nil for origin.
	Remote *string
	URL    *string
	// What the hook got on its input; HasRefs false checks the current
	// branch.
	Refs    []PushedRef
	HasRefs bool
}

// CheckReport is what a push would be refused for.
type CheckReport struct {
	Repository string  `json:"repository"`
	Remote     *string `json:"remote"`
	// Shown without user, password or port.
	URL           *string     `json:"url"`
	Rule          *string     `json:"rule"`
	Source        *RuleSource `json:"source"`
	Push          *string     `json:"push"`
	RemoteAllowed bool        `json:"remote_allowed"`
	// Protected branches this push would write to (or delete) directly.
	ProtectedBranches []string  `json:"protected_branches"`
	Range             string    `json:"range"`
	Commits           int       `json:"commits"`
	Findings          []Finding `json:"findings"`
	Refused           bool      `json:"refused"`
	// What was refused and the way forward, one line each.
	Reasons []string `json:"reasons"`
	// Work left lying around in the repository (the rules file's
	// [hygiene]): said before the push, never a reason to refuse it.
	Reminders []Reminder `json:"reminders"`
}

// rulesHookGHTimeout is how long the push check waits for GitHub's open
// pull requests.
const rulesHookGHTimeout = 5 * time.Second

// rulesPushReminders is the reminders for a push of pushing (commit ids)
// from this repository.
func rulesPushReminders(found *rulesFound, pushing []string) []Reminder {
	if found.file == nil || found.file.Hygiene == nil {
		return nil
	}
	rules := found.file.Hygiene
	var urls []string
	for _, remote := range found.repo.remotes {
		urls = append(urls, remote.urls...)
	}
	if _, skipped := rules.skippedBy(urls); skipped {
		return nil
	}
	github := hygUnknown("not on GitHub")
	if place, ok := githubRepository(urls); ok {
		github = hygAsk("gh", nil, []string{place}, rulesHookGHTimeout)
	}
	return hygRead(found.repo.root, *rules, urls, github, pushing, true).Reminders
}

func rulesIsZero(sha string) bool {
	return sha != "" && strings.Trim(sha, "0") == ""
}

func rulesOnNoRemote(local string) []string {
	return []string{local, "--not", "--remotes"}
}

func rulesCheck(rulesPath, repoPath string, request CheckRequest) (*CheckReport, error) {
	found, err := rulesFind(rulesPath, repoPath)
	if err != nil {
		return nil, err
	}
	repo := found.repo
	remote := "origin"
	if request.Remote != nil {
		remote = *request.Remote
	}
	if strings.HasPrefix(remote, "-") {
		return nil, errf("%s is not a remote name", remote)
	}
	url := request.URL
	if url == nil {
		if u, ok := rulesPushURL(repo, remote); ok {
			url = &u
		}
	}
	scan := found.scanConfig()

	var ranges [][]string
	var branches []string
	if request.HasRefs {
		for _, pushed := range request.Refs {
			if branch, ok := strings.CutPrefix(pushed.RemoteRef, "refs/heads/"); ok {
				// A deletion writes to the branch too.
				branches = append(branches, branch)
			}
			if rulesIsZero(pushed.LocalSha) {
				continue // a deletion carries no commits
			}
			if rulesIsZero(pushed.RemoteSha) {
				ranges = append(ranges, rulesOnNoRemote(pushed.LocalSha))
			} else {
				ranges = append(ranges, []string{pushed.RemoteSha + ".." + pushed.LocalSha})
			}
		}
	} else {
		ranges = append(ranges, personalDefaultRange(repo.root))
		if out, err := gitOutput(repo.root, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
			if branch := trim(out); branch != "" {
				branches = append(branches, branch)
			}
		}
	}

	commits := 0
	var findings []Finding
	for i := range ranges {
		n, more, err := personalScanCommits(repo.root, ranges[i], scan)
		if err != nil {
			if len(ranges[i]) != 1 || !strings.Contains(ranges[i][0], "..") {
				return nil, err
			}
			// The remote's commit is not here (not fetched): read what is on
			// no remote.
			local := ranges[i][0][strings.LastIndex(ranges[i][0], "..")+2:]
			ranges[i] = rulesOnNoRemote(local)
			if n, more, err = personalScanCommits(repo.root, ranges[i], scan); err != nil {
				return nil, err
			}
		}
		commits += n
		findings = append(findings, more...)
	}

	// No remote: nothing leaves, so there is nothing to refuse; the rule
	// applies once one is added.
	remoteOK, protected := true, []string(nil)
	if found.rule != nil && url != nil {
		remoteOK = remoteAllowed(found.rule.Push, *url, found.selfhosted, sshHostname)
		protected = protectedIn(*found.rule, branches)
	}

	var shownURL *string
	if url != nil {
		shown := normalizeURL(*url)
		shownURL = &shown
	}
	var reasons []string
	if len(found.conflicts) > 0 {
		reasons = append(reasons, fmt.Sprintf("Rules %s in %s all match this repository (%s), so its pushes are refused until one is left. Keep one rule for it in the rules file.",
			strings.Join(found.conflicts, ", "), found.rulesPath, strings.Join(found.matchedBy, "; ")))
	}
	if rule := found.rule; rule != nil {
		target := remote
		if shownURL != nil {
			target = *shownURL
		}
		if !remoteOK {
			var line string
			switch rule.Push.Kind {
			case pushNowhere:
				line = fmt.Sprintf("\"%s\" is not pushed anywhere, and %s is no exception.", rule.Name, target)
			case pushSelfhosted:
				hosts := "none listed: local paths only"
				if len(found.selfhosted) > 0 {
					hosts = strings.Join(found.selfhosted, ", ")
				}
				line = fmt.Sprintf("\"%s\" may only be pushed to self-hosted hosts (%s); %s is not one of them. Push to a remote on one of those hosts instead (git remote -v lists this repository's remotes).", rule.Name, hosts, target)
			case pushRemotes:
				line = fmt.Sprintf("\"%s\" may only be pushed to %s; %s is not one of them. Push to one of those instead (git remote -v lists this repository's remotes).", rule.Name, strings.Join(rule.Push.Remotes, ", "), target)
			}
			if note := trim(rule.Note); note != "" {
				line += " The owner's reason: " + note
				if !strings.HasSuffix(line, ".") {
					line += "."
				}
			}
			line += fmt.Sprintf(" Where it may go is the owner's decision, set in %s.", found.sourceText())
			reasons = append(reasons, line)
		}
		for _, branch := range protected {
			reasons = append(reasons, fmt.Sprintf("Branch %s of \"%s\" takes no direct push: it is reached only through a pull request. Push a branch of your own (git push %s HEAD:refs/heads/NEW-BRANCH) and open a pull request into %s.", branch, rule.Name, remote, branch))
		}
	}
	if len(findings) > 0 {
		reasons = append(reasons, fmt.Sprintf("The commits being pushed hold personal data in %d place(s), listed above. Take it out of those commits (git commit --amend for the last one, git rebase for older ones) and push again. Whether an address belongs in this repository is the owner's decision; its place is the allow list in the rules file.", len(findings)))
	}
	refused := len(reasons) > 0
	if refused {
		reasons = append(reasons, rulesQuestion)
	}
	var pushing []string
	if request.HasRefs {
		for _, pushed := range request.Refs {
			if !rulesIsZero(pushed.LocalSha) {
				pushing = append(pushing, pushed.LocalSha)
			}
		}
	} else if head, err := gitOutput(repo.root, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		pushing = []string{trim(head)}
	}
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = strings.Join(r, " ")
	}
	report := &CheckReport{
		Repository:        repo.root,
		Remote:            &remote,
		URL:               shownURL,
		Source:            found.source,
		RemoteAllowed:     remoteOK,
		ProtectedBranches: protected,
		Range:             strings.Join(parts, ", "),
		Commits:           commits,
		Findings:          findings,
		Refused:           refused,
		Reasons:           reasons,
		Reminders:         rulesPushReminders(found, pushing),
	}
	if found.rule != nil {
		name, push := found.rule.Name, found.rule.Push.describe(found.selfhosted)
		report.Rule, report.Push = &name, &push
	}
	return report, nil
}
