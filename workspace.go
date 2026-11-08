package main

// The workspace: finding repositories, their status, sync with private WIP
// refs, commit and push behind the audit, identity and local checks.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	syncRemote               = "sync"
	wipHead                  = "refs/gitdcy/wip"
	wipRemote                = "refs/remotes/sync/wip"
	wipApplied               = "refs/gitdcy/applied"
	ignoreBlockStart         = "# BEGIN GITDCY PRIVATE DEFAULTS"
	ignoreBlockEnd           = "# END GITDCY PRIVATE DEFAULTS"
	repoIgnoreBlockStart     = "# BEGIN GITDCY REPO POLICY"
	repoIgnoreBlockEnd       = "# END GITDCY REPO POLICY"
	discoveryMaxDirectories  = 100_000
	discoveryMaxRepositories = 512
	discoveryTimeout         = 10 * time.Second
	readOnlyBatchTimeout     = 15 * time.Second
	readOnlyStatusWorkers    = 8
	readOnlyStatusRepos      = 128
	checkOutputLimit         = 256 * 1024
	checkOutputDisplayLimit  = 16 * 1024
	defaultCheckTimeout      = 15 * 60
	maxCheckTimeout          = 60 * 60
)

var defaultRepoIgnoreRules = []string{
	"AGENTS.md", ".env", ".env.*", "!.env.example", ".codex/", ".claude/", "private/",
	"docs/private/", "state/", "uploads/", "logs/", "*.log", "*.sqlite", "*.sqlite3", "*.db",
	"*.pem", "*.key", "id_rsa", "id_ed25519", "node_modules/", "target/", ".DS_Store",
}

// ---------------------------------------------------------------------------
// Discovery

func discoverEntries(roots []string) ([]RepoEntry, error) {
	repos := []RepoEntry{}
	seen := map[string]bool{}
	budget := newDiscoveryBudget(readOnlyStatus())
	for _, root := range roots {
		paths, err := discoverRepoPathsWithBudget(root, budget)
		if err != nil {
			return nil, err
		}
		for _, repo := range paths {
			if err := budget.checkTime(); err != nil {
				return nil, err
			}
			canonical, err := canonicalize(repo)
			if err != nil {
				canonical = repo
			}
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
			rm, _ := remotes(repo)
			if err := budget.checkTime(); err != nil {
				return nil, err
			}
			entry := entryFor(repo, rm)
			_, suggested := suggestedOriginRemote(entry)
			entry.ReviewRequired = entry.PrimaryRemote == nil && !suggested
			if err := budget.checkTime(); err != nil {
				return nil, err
			}
			repos = append(repos, entry)
		}
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	return repos, nil
}

// entryFor is a repository's entry from its path and remotes.
func entryFor(repo string, rm map[string]string) RepoEntry {
	var origin *string
	if url, ok := rm["origin"]; ok {
		origin = &url
	}
	provider := ProviderOther
	if origin != nil {
		provider = providerFromURL(*origin)
	}
	if provider == ProviderOther {
		provider = providerFromPath(repo)
	}
	return RepoEntry{
		ID: repoID(repo, provider), Path: repo, Provider: provider, Enabled: true,
		PrimaryRemote: origin, WipSync: true,
	}
}

func discoverRepoPaths(root string) ([]string, error) {
	return discoverRepoPathsWithBudget(root, newDiscoveryBudget(readOnlyStatus()))
}

type discoveryBudget struct {
	directories, repositories int
	deadline                  time.Time
	maxDepth, maxRepositories int
}

func newDiscoveryBudget(readOnly bool) *discoveryBudget {
	b := &discoveryBudget{deadline: time.Now().Add(discoveryTimeout), maxDepth: int(^uint(0) >> 1), maxRepositories: discoveryMaxRepositories}
	if readOnly {
		b.maxDepth, b.maxRepositories = 1, readOnlyStatusRepos
	}
	return b
}

func (b *discoveryBudget) checkTime() error {
	if !time.Now().Before(b.deadline) {
		return errf("repository discovery exceeded its time safety limit")
	}
	return nil
}

func (b *discoveryBudget) enterDirectory() error {
	if err := b.checkTime(); err != nil {
		return err
	}
	b.directories++
	if b.directories > discoveryMaxDirectories {
		return errf("repository discovery exceeded its directory safety limit")
	}
	return nil
}

func (b *discoveryBudget) recordRepository() error {
	b.repositories++
	if b.repositories > b.maxRepositories {
		return errf("repository discovery exceeded its repository safety limit")
	}
	return nil
}

func discoverRepoPathsWithBudget(root string, budget *discoveryBudget) ([]string, error) {
	found := []string{}
	if _, err := os.Stat(root); err != nil {
		return found, nil
	}
	var visit func(dir string, depth int) error
	visit = func(dir string, depth int) error {
		if err := budget.enterDirectory(); err != nil {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil
		}
		if _, err := os.Stat(joinPath(dir, ".git")); err == nil {
			if err := budget.recordRepository(); err != nil {
				return err
			}
			found = append(found, dir)
			return nil
		}
		if depth >= budget.maxDepth {
			return nil
		}
		entries, err := readDirUnsorted(dir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if err := budget.checkTime(); err != nil {
				return err
			}
			path := joinPath(dir, e.Name())
			if !e.IsDir() || shouldSkipDir(e.Name()) {
				continue
			}
			if err := visit(path, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, err
	}
	return found, nil
}

// readDirUnsorted lists a folder in the order the system gives, as Rust's
// read_dir did (the result is sorted by id afterwards).
func readDirUnsorted(dir string) ([]os.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "target", "dist", "build", ".venv", "vendor", "tmp", "log", ".cache", ".next", ".turbo":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Status

func repoStatus(entry RepoEntry) RepoStatus {
	s, err := repoStatusResult(entry)
	if err != nil {
		return failedRepoStatus(entry, err.Error())
	}
	return s
}

func failedRepoStatus(entry RepoEntry, message string) RepoStatus {
	return RepoStatus{
		Entry: entry, Path: entry.Path, Remotes: map[string]string{}, IncomingWipTrusted: true,
		Safety:    RepoSafetyReport{Visibility: VisibilityUnknown, PublicTargeted: true},
		Identity:  unavailableIdentity(message),
		LastError: &message,
	}
}

func repoStatusResult(entry RepoEntry) (RepoStatus, error) {
	config := loadLocalConfig()
	branch, err := currentBranch(entry.Path)
	if err != nil {
		return RepoStatus{}, err
	}
	var tracking *string
	if t, err := trackingBranch(entry.Path); err == nil {
		tracking = &t
	}
	rm, err := remotes(entry.Path)
	if err != nil {
		return RepoStatus{}, err
	}
	identity := identityReportWithConfig(entry, rm, config)
	dirty, err := syncPathsWithConfig(entry, config)
	if err != nil {
		return RepoStatus{}, err
	}
	var ahead, behind *uint32
	if tracking != nil {
		if a, b, err := aheadBehind(entry.Path, *tracking); err == nil {
			ahead, behind = a, b
		}
	}
	branchName := "HEAD"
	if branch != nil {
		branchName = *branch
	}
	incoming, _ := latestIncomingWip(entry.Path, branchName)
	trusted := true
	if incoming != nil {
		trusted = wipDeviceTrustedWithConfig(entry, incoming.Device, config)
	}
	outgoing, _ := localWip(entry.Path, branchName)
	safety, err := auditRepoWithConfig(entry, config)
	if err != nil {
		return RepoStatus{}, err
	}
	return RepoStatus{
		Entry: entry, Path: entry.Path, Branch: branch, TrackingBranch: tracking, Remotes: rm,
		DirtyPaths: dirty, Ahead: ahead, Behind: behind, IncomingWip: incoming,
		IncomingWipTrusted: trusted, OutgoingWip: outgoing, Safety: safety, Identity: identity,
	}, nil
}

func statusAll(manifest WorkspaceManifest) []RepoStatus {
	return statusAllForMode(manifest, readOnlyStatus())
}

func statusAllForMode(manifest WorkspaceManifest, readOnly bool) []RepoStatus {
	var entries []RepoEntry
	for _, r := range manifest.Repos {
		if r.Enabled {
			entries = append(entries, r)
		}
	}
	statuses := make([]*RepoStatus, len(entries))
	if !readOnly {
		out := make([]RepoStatus, len(entries))
		for i, e := range entries {
			out[i] = repoStatus(e)
		}
		return out
	}
	processable := len(entries)
	if processable > readOnlyStatusRepos {
		processable = readOnlyStatusRepos
	}
	type result struct {
		index  int
		status RepoStatus
	}
	for first := 0; first < processable; first += readOnlyStatusWorkers {
		end := first + readOnlyStatusWorkers
		if end > processable {
			end = processable
		}
		batch := entries[first:end]
		results := make(chan result, len(batch))
		for offset, e := range batch {
			go func(i int, e RepoEntry) { results <- result{i, repoStatus(e)} }(first+offset, e)
		}
		deadline := time.Now().Add(readOnlyBatchTimeout)
		timedOut := false
		for range batch {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				timedOut = true
				break
			}
			select {
			case r := <-results:
				s := r.status
				statuses[r.index] = &s
			case <-time.After(remaining):
				timedOut = true
			}
			if timedOut {
				break
			}
		}
		for offset, e := range batch {
			if statuses[first+offset] == nil {
				s := failedRepoStatus(e, "repository status exceeded its read-only time safety limit")
				statuses[first+offset] = &s
			}
		}
		if timedOut {
			for i := end; i < len(entries); i++ {
				s := failedRepoStatus(entries[i], "repository status was not started after a prior batch exceeded its read-only time safety limit")
				statuses[i] = &s
			}
			break
		}
	}
	for i := processable; i < len(entries); i++ {
		if statuses[i] == nil {
			s := failedRepoStatus(entries[i], "repository status was not started because the read-only repository safety limit was reached")
			statuses[i] = &s
		}
	}
	out := make([]RepoStatus, len(entries))
	for i, s := range statuses {
		out[i] = *s
	}
	return out
}

// ---------------------------------------------------------------------------
// Sync

func syncRepo(entry RepoEntry) SyncReport {
	report := SyncReport{RepoID: entry.ID}
	if err := syncRepoWithConfig(entry, &report, loadLocalConfig()); err != nil {
		report.block(err.Error())
	}
	return report
}

func syncRepoWithConfig(entry RepoEntry, report *SyncReport, config LocalConfig) error {
	rm, err := remotes(entry.Path)
	if err != nil {
		return err
	}
	identity, err := enforceIdentity(entry, rm, config)
	if err != nil {
		return err
	}
	b, err := currentBranch(entry.Path)
	if err != nil {
		return err
	}
	branch := "HEAD"
	if b != nil {
		branch = *b
	}
	beforeDirty, err := syncPathsWithConfig(entry, config)
	if err != nil {
		return err
	}
	remote, hasRemote := wipRemoteName(rm)
	if entry.WipSync && hasRemote && len(beforeDirty) > 0 {
		sha, err := createWipSnapshot(entry.Path, branch, beforeDirty, identity)
		if err != nil {
			return err
		}
		if err := pushWipSnapshot(entry.Path, remote, branch, sha); err != nil {
			return err
		}
		report.action("pushed WIP snapshot " + shortSHA(sha))
	} else if len(beforeDirty) > 0 {
		report.action("dirty; skipped WIP snapshot because no private WIP remote is configured")
	}
	for _, name := range sortedKeys(rm) {
		if err := git(entry.Path, "fetch", "--prune", "--tags", name); err != nil {
			return err
		}
		report.action("fetched " + name)
	}
	if hasRemote {
		if err := fetchWipRefs(entry.Path, remote); err != nil {
			return err
		}
		report.action("fetched WIP refs from " + remote)
	}
	dirtyAfter, err := syncPathsWithConfig(entry, config)
	if err != nil {
		return err
	}
	if len(dirtyAfter) == 0 {
		if tracking, err := trackingBranch(entry.Path); err == nil {
			_, behind, err := aheadBehind(entry.Path, tracking)
			if err != nil {
				return err
			}
			if behind != nil && *behind > 0 {
				if err := git(entry.Path, "pull", "--ff-only"); err != nil {
					return err
				}
				report.action("fast-forward pulled tracking branch")
			}
		}
	} else {
		report.action("skipped branch pull because working tree is dirty")
	}
	if entry.WipSync && hasRemote {
		applied, err := applyLatestIncomingWip(entry, branch, config)
		if err != nil {
			return err
		}
		if applied != nil {
			report.action("applied incoming WIP from " + applied.Device)
			combined, err := syncPathsWithConfig(entry, config)
			if err != nil {
				return err
			}
			if len(combined) > 0 {
				sha, err := createWipSnapshot(entry.Path, branch, combined, identity)
				if err != nil {
					return err
				}
				if err := pushWipSnapshot(entry.Path, remote, branch, sha); err != nil {
					return err
				}
				report.action("pushed combined WIP " + shortSHA(sha))
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Clone, commit, push

func cloneRepo(request CloneRequest) (string, error) {
	provider := providerFromURL(request.URL)
	if request.Provider != nil {
		provider = *request.Provider
	}
	name := repoNameFromURL(request.URL)
	if request.Name != nil {
		name = *request.Name
	}
	destination := joinPath(joinPath(request.WorkspaceRoot, provider.folder()), sanitizeComponent(name))
	config := loadLocalConfig()
	candidate := RepoEntry{ID: repoID(destination, provider), Path: destination, Provider: provider, Enabled: true, PrimaryRemote: &request.URL, WipSync: true}
	if _, err := enforceIdentity(candidate, map[string]string{"origin": request.URL}, config); err != nil {
		return "", err
	}
	if _, err := os.Stat(destination); err == nil {
		return "", errf("destination already exists: %s", destination)
	}
	if parent := filepath.Dir(destination); parent != "" {
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return "", wrapf(err, "create destination parent %s", parent)
		}
	}
	state, err := runStatus(exec.Command("git", "clone", request.URL, destination))
	if err != nil {
		return "", wrap(err, "run git clone")
	}
	if !state.Success() {
		return "", errf("git clone failed with status %s", exitStatusText(state))
	}
	return destination, nil
}

func commit(repo, message string, paths []string) error {
	if trim(message) == "" {
		return errf("commit message is required")
	}
	entry, err := entryForRepo(repo)
	if err != nil {
		return err
	}
	config := loadLocalConfig()
	rm, err := remotes(repo)
	if err != nil {
		return err
	}
	identity, err := enforceIdentity(entry, rm, config)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		err = git(repo, "add", "-A")
	} else {
		err = gitPaths(repo, []string{"add", "-A"}, paths)
	}
	if err != nil {
		return err
	}
	report, err := auditStagedWithConfig(entry, config)
	if err != nil {
		return err
	}
	if report.hasFatalFindings() {
		_ = git(repo, "reset", "-q")
		return errf("%s", formatAuditBlock(entry, report))
	}
	if identity != nil {
		return gitEnv(repo, identityEnv(*identity), "commit", "-m", message)
	}
	return git(repo, "commit", "-m", message)
}

func identityEnv(p GitIdentityProfile) []string {
	return []string{"GIT_AUTHOR_NAME", p.Name, "GIT_AUTHOR_EMAIL", p.Email, "GIT_COMMITTER_NAME", p.Name, "GIT_COMMITTER_EMAIL", p.Email}
}

func push(repo string) error {
	entry, err := entryForRepo(repo)
	if err != nil {
		return err
	}
	config := loadLocalConfig()
	rm, err := remotes(repo)
	if err != nil {
		return err
	}
	if _, err := enforceIdentity(entry, rm, config); err != nil {
		return err
	}
	report, err := auditRepoWithConfig(entry, config)
	if err != nil {
		return err
	}
	if report.hasFatalFindings() {
		return errf("%s", formatAuditBlock(entry, report))
	}
	checks := checkRepoWithConfig(entry, rm, config, true)
	if checks.isBlocking() {
		return errf("%s", formatCheckBlock(entry, checks))
	}
	return git(repo, "push")
}

func entryForRepo(repo string) (RepoEntry, error) {
	rm, err := remotes(repo)
	if err != nil {
		return RepoEntry{}, err
	}
	return entryFor(repo, rm), nil
}

func formatAuditBlock(entry RepoEntry, report RepoSafetyReport) string {
	out := []string{fmt.Sprintf("GitDCY safety audit blocked %s (visibility: %s)", entry.ID, report.Visibility.label())}
	for _, f := range report.Findings {
		if !f.Fatal {
			continue
		}
		path := "-"
		if f.Path != nil {
			path = *f.Path
		}
		out = append(out, fmt.Sprintf("- [%s] %s: %s (%s)", f.severityLabel(), path, f.Reason, f.Remediation))
	}
	return strings.Join(out, "\n")
}

func installGlobalIgnoreTemplate() (string, error) {
	path, err := globalExcludesFilePath()
	if err != nil {
		return "", err
	}
	if parent := filepath.Dir(path); parent != "" {
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return "", wrapf(err, "create git ignore directory %s", parent)
		}
	}
	existing, _ := readText(path)
	merged := mergeManagedBlock(existing, ignoreBlockStart, ignoreBlockEnd, defaultRepoIgnoreRules)
	if err := os.WriteFile(path, []byte(merged), 0o666); err != nil {
		return "", wrapf(err, "write %s", path)
	}
	current, err := gitConfigGlobalExcludesFile()
	if err != nil {
		return "", err
	}
	if current == nil {
		out, err := runOutput(exec.Command("git", "config", "--global", "core.excludesfile", path))
		if err != nil {
			return "", wrap(err, "set git global core.excludesfile")
		}
		if !out.success() {
			return "", errf("%s", commandError("git config", out))
		}
	}
	return path, nil
}

func globalExcludesFilePath() (string, error) {
	current, err := gitConfigGlobalExcludesFile()
	if err != nil {
		return "", err
	}
	if current != nil {
		return expandHome(*current), nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return joinPath(dir, "git-ignore"), nil
}

func gitConfigGlobalExcludesFile() (*string, error) {
	out, err := runOutput(exec.Command("git", "config", "--global", "--get", "core.excludesfile"))
	if err != nil {
		return nil, wrap(err, "read git global core.excludesfile")
	}
	if out.success() {
		if v := trim(lossy(out.Stdout)); v != "" {
			return &v, nil
		}
	}
	return nil, nil
}

func mergeManagedIgnoreBlock(existing string) string {
	return mergeManagedBlock(existing, ignoreBlockStart, ignoreBlockEnd, defaultRepoIgnoreRules)
}

func mergeManagedBlock(existing, start, end string, rules []string) string {
	var out strings.Builder
	inBlock := false
	for _, line := range lines(existing) {
		switch trim(line) {
		case start:
			inBlock = true
			continue
		case end:
			inBlock = false
			continue
		}
		if !inBlock {
			out.WriteString(line + "\n")
		}
	}
	text := out.String()
	for strings.HasSuffix(text, "\n\n") {
		text = text[:len(text)-1]
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text != "" {
		text += "\n"
	}
	text += start + "\n"
	for _, r := range rules {
		text += r + "\n"
	}
	return text + end + "\n"
}

func setRemote(repo, name, url string) error {
	if trim(name) == "" || trim(url) == "" {
		return errf("remote name and URL are required")
	}
	current, err := remotes(repo)
	if err != nil {
		return err
	}
	if name != syncRemote {
		config := loadLocalConfig()
		provider := providerFromPath(repo)
		if name == "origin" {
			if p := providerFromURL(url); p != ProviderOther {
				provider = p
			}
		}
		candidates := map[string]string{}
		for k, v := range current {
			candidates[k] = v
		}
		candidates[name] = url
		var origin *string
		if o, ok := candidates["origin"]; ok {
			origin = &o
		}
		candidate := RepoEntry{ID: repoID(repo, provider), Path: repo, Provider: provider, Enabled: true, PrimaryRemote: origin, WipSync: true}
		if _, err := enforceIdentity(candidate, candidates, config); err != nil {
			return err
		}
	}
	if _, ok := current[name]; ok {
		return git(repo, "remote", "set-url", name, url)
	}
	return git(repo, "remote", "add", name, url)
}

func setSuggestedOriginRemote(entry RepoEntry) (string, error) {
	url, ok := suggestedOriginRemote(entry)
	if !ok {
		return "", errf("no origin remote template configured for %s", entry.Provider.folder())
	}
	if err := setRemote(entry.Path, "origin", url); err != nil {
		return "", err
	}
	return url, nil
}

func currentBranch(repo string) (*string, error) {
	out, err := gitOutput(repo, "branch", "--show-current")
	if err != nil {
		return nil, err
	}
	if b := trim(out); b != "" {
		return &b, nil
	}
	return nil, nil
}

func trackingBranch(repo string) (string, error) {
	out, err := gitOutput(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return trim(out), err
}

func remotes(repo string) (map[string]string, error) {
	out, err := gitOutput(repo, "remote", "-v")
	if err != nil {
		return nil, err
	}
	rm := map[string]string{}
	for _, line := range lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if _, ok := rm[fields[0]]; !ok {
			rm[fields[0]] = fields[1]
		}
	}
	return rm, nil
}

// ---------------------------------------------------------------------------
// Local checks

func checkRepo(entry RepoEntry) RepoCheckReport {
	config := loadLocalConfig()
	rm, err := remotes(entry.Path)
	if err != nil {
		return RepoCheckReport{State: CheckUnavailable, Message: "could not read repository remotes: " + err.Error()}
	}
	return checkRepoWithConfig(entry, rm, config, false)
}

func checkRepoWithConfig(entry RepoEntry, rm map[string]string, config LocalConfig, beforePush bool) RepoCheckReport {
	requireChecks := config.RequireChecks != nil && *config.RequireChecks
	if config.CheckProfiles == nil || len(*config.CheckProfiles) == 0 {
		r := RepoCheckReport{EnforcementEnabled: requireChecks, State: CheckDisabled, Message: "no local check profile is configured for this repository"}
		if requireChecks {
			r.State, r.Message = CheckUnconfigured, "check enforcement is enabled but no check profiles are configured"
		}
		return r
	}
	profiles := *config.CheckProfiles
	var invalid []string
	for _, id := range sortedKeys(profiles) {
		if reason, bad := invalidCheckProfileReason(id, profiles[id]); bad {
			invalid = append(invalid, id+": "+reason)
		}
	}
	if config.ConfigError != nil {
		invalid = append([]string{*config.ConfigError}, invalid...)
	}
	if len(invalid) > 0 {
		return RepoCheckReport{EnforcementEnabled: true, State: CheckInvalid, Message: "invalid check profile configuration: " + strings.Join(invalid, "; ")}
	}
	var matching []string
	for _, id := range sortedKeys(profiles) {
		p := profiles[id]
		if profileSelectorsMatch(p.PathPrefixes, p.RemotePatterns, p.Providers, entry.Path, entry.Provider, rm) {
			matching = append(matching, id)
		}
	}
	candidates := []string{}
	for _, id := range matching {
		if !beforePush || profiles[id].RunBeforePush {
			candidates = append(candidates, id)
		}
	}
	enforcement := requireChecks || len(candidates) > 0
	if len(candidates) != 1 {
		state := CheckAmbiguous
		message := "multiple local check profiles match this repository: " + strings.Join(candidates, ", ")
		if len(candidates) == 0 {
			state = CheckDisabled
			if enforcement {
				state = CheckUnconfigured
			}
			message = "no local check profile matches this repository path, provider, and remotes"
			if beforePush && len(matching) > 0 {
				message = "matching check profiles are disabled before push"
			}
		}
		return RepoCheckReport{EnforcementEnabled: enforcement, State: state, Candidates: candidates, Message: message}
	}
	profileID := candidates[0]
	profile := profiles[profileID]
	var clean *bool
	if c, err := worktreeIsClean(entry.Path); err == nil {
		clean = &c
	}
	if beforePush && profile.RequireCleanWorktree {
		if clean != nil && !*clean {
			return RepoCheckReport{EnforcementEnabled: true, State: CheckFailed, Profile: &profileID, Candidates: candidates, WorktreeClean: clean,
				Message: "working tree is not clean; checks would not exactly match the commit being pushed"}
		}
		if clean == nil {
			return RepoCheckReport{EnforcementEnabled: true, State: CheckUnavailable, Profile: &profileID, Candidates: candidates,
				Message: "could not determine whether the working tree is clean"}
		}
	}
	var results []CheckResult
	for _, check := range profile.Checks {
		result := runCheckCommand(entry.Path, check)
		results = append(results, result)
		if result.State != ResultPassed {
			state := CheckFailed
			switch result.State {
			case ResultTimedOut:
				state = CheckTimedOut
			case ResultUnavailable:
				state = CheckUnavailable
			}
			return RepoCheckReport{EnforcementEnabled: true, State: state, Profile: &profileID, Candidates: candidates, WorktreeClean: clean, Results: results,
				Message: fmt.Sprintf("check %s failed in profile %s", result.Name, profileID)}
		}
	}
	return RepoCheckReport{EnforcementEnabled: true, State: CheckPassed, Profile: &profileID, Candidates: candidates, WorktreeClean: clean, Results: results,
		Message: fmt.Sprintf("all %d configured checks passed in profile %s", len(profile.Checks), profileID)}
}

func invalidCheckProfileReason(id string, p CheckProfile) (string, bool) {
	if trim(id) == "" {
		return "profile id is empty", true
	}
	if len(p.PathPrefixes) == 0 && len(p.RemotePatterns) == 0 && len(p.Providers) == 0 {
		return "at least one path_prefixes, remote_patterns, or providers selector is required", true
	}
	for _, prefix := range p.PathPrefixes {
		if !filepath.IsAbs(expandHome(prefix)) {
			return "path_prefixes must be absolute paths or use ~/", true
		}
	}
	for _, pattern := range p.RemotePatterns {
		if trim(pattern) == "" {
			return "remote_patterns cannot contain empty values", true
		}
	}
	if len(p.Checks) == 0 {
		return "at least one check command is required", true
	}
	for _, c := range p.Checks {
		if trim(c.Name) == "" {
			return "check names cannot be empty", true
		}
		if trim(c.Program) == "" {
			return fmt.Sprintf("check %s has no program", c.Name), true
		}
		nul := strings.Contains(c.Program, "\x00")
		for _, a := range c.Args {
			nul = nul || strings.Contains(a, "\x00")
		}
		if nul {
			return fmt.Sprintf("check %s contains a NUL character", c.Name), true
		}
		if c.TimeoutSeconds != nil && (*c.TimeoutSeconds == 0 || *c.TimeoutSeconds > maxCheckTimeout) {
			return fmt.Sprintf("check %s timeout_seconds must be between 1 and %d", c.Name, maxCheckTimeout), true
		}
	}
	return "", false
}

func profileSelectorsMatch(prefixes, patterns []string, providers []Provider, repo string, provider Provider, rm map[string]string) bool {
	pathMatches := len(prefixes) == 0
	for _, prefix := range prefixes {
		p := expandHome(prefix)
		canonicalRepo, err := canonicalize(repo)
		if err != nil {
			canonicalRepo = repo
		}
		canonicalPrefix, err := canonicalize(p)
		if err != nil {
			canonicalPrefix = p
		}
		if pathStartsWith(canonicalRepo, canonicalPrefix) {
			pathMatches = true
		}
	}
	remoteMatches := len(patterns) == 0
	for _, pattern := range patterns {
		name, url, ok := "origin", "", false
		if u, has := rm["origin"]; has {
			url, ok = u, true
		} else if u, has := rm[syncRemote]; has {
			name, url, ok = syncRemote, u, true
		}
		if ok && remoteSelectorMatches(pattern, name, url) {
			remoteMatches = true
		}
	}
	providerMatches := len(providers) == 0
	for _, p := range providers {
		if p == provider {
			providerMatches = true
		}
	}
	return pathMatches && remoteMatches && providerMatches
}

func worktreeIsClean(repo string) (bool, error) {
	out, err := gitOutput(repo, "status", "--porcelain=v1", "--untracked-files=all")
	return trim(out) == "", err
}

func runCheckCommand(repo string, check CheckCommand) CheckResult {
	timeout := time.Duration(defaultCheckTimeout) * time.Second
	if check.TimeoutSeconds != nil {
		timeout = time.Duration(*check.TimeoutSeconds) * time.Second
	}
	cmd := exec.Command(check.Program, check.Args...)
	cmd.Dir = repo
	setEnv(cmd, "CI", "true")
	setEnv(cmd, "GIT_TERMINAL_PROMPT", "0")
	setEnv(cmd, "CARGO_TERM_COLOR", "never")
	started := time.Now()
	display := check.displayCommand()
	out, err := runBounded(cmd, checkOutputLimit, timeout)
	if err != nil {
		return CheckResult{Name: check.Name, Command: display, State: ResultUnavailable, DurationMS: time.Since(started).Milliseconds(), Output: truncateCheckOutput(err.Error())}
	}
	duration := time.Since(started).Milliseconds()
	_, hasCode := out.code()
	timedOut := !out.success() && !hasCode && time.Since(started) >= timeout
	state := ResultFailed
	switch {
	case out.success():
		state = ResultPassed
	case timedOut:
		state = ResultTimedOut
	}
	text := combinedCheckOutput(out)
	if trim(text) == "" && !out.success() {
		text = "process exited with " + out.statusText()
	}
	return CheckResult{Name: check.Name, Command: display, State: state, DurationMS: duration, Output: truncateCheckOutput(text)}
}

func combinedCheckOutput(out *procOutput) string {
	stdout, stderr := trim(lossy(out.Stdout)), trim(lossy(out.Stderr))
	switch {
	case stdout == "" && stderr == "":
		return ""
	case stderr == "":
		return stdout
	case stdout == "":
		return stderr
	}
	return stdout + "\n" + stderr
}

func truncateCheckOutput(output string) string {
	if len(output) <= checkOutputDisplayLimit {
		return output
	}
	end := checkOutputDisplayLimit
	for end > 0 && output[end]&0xC0 == 0x80 {
		end--
	}
	return output[:end] + "\n[check output truncated]"
}

func formatCheckBlock(entry RepoEntry, report RepoCheckReport) string {
	out := []string{fmt.Sprintf("GitDCY checks blocked %s (%s)", entry.ID, report.stateLabel())}
	for _, r := range report.Results {
		if r.State == ResultPassed {
			continue
		}
		out = append(out, fmt.Sprintf("- [%s] %s (%s)", r.State, r.Name, r.Command))
		if trim(r.Output) != "" {
			for _, line := range lines(r.Output) {
				out = append(out, "  "+line)
			}
		}
	}
	out = append(out, "- "+report.Message)
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Identity

type effectiveGitIdentity struct {
	authorName, authorEmail, committerName, committerEmail *string
	environmentOverrides                                   []string
}

func identityReport(entry RepoEntry) RepoIdentityReport {
	rm, _ := remotes(entry.Path)
	return identityReportWithConfig(entry, rm, loadLocalConfig())
}

func identityReportWithConfig(entry RepoEntry, rm map[string]string, config LocalConfig) RepoIdentityReport {
	enforcement := identityEnforcementEnabled(config)
	actual := effectiveIdentity(entry.Path)
	base := RepoIdentityReport{
		EnforcementEnabled: enforcement, ActualName: actual.authorName, ActualEmail: actual.authorEmail,
		CommitterName: actual.committerName, CommitterEmail: actual.committerEmail, EnvironmentOverrides: actual.environmentOverrides,
	}
	if config.IdentityProfiles == nil || len(*config.IdentityProfiles) == 0 {
		base.State, base.Message = IdentityDisabled, "identity enforcement is disabled; no identity profiles are configured"
		if enforcement {
			base.State, base.Message = IdentityUnconfigured, "identity enforcement is enabled but no identity profiles are configured"
		}
		return base
	}
	profiles := *config.IdentityProfiles
	var invalid []string
	for _, id := range sortedKeys(profiles) {
		if reason, bad := invalidIdentityProfileReason(id, profiles[id]); bad {
			invalid = append(invalid, id+": "+reason)
		}
	}
	if config.ConfigError != nil {
		invalid = append([]string{*config.ConfigError}, invalid...)
	}
	if len(invalid) > 0 {
		base.State, base.Message = IdentityInvalid, "invalid identity profile configuration: "+strings.Join(invalid, "; ")
		return base
	}
	candidates := []string{}
	for _, id := range sortedKeys(profiles) {
		p := profiles[id]
		if profileSelectorsMatch(p.PathPrefixes, p.RemotePatterns, p.Providers, entry.Path, entry.Provider, rm) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) != 1 {
		base.Candidates = candidates
		if len(candidates) == 0 {
			base.State, base.Message = IdentityUnconfigured, "no identity profile matches this repository path, provider, and remotes"
		} else {
			base.State, base.Message = IdentityAmbiguous, "multiple identity profiles match this repository: "+strings.Join(candidates, ", ")
		}
		return base
	}
	profileID := candidates[0]
	p := profiles[profileID]
	name, email := trim(p.Name), trim(p.Email)
	var differences []string
	if !same(actual.authorName, name) || !same(actual.authorEmail, email) {
		differences = append(differences, "author is "+displayIdentity(actual.authorName, actual.authorEmail))
	}
	if !same(actual.committerName, name) || !same(actual.committerEmail, email) {
		differences = append(differences, "committer is "+displayIdentity(actual.committerName, actual.committerEmail))
	}
	base.Profile, base.Candidates, base.ExpectedName, base.ExpectedEmail = &profileID, candidates, &name, &email
	if len(differences) == 0 {
		base.State, base.Message = IdentityMatched, "Git author and committer match identity profile "+profileID
	} else {
		base.State = IdentityMismatch
		base.Message = fmt.Sprintf("Git identity does not match profile %s (expected %s <%s>; %s)", profileID, name, email, strings.Join(differences, "; "))
	}
	return base
}

func same(p *string, v string) bool { return p != nil && *p == v }

func identityEnforcementEnabled(config LocalConfig) bool {
	if config.RequireIdentity != nil {
		return *config.RequireIdentity
	}
	return config.IdentityProfiles != nil && len(*config.IdentityProfiles) > 0
}

func invalidIdentityProfileReason(id string, p GitIdentityProfile) (string, bool) {
	switch {
	case trim(id) == "":
		return "profile id is empty", true
	case trim(p.Name) == "":
		return "name is empty", true
	case trim(p.Email) == "":
		return "email is empty", true
	case len(p.PathPrefixes) == 0 && len(p.RemotePatterns) == 0 && len(p.Providers) == 0:
		return "at least one path_prefixes, remote_patterns, or providers selector is required", true
	}
	for _, prefix := range p.PathPrefixes {
		if !filepath.IsAbs(expandHome(prefix)) {
			return "path_prefixes must be absolute paths or use ~/", true
		}
	}
	for _, pattern := range p.RemotePatterns {
		if trim(pattern) == "" {
			return "remote_patterns cannot contain empty values", true
		}
	}
	return "", false
}

func identityProfileMatches(p GitIdentityProfile, repo string, provider Provider, rm map[string]string) bool {
	return profileSelectorsMatch(p.PathPrefixes, p.RemotePatterns, p.Providers, repo, provider, rm)
}

func remoteSelectorMatches(pattern, name, url string) bool {
	pattern = asciiLower(trim(pattern))
	if pattern == "" {
		return false
	}
	if asciiLower(name) == pattern {
		return true
	}
	pattern = normalizeRemoteSelector(pattern)
	u := normalizeRemoteSelector(url)
	return u == pattern || strings.HasPrefix(u, pattern+"/")
}

func normalizeRemoteSelector(value string) string {
	value = asciiLower(trim(value))
	rest := value
	if _, r, ok := splitOnce(value, "://"); ok {
		rest = r
	}
	if i := strings.IndexAny(rest, "/:"); i >= 0 && strings.Contains(rest[:i], "@") {
		authority := rest[:i]
		_, withoutUser, _ := rsplitOnce(authority, "@")
		rest = withoutUser + rest[i:]
	} else if _, withoutUser, ok := rsplitOnce(rest, "@"); ok {
		rest = withoutUser
	}
	normalized := rest
	if host, path, ok := splitOnce(rest, ":"); ok {
		normalized = host + "/" + path
	} else if host, path, ok := splitOnce(rest, "/"); ok {
		normalized = host + "/" + path
	}
	normalized = strings.Trim(normalized, "/")
	return strings.TrimSuffix(normalized, ".git")
}

func effectiveIdentity(repo string) effectiveGitIdentity {
	configuredName := gitConfigValue(repo, "user.name")
	configuredEmail := gitConfigValue(repo, "user.email")
	var id effectiveGitIdentity
	pick := func(name string, fallback *string) *string {
		if v, ok := os.LookupEnv(name); ok {
			id.environmentOverrides = append(id.environmentOverrides, name)
			return &v
		}
		return fallback
	}
	id.authorName = pick("GIT_AUTHOR_NAME", configuredName)
	id.authorEmail = pick("GIT_AUTHOR_EMAIL", configuredEmail)
	id.committerName = pick("GIT_COMMITTER_NAME", configuredName)
	id.committerEmail = pick("GIT_COMMITTER_EMAIL", configuredEmail)
	return id
}

func gitConfigValue(repo, key string) *string {
	var out *procOutput
	var err error
	if info, statErr := os.Stat(repo); statErr == nil && info.IsDir() {
		out, err = gitCommandOutput(gitCommand(repo, "config", "--get", key))
	} else {
		out, err = runOutput(exec.Command("git", "config", "--get", key))
	}
	if err != nil || !out.success() {
		return nil
	}
	if v := trim(lossy(out.Stdout)); v != "" {
		return &v
	}
	return nil
}

func enforceIdentity(entry RepoEntry, rm map[string]string, config LocalConfig) (*GitIdentityProfile, error) {
	report := identityReportWithConfig(entry, rm, config)
	if report.isBlocking() {
		return nil, errf("identity check blocked %s: %s", entry.ID, report.Message)
	}
	if !report.EnforcementEnabled || report.State != IdentityMatched {
		return nil, nil
	}
	if report.Profile == nil {
		return nil, errf("identity check matched without a profile for %s", entry.ID)
	}
	if config.IdentityProfiles != nil {
		if p, ok := (*config.IdentityProfiles)[*report.Profile]; ok {
			return &p, nil
		}
	}
	return nil, errf("identity profile disappeared while checking %s", entry.ID)
}

// ---------------------------------------------------------------------------
// Audit and policy

func dirtyPaths(repo string) ([]ChangedPath, error) {
	out, err := gitBytes(repo, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parsePorcelainV2Z(out), nil
}

func syncPaths(entry RepoEntry) ([]ChangedPath, error) {
	return syncPathsWithConfig(entry, loadLocalConfig())
}

func auditRepo(entry RepoEntry) (RepoSafetyReport, error) {
	return auditRepoWithConfig(entry, loadLocalConfig())
}

type auditResult struct {
	entry  RepoEntry
	report RepoSafetyReport
	err    error
}

func auditAll(manifest WorkspaceManifest) []auditResult {
	var out []auditResult
	for _, e := range manifest.Repos {
		if e.Enabled {
			r, err := auditRepo(e)
			out = append(out, auditResult{e, r, err})
		}
	}
	return out
}

func policyReport(entry RepoEntry) (RepoPolicyReport, error) {
	return policyReportWithConfig(entry, loadLocalConfig())
}

type policyResult struct {
	entry  RepoEntry
	report RepoPolicyReport
	err    error
}

func policyAll(manifest WorkspaceManifest) []policyResult {
	var out []policyResult
	for _, e := range manifest.Repos {
		if e.Enabled {
			r, err := policyReport(e)
			out = append(out, policyResult{e, r, err})
		}
	}
	return out
}

func applyPolicy(entry RepoEntry) ([]PolicyAction, error) {
	report, err := policyReportWithConfig(entry, loadLocalConfig())
	if err != nil {
		return nil, err
	}
	var actions []PolicyAction
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "missing repo ignore rule") {
			if err := writeRepoIgnoreBlock(entry.Path, report.Policy.IgnoreRules); err != nil {
				return nil, err
			}
			actions = append(actions, PolicyAction{"updated .gitignore with GitDCY repo policy block"})
			break
		}
	}
	return actions, nil
}

func policyReportWithConfig(entry RepoEntry, config LocalConfig) (RepoPolicyReport, error) {
	rm, err := remotes(entry.Path)
	if err != nil {
		return RepoPolicyReport{}, err
	}
	visibility := classifyRepoVisibility(entry, rm, config)
	rules := repoIgnoreRules(entry, config)
	var findings []PolicyFinding
	var actions []PolicyAction
	safety, err := auditRepoWithConfig(entry, config)
	if err != nil {
		return RepoPolicyReport{}, err
	}
	for _, f := range safety.Findings {
		findings = append(findings, PolicyFinding{Severity: PolicyBlocker, Path: f.Path, Message: f.Reason})
	}
	identity := identityReportWithConfig(entry, rm, config)
	if identity.isBlocking() {
		findings = append(findings, PolicyFinding{Severity: PolicyBlocker, Message: "identity policy: " + identity.Message})
	}
	existing := repoIgnoreLines(entry.Path)
	for _, rule := range rules {
		if !existing[rule] {
			findings = append(findings, PolicyFinding{Severity: PolicyDrift, Path: ptr(".gitignore"), Message: "missing repo ignore rule `" + rule + "`"})
		}
	}
	if _, ok := rm[syncRemote]; !ok && entry.Provider != ProviderForgejo {
		findings = append(findings, PolicyFinding{Severity: PolicyInfo, Message: "no private sync remote configured for WIP refs"})
	}
	for _, f := range findings {
		if strings.Contains(f.Message, "missing repo ignore rule") {
			actions = append(actions, PolicyAction{"write or refresh GitDCY repo policy block in .gitignore"})
			break
		}
	}
	return RepoPolicyReport{
		Policy:   RepoPolicy{Visibility: visibility, PublicTargeted: visibility != VisibilityPrivate, IgnoreRules: rules},
		Findings: findings, Actions: actions,
	}, nil
}

func repoIgnoreRules(entry RepoEntry, config LocalConfig) []string {
	rules := append([]string{}, defaultRepoIgnoreRules...)
	if config.IgnoreProfiles != nil {
		name, _ := fileName(entry.Path)
		for _, key := range []string{"*", entry.Provider.folder(), entry.ID, name} {
			rules = append(rules, (*config.IgnoreProfiles)[key]...)
		}
	}
	return sortDedup(rules)
}

func repoIgnoreLines(repo string) map[string]bool {
	text, _ := readText(joinPath(repo, ".gitignore"))
	set := map[string]bool{}
	for _, line := range lines(text) {
		line = trim(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			set[line] = true
		}
	}
	return set
}

func writeRepoIgnoreBlock(repo string, rules []string) error {
	path := joinPath(repo, ".gitignore")
	existing, _ := readText(path)
	merged := mergeManagedBlock(existing, repoIgnoreBlockStart, repoIgnoreBlockEnd, rules)
	if err := os.WriteFile(path, []byte(merged), 0o666); err != nil {
		return wrapf(err, "write %s", path)
	}
	return nil
}

func auditRepoWithConfig(entry RepoEntry, config LocalConfig) (RepoSafetyReport, error) {
	rm, err := remotes(entry.Path)
	if err != nil {
		return RepoSafetyReport{}, err
	}
	visibility := classifyRepoVisibility(entry, rm, config)
	report := RepoSafetyReport{Visibility: visibility, PublicTargeted: visibility != VisibilityPrivate}
	tracked, err := trackedPaths(entry.Path)
	if err != nil {
		return RepoSafetyReport{}, err
	}
	for _, path := range tracked {
		if f, ok := currentTreeFinding(path, report.PublicTargeted); ok {
			report.Findings = append(report.Findings, f)
		}
	}
	return report, nil
}

func auditStagedWithConfig(entry RepoEntry, config LocalConfig) (RepoSafetyReport, error) {
	rm, err := remotes(entry.Path)
	if err != nil {
		return RepoSafetyReport{}, err
	}
	visibility := classifyRepoVisibility(entry, rm, config)
	report := RepoSafetyReport{Visibility: visibility, PublicTargeted: visibility != VisibilityPrivate}
	staged, err := stagedPaths(entry.Path)
	if err != nil {
		return RepoSafetyReport{}, err
	}
	for _, change := range staged {
		if change.deleted {
			continue
		}
		if f, ok := stagedPathFinding(change.path, report.PublicTargeted); ok {
			report.Findings = append(report.Findings, f)
		}
	}
	return report, nil
}

func classifyRepoVisibility(entry RepoEntry, rm map[string]string, config LocalConfig) RepoVisibility {
	if o, ok := visibilityOverride(entry, config); ok {
		if o == "public" {
			return VisibilityPublic
		}
		return VisibilityPrivate
	}
	for _, name := range sortedKeys(rm) {
		if publicExportRemoteName(name, config) {
			return VisibilityPublic
		}
	}
	if len(rm) == 0 {
		return VisibilityPrivate
	}
	sawUnknown := false
	for _, name := range sortedKeys(rm) {
		url := rm[name]
		if privateRemoteURL(url, config) {
			continue
		}
		switch v, ok := remoteURLVisibility(url); {
		case ok && v == VisibilityPublic:
			return VisibilityPublic
		case ok && v == VisibilityPrivate:
		default:
			sawUnknown = true
		}
	}
	if sawUnknown {
		return VisibilityUnknown
	}
	return VisibilityPrivate
}

func publicExportRemoteName(name string, config LocalConfig) bool {
	if name == "public" {
		return true
	}
	return config.PublicExportRemotes != nil && contains(*config.PublicExportRemotes, name)
}

func privateRemoteURL(url string, config LocalConfig) bool {
	lower := asciiLower(url)
	if providerFromURL(url) == ProviderForgejo {
		return true
	}
	for _, p := range privateRemotePatterns(config) {
		if strings.Contains(lower, asciiLower(p)) {
			return true
		}
	}
	return false
}

func privateRemotePatterns(config LocalConfig) []string {
	patterns := []string{"forgejo", "forgejo-easy"}
	if config.PrivateRemotePatterns != nil {
		patterns = append(patterns, *config.PrivateRemotePatterns...)
	}
	return sortDedup(patterns)
}

func visibilityOverride(entry RepoEntry, config LocalConfig) (VisibilityOverride, bool) {
	if config.VisibilityOverrides == nil {
		return "", false
	}
	name, _ := fileName(entry.Path)
	for _, key := range []string{"*", entry.ID, name} {
		if v, ok := (*config.VisibilityOverrides)[key]; ok {
			return v, true
		}
	}
	return "", false
}

func remoteURLVisibility(url string) (RepoVisibility, bool) {
	if readOnlyStatus() {
		return "", false
	}
	if slug, ok := repoSlugForHost(url, "github.com"); ok {
		return hostedVisibility(exec.Command("gh", "repo", "view", slug, "--json", "visibility", "--jq", ".visibility"))
	}
	if slug, ok := repoSlugForHost(url, "gitlab.com"); ok {
		return hostedVisibility(exec.Command("glab", "repo", "view", slug, "--output", "json"))
	}
	return "", false
}

func hostedVisibility(cmd *exec.Cmd) (RepoVisibility, bool) {
	out, err := runOutput(cmd)
	if err != nil || !out.success() {
		return "", false
	}
	lower := asciiLower(lossy(out.Stdout))
	switch {
	case strings.Contains(lower, "public"):
		return VisibilityPublic, true
	case strings.Contains(lower, "private"):
		return VisibilityPrivate, true
	}
	return "", false
}

func repoSlugForHost(url, host string) (string, bool) {
	trimmed := strings.TrimRight(trimSuffixes(trim(url), ".git"), "/")
	if _, rest, ok := splitOnce(trimmed, host+"/"); ok {
		return normalizeRemoteSlug(rest)
	}
	if _, rest, ok := splitOnce(trimmed, host+":"); ok {
		return normalizeRemoteSlug(rest)
	}
	return "", false
}

// trimSuffixes is Rust's trim_end_matches with a string: every repeat.
func trimSuffixes(s, suffix string) string {
	for suffix != "" && strings.HasSuffix(s, suffix) {
		s = s[:len(s)-len(suffix)]
	}
	return s
}

func normalizeRemoteSlug(value string) (string, bool) {
	slug := trimSuffixes(strings.Trim(value, "/"), ".git")
	var parts []string
	for _, p := range strings.Split(slug, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

type stagedPath struct {
	path    string
	deleted bool
}

func nulParts(out []byte) []string {
	var parts []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			parts = append(parts, lossy(p))
		}
	}
	return parts
}

func stagedPaths(repo string) ([]stagedPath, error) {
	out, err := gitBytes(repo, "diff", "--cached", "--name-status", "-z")
	if err != nil {
		return nil, err
	}
	parts := nulParts(out)
	var staged []stagedPath
	for i := 0; i < len(parts); {
		status := parts[i]
		i++
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i+1 >= len(parts) {
				break
			}
			staged = append(staged, stagedPath{path: parts[i+1]})
			i += 2
			continue
		}
		if i >= len(parts) {
			break
		}
		staged = append(staged, stagedPath{path: parts[i], deleted: strings.HasPrefix(status, "D")})
		i++
	}
	return staged, nil
}

func trackedPaths(repo string) ([]string, error) {
	out, err := gitBytes(repo, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return nulParts(out), nil
}

func currentTreeFinding(path string, public bool) (SafetyFinding, bool) {
	if generatedCachePath(path) {
		return finding(path, "generated dependency/build cache is tracked", "remove it from Git and add the cache path to .gitignore"), true
	}
	return publicOnlyPathFinding(path, public)
}

func stagedPathFinding(path string, public bool) (SafetyFinding, bool) {
	if generatedCachePath(path) {
		return finding(path, "generated dependency/build cache is staged", "unstage it and add the cache path to .gitignore"), true
	}
	return publicOnlyPathFinding(path, public)
}

func publicOnlyPathFinding(path string, public bool) (SafetyFinding, bool) {
	if !public {
		return SafetyFinding{}, false
	}
	switch {
	case agentNotesPath(path):
		return finding(path, "agent operating notes are private-by-default for public repos", "remove the file from the public tree or mark the repo private in GitDCY local config"), true
	case privateEnvPath(path):
		return finding(path, "private environment file is not public source", "remove it from Git and keep only sanitized .env.example files"), true
	case privateRuntimePath(path):
		return finding(path, "private/runtime path is not public source", "remove it from Git, move it to ignored local state, or confirm the repo is private"), true
	case privateKeyOrDatabasePath(path):
		return finding(path, "private key or local database path is not public source", "remove it from Git and rotate credentials if a real secret was committed"), true
	}
	return SafetyFinding{}, false
}

func finding(path, reason, remediation string) SafetyFinding {
	return SafetyFinding{Fatal: true, Path: &path, Reason: reason, Remediation: remediation}
}

func lastPart(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func generatedCachePath(path string) bool {
	return pathHasComponent(path, "node_modules") || pathHasComponent(path, "target")
}

func agentNotesPath(path string) bool { return lastPart(path) == "AGENTS.md" }

func privateEnvPath(path string) bool {
	name := lastPart(path)
	return strings.HasPrefix(name, ".env") && name != ".env.example"
}

func privateRuntimePath(path string) bool {
	for _, p := range []string{"private/", "docs/private/", ".codex/", ".claude/", "state/", "uploads/", "logs/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func privateKeyOrDatabasePath(path string) bool {
	name := lastPart(path)
	if name == "id_rsa" || name == "id_ed25519" {
		return true
	}
	for _, s := range []string{".pem", ".key", ".sqlite", ".sqlite3", ".db"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func pathHasComponent(path, component string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == component {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Dirty paths and local sync files

func syncPathsWithConfig(entry RepoEntry, config LocalConfig) ([]ChangedPath, error) {
	paths, err := dirtyPaths(entry.Path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p.Path] = true
	}
	for _, path := range configuredLocalSyncFiles(entry, config) {
		if seen[path] || !localSyncFileExists(entry.Path, path) {
			continue
		}
		if gitTrackedPath(entry.Path, path) {
			continue
		}
		seen[path] = true
		paths = append(paths, ChangedPath{Path: path, Kind: ChangeLocal})
	}
	sort.SliceStable(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	return paths, nil
}

func configuredLocalSyncFiles(entry RepoEntry, config LocalConfig) []string {
	files := []string{}
	if config.LocalSyncFiles == nil {
		return files
	}
	name, _ := fileName(entry.Path)
	seen := map[string]bool{}
	for _, key := range []string{"*", entry.ID, name} {
		for _, value := range (*config.LocalSyncFiles)[key] {
			if p, ok := safeRelativeLocalSyncPath(value); ok && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	return files
}

func configuredTrustedWipDevices(entry RepoEntry, config LocalConfig) []string {
	devices := []string{}
	if config.TrustedWipDevices == nil {
		return devices
	}
	name, _ := fileName(entry.Path)
	seen := map[string]bool{}
	for _, key := range []string{"*", entry.ID, name} {
		for _, value := range (*config.TrustedWipDevices)[key] {
			if d, ok := normalizeDeviceID(value); ok && !seen[d] {
				seen[d] = true
				devices = append(devices, d)
			}
		}
	}
	return devices
}

func wipDeviceTrustedWithConfig(entry RepoEntry, device string, config LocalConfig) bool {
	d, ok := normalizeDeviceID(device)
	return ok && contains(configuredTrustedWipDevices(entry, config), d)
}

func safeRelativeLocalSyncPath(value string) (string, bool) {
	trimmed := trim(value)
	if trimmed == "" || filepath.IsAbs(trimmed) || isSep(trimmed[0]) {
		return "", false
	}
	var parts []string
	start := 0
	for i := 0; i <= len(trimmed); i++ {
		if i == len(trimmed) || isSep(trimmed[i]) {
			part := trimmed[start:i]
			start = i + 1
			switch part {
			case "", ".":
				continue
			case "..", ".git":
				return "", false
			}
			if filepath.VolumeName(part) != "" {
				return "", false
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "/"), true
}

func normalizeDeviceID(value string) (string, bool) {
	d := sanitizeRefComponent(trim(value))
	if d == "" || strings.Contains(d, "/") {
		return "", false
	}
	return d, true
}

func localSyncFileExists(repo, path string) bool {
	info, err := os.Lstat(joinPath(repo, path))
	return err == nil && info.Mode().IsRegular()
}

func gitTrackedPath(repo, path string) bool {
	out, err := gitCommandOutput(gitCommandWithPaths(repo, []string{"ls-files", "--error-unmatch"}, []string{path}))
	return err == nil && out.success()
}

func parsePorcelainV2Z(output []byte) []ChangedPath {
	var paths []ChangedPath
	parts := bytes.Split(output, []byte{0})
	for i := 0; i < len(parts); i++ {
		if len(parts[i]) == 0 {
			continue
		}
		text := lossy(parts[i])
		if p, ok := strings.CutPrefix(text, "? "); ok {
			paths = append(paths, ChangedPath{Path: p, Kind: ChangeNew})
			continue
		}
		if strings.HasPrefix(text, "1 ") || strings.HasPrefix(text, "u ") {
			if _, p, ok := rsplitOnce(text, " "); ok {
				paths = append(paths, ChangedPath{Path: p, Kind: ChangeTracked})
			}
			continue
		}
		if strings.HasPrefix(text, "2 ") {
			if _, p, ok := rsplitOnce(text, " "); ok {
				paths = append(paths, ChangedPath{Path: p, Kind: ChangeTracked})
			}
			// The original path follows; skip it (empty parts do not count).
			for i+1 < len(parts) && len(parts[i+1]) == 0 {
				i++
			}
			i++
		}
	}
	sort.SliceStable(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	var out []ChangedPath
	for i, p := range paths {
		if i == 0 || p.Path != paths[i-1].Path {
			out = append(out, p)
		}
	}
	return out
}

func aheadBehind(repo, tracking string) (*uint32, *uint32, error) {
	out, err := gitOutput(repo, "rev-list", "--left-right", "--count", tracking+"...HEAD")
	if err != nil {
		return nil, nil, err
	}
	fields := strings.Fields(out)
	parse := func(i int) *uint32 {
		if i >= len(fields) {
			return nil
		}
		n, err := strconv.ParseUint(fields[i], 10, 32)
		if err != nil {
			return nil
		}
		v := uint32(n)
		return &v
	}
	return parse(1), parse(0), nil
}

// ---------------------------------------------------------------------------
// WIP snapshots

func createWipSnapshot(repo, branch string, dirty []ChangedPath, identity *GitIdentityProfile) (string, error) {
	tempIndex, err := tempIndexPath(repo)
	if err != nil {
		return "", err
	}
	defer os.Remove(tempIndex)
	env := []string{"GIT_INDEX_FILE", tempIndex}
	if err := gitEnv(repo, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	paths := make([]string, len(dirty))
	for i, d := range dirty {
		paths[i] = d.Path
	}
	if err := gitPathsEnv(repo, []string{"add", "-A", "-f"}, paths, env); err != nil {
		return "", err
	}
	treeOut, err := gitOutputEnv(repo, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree := trim(treeOut)
	if existing, err := localWip(repo, branch); err != nil {
		return "", err
	} else if existing != nil {
		if t, err := commitTree(repo, existing.SHA); err == nil && t == tree {
			return existing.SHA, nil
		}
	}
	device := repoDeviceID(repo)
	message := fmt.Sprintf("GitDCY WIP from %s on %s", device, branch)
	parentOut, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	parent := trim(parentOut)
	var shaOut string
	if identity != nil {
		shaOut, err = gitOutputEnv(repo, identityEnv(*identity), "commit-tree", tree, "-p", parent, "-m", message)
	} else {
		shaOut, err = gitOutput(repo, "commit-tree", tree, "-p", parent, "-m", message)
	}
	if err != nil {
		return "", err
	}
	sha := trim(shaOut)
	if err := git(repo, "update-ref", localWipRef(device, branch), sha); err != nil {
		return "", err
	}
	return sha, nil
}

func pushWipSnapshot(repo, remote, branch, sha string) error {
	device := repoDeviceID(repo)
	refname := localWipRef(device, branch)
	if err := git(repo, "update-ref", refname, sha); err != nil {
		return err
	}
	remoteRef := fmt.Sprintf("refs/gitdcy/wip/%s/%s", device, sanitizeRefComponent(branch))
	return git(repo, "push", remote, refname+":"+remoteRef)
}

func fetchWipRefs(repo, remote string) error {
	return git(repo, "fetch", "--prune", remote, "+refs/gitdcy/wip/*:"+wipRemote+"/*")
}

func latestIncomingWip(repo, branch string) (*WipRef, error) {
	current := repoDeviceID(repo)
	refs, err := wipRefs(repo, branch, wipRemote)
	if err != nil {
		return nil, err
	}
	var best *WipRef
	for i := range refs {
		if refs[i].Device == current {
			continue
		}
		if best == nil || refs[i].Timestamp >= best.Timestamp {
			best = &refs[i]
		}
	}
	return best, nil
}

func localWip(repo, branch string) (*WipRef, error) {
	refs, err := wipRefs(repo, branch, wipHead)
	if err != nil {
		return nil, err
	}
	var best *WipRef
	for i := range refs {
		if best == nil || refs[i].Timestamp >= best.Timestamp {
			best = &refs[i]
		}
	}
	return best, nil
}

func wipRefs(repo, branch, prefix string) ([]WipRef, error) {
	out, err := gitOutput(repo, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	if err != nil {
		return nil, err
	}
	component := sanitizeRefComponent(branch)
	var refs []WipRef
	for _, line := range lines(out) {
		refname, sha, ok := splitOnce(line, " ")
		if !ok {
			continue
		}
		short, ok := strings.CutPrefix(refname, prefix+"/")
		if !ok {
			continue
		}
		device, refBranch, ok := splitOnce(short, "/")
		if !ok || refBranch != component {
			continue
		}
		ts, err := commitTimestamp(repo, sha)
		if err != nil {
			ts = 0
		}
		refs = append(refs, WipRef{Refname: refname, ShortName: short, Device: device, Branch: refBranch, SHA: sha, Timestamp: ts})
	}
	return refs, nil
}

func applyLatestIncomingWip(entry RepoEntry, branch string, config LocalConfig) (*WipRef, error) {
	repo := entry.Path
	wip, err := latestIncomingWip(repo, branch)
	if err != nil || wip == nil {
		return nil, err
	}
	if !wipDeviceTrustedWithConfig(entry, wip.Device, config) {
		return nil, errf("incoming WIP from untrusted device %s; approve it before syncing", wip.Device)
	}
	appliedRef := appliedWipRef(wip.Device, branch)
	if sha, err := gitOutput(repo, "rev-parse", "--verify", "--quiet", appliedRef); err == nil && trim(sha) == wip.SHA {
		return nil, nil
	}
	parentOut, err := gitOutput(repo, "rev-parse", wip.SHA+"^")
	if err != nil {
		return nil, err
	}
	parent := trim(parentOut)
	incoming, err := changedFilesBetween(repo, parent, wip.SHA)
	if err != nil {
		return nil, err
	}
	local, err := syncPathsWithConfig(entry, config)
	if err != nil {
		return nil, err
	}
	if len(local) > 0 {
		localFiles := map[string]bool{}
		for _, p := range local {
			localFiles[p.Path] = true
		}
		for _, p := range incoming {
			if localFiles[p] {
				return nil, errf("incoming WIP from %s also changes %s", wip.Device, p)
			}
		}
	}
	diff, err := gitBytes(repo, "diff", "--binary", parent, wip.SHA)
	if err != nil {
		return nil, err
	}
	if err := gitApply(repo, diff, true); err != nil {
		return nil, err
	}
	if err := gitApply(repo, diff, false); err != nil {
		return nil, err
	}
	if err := unstageIgnoredPaths(repo, incoming); err != nil {
		return nil, err
	}
	if err := git(repo, "update-ref", appliedRef, wip.SHA); err != nil {
		return nil, err
	}
	return wip, nil
}

func changedFilesBetween(repo, from, to string) ([]string, error) {
	out, err := gitOutput(repo, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range lines(out) {
		if line = trim(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func unstageIgnoredPaths(repo string, paths []string) error {
	var ignored []string
	for _, p := range paths {
		if gitIgnoredPath(repo, p) {
			ignored = append(ignored, p)
		}
	}
	if len(ignored) > 0 {
		return gitPaths(repo, []string{"reset", "-q"}, ignored)
	}
	return nil
}

func gitIgnoredPath(repo, path string) bool {
	out, err := gitCommandOutput(gitCommandWithPaths(repo, []string{"check-ignore", "--no-index", "-q"}, []string{path}))
	return err == nil && out.success()
}

func commitTimestamp(repo, sha string) (int64, error) {
	out, err := gitOutput(repo, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(trim(out), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func commitTree(repo, sha string) (string, error) {
	out, err := gitOutput(repo, "show", "-s", "--format=%T", sha)
	return trim(out), err
}

func gitApply(repo string, diff []byte, check bool) error {
	args := []string{"-C", repo, "apply", "--3way"}
	if check {
		args = append(args, "--check")
	}
	cmd := exec.Command("git", args...)
	cmd.Stdin = bytes.NewReader(diff)
	out, err := runOutput(cmd)
	if err != nil {
		return wrap(err, "spawn git apply")
	}
	if !out.success() {
		return errf("%s", commandError("git apply", out))
	}
	return nil
}

func tempIndexPath(repo string) (string, error) {
	out, err := gitOutput(repo, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	dir := trim(out)
	if !filepath.IsAbs(dir) {
		dir = joinPath(repo, dir)
	}
	return joinPath(dir, fmt.Sprintf("gitdcy-index-%d", time.Now().UnixNano())), nil
}

func repoID(path string, provider Provider) string {
	name, ok := fileName(path)
	if !ok {
		name = "repo"
	}
	return provider.folder() + "/" + name
}

func repoNameFromURL(url string) string {
	trimmed := strings.TrimRight(url, "/")
	last := trimmed
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		last = trimmed[i+1:]
	}
	if last == "" {
		last = "repo"
	}
	return strings.TrimSuffix(last, ".git")
}

func sanitizeComponent(value string) string {
	var b strings.Builder
	for _, c := range value {
		if isASCIIAlnum(c) || c == '.' || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func sanitizeRefComponent(value string) string {
	var b strings.Builder
	for _, c := range value {
		if isASCIIAlnum(c) || c == '.' || c == '-' || c == '_' || c == '/' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "/")
	s = strings.ReplaceAll(s, "//", "/")
	return trimSuffixes(s, ".lock")
}

func localWipRef(device, branch string) string {
	return wipHead + "/" + sanitizeRefComponent(device) + "/" + sanitizeRefComponent(branch)
}

func appliedWipRef(device, branch string) string {
	return wipApplied + "/" + sanitizeRefComponent(device) + "/" + sanitizeRefComponent(branch)
}

func wipRemoteName(rm map[string]string) (string, bool) {
	if _, ok := rm[syncRemote]; ok {
		return syncRemote, true
	}
	if url, ok := rm["origin"]; ok && providerFromURL(url) == ProviderForgejo {
		return "origin", true
	}
	return "", false
}

func shortSHA(sha string) string {
	r := []rune(sha)
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// deviceID is this device's name: GITDCY_DEVICE, COMPUTERNAME, HOSTNAME,
// else what `hostname` says.
func deviceID() string {
	raw := ""
	for _, name := range []string{"GITDCY_DEVICE", "COMPUTERNAME", "HOSTNAME"} {
		if v, ok := os.LookupEnv(name); ok {
			raw = v
			break
		}
	}
	if trim(raw) == "" {
		if readOnlyStatus() {
			raw = "read-only-status"
		} else if out, err := runOutput(exec.Command("hostname")); err == nil && utf8Valid(out.Stdout) {
			raw = string(out.Stdout)
		} else {
			raw = "device"
		}
	}
	return sanitizeRefComponent(trim(raw))
}

func utf8Valid(b []byte) bool { return lossy(b) == string(b) }

func repoDeviceID(repo string) string {
	if out, err := gitOutput(repo, "config", "--get", "gitdcy.device"); err == nil {
		if v := sanitizeRefComponent(trim(out)); v != "" {
			return v
		}
	}
	return deviceID()
}
