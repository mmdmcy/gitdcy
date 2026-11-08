package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestConfigDirIsWhereTheDirectoriesCratePutIt(t *testing.T) {
	home := func() (string, bool) { return "/home/u", true }
	none := func(string) (string, bool) { return "", false }
	xdg := func(value string) func(string) (string, bool) {
		return func(name string) (string, bool) { return value, name == "XDG_CONFIG_HOME" }
	}
	check := func(got string, ok bool, want string) {
		t.Helper()
		if !ok || got != want {
			t.Errorf("got %q (%v), want %q", got, ok, want)
		}
	}
	got, ok := configDirFrom("linux", none, home)
	check(got, ok, "/home/u/.config/gitdcy")
	got, ok = configDirFrom("linux", xdg("/etc/xdg-u"), home)
	check(got, ok, "/etc/xdg-u/gitdcy")
	// A relative or empty XDG_CONFIG_HOME is ignored, as the spec says.
	got, ok = configDirFrom("linux", xdg("rel/conf"), home)
	check(got, ok, "/home/u/.config/gitdcy")
	got, ok = configDirFrom("linux", xdg(""), home)
	check(got, ok, "/home/u/.config/gitdcy")
	// No home folder: no config folder, even with XDG_CONFIG_HOME set.
	if _, ok := configDirFrom("linux", xdg("/etc/xdg-u"), func() (string, bool) { return "", false }); ok {
		t.Error("a config folder without a home")
	}
	got, ok = configDirFrom("darwin", none, home)
	check(got, ok, "/home/u/Library/Application Support/dev.gitdcy.GitDCY")
}

func TestReadOnlyStatusWorkersPreserveManifestOrder(t *testing.T) {
	manifest := WorkspaceManifest{WorkspaceRoot: "/nonexistent", Repos: []RepoEntry{
		testEntry("first", "/nonexistent/first"), testEntry("second", "/nonexistent/second"),
	}}
	statuses := statusAllForMode(manifest, true)
	if len(statuses) != 2 || statuses[0].Entry.ID != "first" || statuses[1].Entry.ID != "second" {
		t.Fatalf("%+v", statuses)
	}
	for _, s := range statuses {
		if s.LastError == nil {
			t.Errorf("%s has no error", s.Entry.ID)
		}
	}
}

func TestReadOnlyFallbackDiscoveryIsOneLevel(t *testing.T) {
	root := t.TempDir()
	direct := filepath.Join(root, "direct")
	nested := filepath.Join(root, "group", "nested")
	must(t, os.MkdirAll(filepath.Join(direct, ".git"), 0o755))
	must(t, os.MkdirAll(filepath.Join(nested, ".git"), 0o755))
	found, err := discoverRepoPathsWithBudget(root, newDiscoveryBudget(true))
	must(t, err)
	if !reflect.DeepEqual(found, []string{direct}) {
		t.Fatalf("%v", found)
	}
}

func TestRepositoryDiscoveryDoesNotFollowDirectorySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	must(t, os.MkdirAll(filepath.Join(repository, ".git"), 0o755))
	must(t, os.Symlink(root, filepath.Join(root, "loop")))
	found, err := discoverRepoPaths(root)
	must(t, err)
	if !reflect.DeepEqual(found, []string{repository}) {
		t.Fatalf("%v", found)
	}
}

func TestParsesPorcelainTrackedAndNewPaths(t *testing.T) {
	parsed := parsePorcelainV2Z([]byte("1 .M N... 100644 100644 100644 abc abc src/main.rs\x00? src/new.rs\x00"))
	want := []ChangedPath{{"src/main.rs", ChangeTracked}, {"src/new.rs", ChangeNew}}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("%+v", parsed)
	}
}

func TestDerivesRepoNameFromCommonURLs(t *testing.T) {
	if got := repoNameFromURL("https://github.com/example/gitdcy.git"); got != "gitdcy" {
		t.Error(got)
	}
	if got := repoNameFromURL("git@gitlab.com:example/orbit.git"); got != "orbit" {
		t.Error(got)
	}
}

func TestRoutesProvidersFromURLs(t *testing.T) {
	for url, want := range map[string]Provider{
		"https://github.com/a/b.git":             ProviderGithub,
		"git@github-special:owner/example.git":   ProviderGithub,
		"git@gitlab.com:a/b.git":                 ProviderGitlab,
		"ssh://git@forgejo.example/a/b.git":      ProviderForgejo,
		"ssh://git@example.internal/a/b.git":     ProviderOther,
		"https://user@GitHub.com/Owner/repo.git": ProviderGithub,
	} {
		if got := providerFromURL(url); got != want {
			t.Errorf("%s: %s, want %s", url, got, want)
		}
	}
}

func TestRemoteIdentitySelectorMatchesHostAliasNotAnUnrelatedPathFragment(t *testing.T) {
	profile := GitIdentityProfile{Name: "Profile", Email: "profile@example.invalid", RemotePatterns: []string{"github-special"}, Providers: []Provider{ProviderGithub}}
	wrong := map[string]string{"origin": "ssh://git@other-host/team/github-special.git"}
	if identityProfileMatches(profile, "/tmp/repo", ProviderGithub, wrong) {
		t.Error("matched an unrelated path fragment")
	}
	right := map[string]string{"origin": "git@github-special:owner/repo.git"}
	if !identityProfileMatches(profile, "/tmp/repo", ProviderGithub, right) {
		t.Error("did not match the host alias")
	}
}

func TestIdentityProfilesDeserializeFromLocalTOML(t *testing.T) {
	var config LocalConfig
	must(t, tomlUnmarshal("require_identity = true\n[identity_profiles.forgejo-selfhosted]\nname = \"Forgejo User\"\nemail = \"forgejo@example.invalid\"\nproviders = [\"forgejo\"]\n", &config))
	if config.RequireIdentity == nil || !*config.RequireIdentity {
		t.Fatal("require_identity")
	}
	p := (*config.IdentityProfiles)["forgejo-selfhosted"]
	if p.Name != "Forgejo User" || p.Email != "forgejo@example.invalid" || !reflect.DeepEqual(p.Providers, []Provider{ProviderForgejo}) {
		t.Fatalf("%+v", p)
	}
}

func TestIncompleteIdentityProfileIsInvalidAndFailClosed(t *testing.T) {
	var config LocalConfig
	must(t, tomlUnmarshal("require_identity = true\n[identity_profiles.incomplete]\nproviders = [\"forgejo\"]\n", &config))
	report := identityReportWithConfig(testEntry("forgejo/incomplete", "/tmp/incomplete"), map[string]string{}, config)
	if report.State != IdentityInvalid || !report.isBlocking() {
		t.Fatalf("%+v", report)
	}
}

func TestIdentityProfileRequiresAllDeclaredSelectorsAndMatchesGitIdentity(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	config := LocalConfig{RequireIdentity: ptr(true), IdentityProfiles: &map[string]GitIdentityProfile{
		"fixture-github": {Name: "GitDCY Test", Email: "gitdcy@example.invalid", PathPrefixes: []string{f.root}, RemotePatterns: []string{"origin"}, Providers: []Provider{ProviderGithub}},
	}}
	rm, err := remotes(repo)
	must(t, err)
	report := identityReportWithConfig(testEntry("github/fixture", repo), rm, config)
	if report.State != IdentityMatched || report.isBlocking() || report.Profile == nil || *report.Profile != "fixture-github" {
		t.Fatalf("%+v", report)
	}
	if report.expectedDisplay() != "GitDCY Test <gitdcy@example.invalid>" || report.actualDisplay() != "GitDCY Test <gitdcy@example.invalid>" {
		t.Fatalf("%s / %s", report.expectedDisplay(), report.actualDisplay())
	}
}

func TestIdentityProfileMismatchBlocksSyncBeforeWorktreeActions(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	config := LocalConfig{RequireIdentity: ptr(true), IdentityProfiles: &map[string]GitIdentityProfile{
		"wrong-profile": {Name: "Wrong User", Email: "wrong@example.invalid", PathPrefixes: []string{f.root}, RemotePatterns: []string{"origin"}, Providers: []Provider{ProviderGithub}},
	}}
	report := syncWithConfig(testEntry("github/fixture", repo), config)
	if report.Blocked == nil || !strings.Contains(*report.Blocked, "identity check blocked") {
		t.Fatalf("%+v", report)
	}
	if len(report.Actions) != 0 {
		t.Fatalf("identity failure must happen before sync actions: %+v", report)
	}
}

func TestIdentityProfileAmbiguityIsBlocking(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	profile := GitIdentityProfile{Name: "GitDCY Test", Email: "gitdcy@example.invalid", PathPrefixes: []string{f.root}, RemotePatterns: []string{"origin"}, Providers: []Provider{ProviderGithub}}
	config := LocalConfig{RequireIdentity: ptr(true), IdentityProfiles: &map[string]GitIdentityProfile{"first": profile, "second": profile}}
	rm, err := remotes(repo)
	must(t, err)
	report := identityReportWithConfig(testEntry("github/fixture", repo), rm, config)
	if report.State != IdentityAmbiguous || !report.isBlocking() || !reflect.DeepEqual(report.Candidates, []string{"first", "second"}) {
		t.Fatalf("%+v", report)
	}
}

func TestForgejoProviderProfileCoversReposWithoutAnOriginRemote(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	gitRun(t, repo, "remote", "remove", "origin")
	config := LocalConfig{RequireIdentity: ptr(true), IdentityProfiles: &map[string]GitIdentityProfile{
		"forgejo-selfhosted": {Name: "GitDCY Test", Email: "gitdcy@example.invalid", Providers: []Provider{ProviderForgejo}},
	}}
	entry := testEntry("forgejo/fixture", repo)
	entry.Provider = ProviderForgejo
	report := identityReportWithConfig(entry, map[string]string{}, config)
	if report.State != IdentityMatched || report.isBlocking() {
		t.Fatalf("%+v", report)
	}
}

func TestWipSnapshotUsesTheSelectedIdentityExplicitly(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("profile-owned WIP\n"), 0o644))
	dirty, err := dirtyPaths(repo)
	must(t, err)
	profile := GitIdentityProfile{Name: "WIP Profile", Email: "wip-profile@example.invalid"}
	sha, err := createWipSnapshot(repo, "main", dirty, &profile)
	must(t, err)
	out, err := gitOutput(repo, "show", "-s", "--format=%an <%ae> %cn <%ce>", sha)
	must(t, err)
	if trim(out) != "WIP Profile <wip-profile@example.invalid> WIP Profile <wip-profile@example.invalid>" {
		t.Fatal(out)
	}
}

func TestCheckProfilesDeserializeWithDirectCICommands(t *testing.T) {
	var config LocalConfig
	must(t, tomlUnmarshal(`[check_profiles.rust-project]
path_prefixes = ["~/Documents/example"]
remote_patterns = ["github.com/example/project"]

[[check_profiles.rust-project.checks]]
name = "format"
program = "cargo"
args = ["fmt", "--all", "--check"]
timeout_seconds = 900
`, &config))
	p := (*config.CheckProfiles)["rust-project"]
	if !p.RunBeforePush || !p.RequireCleanWorktree {
		t.Fatalf("defaults: %+v", p)
	}
	if p.Checks[0].displayCommand() != "cargo fmt --all --check" || p.Checks[0].TimeoutSeconds == nil || *p.Checks[0].TimeoutSeconds != 900 {
		t.Fatalf("%+v", p.Checks[0])
	}
}

func TestConfiguredChecksRunAndReportFailuresWithoutAShell(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	profile := defaultCheckProfile()
	profile.PathPrefixes, profile.RemotePatterns, profile.Providers = []string{f.root}, []string{"origin"}, []Provider{ProviderGithub}
	profile.Checks = []CheckCommand{{Name: "git-status", Program: "git", Args: []string{"status", "--porcelain"}}}
	config := LocalConfig{CheckProfiles: &map[string]CheckProfile{"checks": profile}}
	entry := testEntry("github/fixture", repo)
	rm, err := remotes(repo)
	must(t, err)
	report := checkRepoWithConfig(entry, rm, config, false)
	if report.State != CheckPassed || report.isBlocking() || len(report.Results) != 1 || report.Results[0].State != ResultPassed || report.Results[0].Command != "git status --porcelain" {
		t.Fatalf("%+v", report)
	}
	failing := defaultCheckProfile()
	failing.PathPrefixes, failing.RemotePatterns, failing.Providers = []string{f.root}, []string{"origin"}, []Provider{ProviderGithub}
	failing.Checks = []CheckCommand{{Name: "missing-branch", Program: "git", Args: []string{"rev-parse", "--verify", "refs/heads/branch-that-does-not-exist"}}}
	failingReport := checkRepoWithConfig(entry, rm, LocalConfig{CheckProfiles: &map[string]CheckProfile{"checks": failing}}, false)
	if failingReport.State != CheckFailed || !failingReport.isBlocking() || trim(failingReport.Results[0].Output) == "" {
		t.Fatalf("%+v", failingReport)
	}
}

func TestPushChecksFailClosedOnADirtyWorktree(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.WriteFile(filepath.Join(repo, "pending.txt"), []byte("not committed\n"), 0o644))
	profile := defaultCheckProfile()
	profile.PathPrefixes, profile.RemotePatterns, profile.Providers = []string{f.root}, []string{"origin"}, []Provider{ProviderGithub}
	profile.Checks = []CheckCommand{{Name: "status", Program: "git", Args: []string{"status", "--porcelain"}}}
	rm, err := remotes(repo)
	must(t, err)
	report := checkRepoWithConfig(testEntry("github/fixture", repo), rm, LocalConfig{CheckProfiles: &map[string]CheckProfile{"checks": profile}}, true)
	if report.State != CheckFailed || !report.isBlocking() || report.WorktreeClean == nil || *report.WorktreeClean || len(report.Results) != 0 || !strings.Contains(report.Message, "not clean") {
		t.Fatalf("%+v", report)
	}
}

// twoClones is a fixture with two clones that share a sync remote, each
// with its own device name.
func twoClones(t *testing.T) (*gitFixture, string, string) {
	f := newGitFixture(t)
	first, second := f.cloneRepo(t, "first"), f.cloneRepo(t, "second")
	for repo, device := range map[string]string{first: "first-device", second: "second-device"} {
		gitRun(t, repo, "remote", "add", syncRemote, f.remote)
		gitRun(t, repo, "config", "gitdcy.device", device)
	}
	return f, first, second
}

func TestDirtyWipMovesBetweenClonesWithoutIgnoredFiles(t *testing.T) {
	_, first, second := twoClones(t)
	must(t, os.WriteFile(filepath.Join(first, "README.md"), []byte("changed on first\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(first, "new-source.rs"), []byte("fn main() {}\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(first, "node_modules/pkg"), 0o755))
	must(t, os.WriteFile(filepath.Join(first, "node_modules/pkg/ignored.js"), []byte("ignored\n"), 0o644))
	config := configTrusting("github/fixture", "first-device")
	if r := syncWithConfig(testEntry("github/fixture", first), config); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	if r := syncWithConfig(testEntry("github/fixture", second), config); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	if readFile(t, filepath.Join(second, "README.md")) != "changed on first\n" {
		t.Fatal("README not moved")
	}
	if _, err := os.Stat(filepath.Join(second, "new-source.rs")); err != nil {
		t.Fatal("new file not moved")
	}
	if _, err := os.Stat(filepath.Join(second, "node_modules/pkg/ignored.js")); err == nil {
		t.Fatal("ignored file moved")
	}
	dirty, err := dirtyPaths(second)
	must(t, err)
	if !hasPath(dirty, "new-source.rs") {
		t.Fatalf("%+v", dirty)
	}
}

func TestLocalAllowlistMovesIgnoredEnvFile(t *testing.T) {
	_, first, second := twoClones(t)
	must(t, os.WriteFile(filepath.Join(first, ".env"), []byte("APP_SECRET=local-only\n"), 0o644))
	dirty, err := dirtyPaths(first)
	must(t, err)
	if hasPath(dirty, ".env") {
		t.Fatal(".env counted as dirty")
	}
	config := configTrusting("github/fixture", "first-device")
	config.LocalSyncFiles = &map[string][]string{"github/fixture": {".env"}}
	firstEntry, secondEntry := testEntry("github/fixture", first), testEntry("github/fixture", second)
	paths, err := syncPathsWithConfig(firstEntry, config)
	must(t, err)
	local := false
	for _, p := range paths {
		local = local || (p.Path == ".env" && p.Kind == ChangeLocal)
	}
	if !local {
		t.Fatalf("%+v", paths)
	}
	if r := syncWithConfig(firstEntry, config); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	if r := syncWithConfig(secondEntry, config); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	if readFile(t, filepath.Join(second, ".env")) != "APP_SECRET=local-only\n" {
		t.Fatal(".env not moved")
	}
	dirty, err = dirtyPaths(second)
	must(t, err)
	if hasPath(dirty, ".env") {
		t.Fatal(".env staged on the second clone")
	}
}

func TestIncomingWipBlocksOnSameDirtyFile(t *testing.T) {
	_, first, second := twoClones(t)
	must(t, os.WriteFile(filepath.Join(first, "README.md"), []byte("changed on first\n"), 0o644))
	config := configTrusting("github/fixture", "first-device")
	if r := syncWithConfig(testEntry("github/fixture", first), config); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	must(t, os.WriteFile(filepath.Join(second, "README.md"), []byte("changed on second\n"), 0o644))
	r := syncWithConfig(testEntry("github/fixture", second), config)
	if r.Blocked == nil || !strings.Contains(*r.Blocked, "README.md") {
		t.Fatalf("%+v", r)
	}
	if readFile(t, filepath.Join(second, "README.md")) != "changed on second\n" {
		t.Fatal("local change overwritten")
	}
}

func TestIncomingWipWaitsForDeviceTrust(t *testing.T) {
	_, first, second := twoClones(t)
	firstEntry, secondEntry := testEntry("github/fixture", first), testEntry("github/fixture", second)
	must(t, os.WriteFile(filepath.Join(first, "README.md"), []byte("changed on first\n"), 0o644))
	if r := syncWithConfig(firstEntry, LocalConfig{}); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	blocked := syncWithConfig(secondEntry, LocalConfig{})
	if blocked.Blocked == nil || !strings.Contains(*blocked.Blocked, "untrusted device first-device") {
		t.Fatalf("%+v", blocked)
	}
	if readFile(t, filepath.Join(second, "README.md")) != "base\n" {
		t.Fatal("applied before trust")
	}
	if r := syncWithConfig(secondEntry, configTrusting("*", "first-device")); r.Blocked != nil {
		t.Fatalf("%+v", r)
	}
	if readFile(t, filepath.Join(second, "README.md")) != "changed on first\n" {
		t.Fatal("not applied after trust")
	}
}

func auditPaths(r RepoSafetyReport) map[string]bool {
	set := map[string]bool{}
	for _, f := range r.Findings {
		if f.Path != nil {
			set[*f.Path] = true
		}
	}
	return set
}

func TestPublicAuditBlocksAgentNotesAndPrivateEnvFiles(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, ".env.local"), []byte("SECRET=value\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, ".env.example"), []byte("SECRET=\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md", ".env.local", ".env.example")
	report, err := auditRepoWithConfig(testEntry("github/fixture", repo), LocalConfig{})
	must(t, err)
	paths := auditPaths(report)
	if !paths["AGENTS.md"] || !paths[".env.local"] || paths[".env.example"] {
		t.Fatalf("%+v", report)
	}
}

func TestPrivateOverrideAllowsAgentNotes(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md")
	config := LocalConfig{VisibilityOverrides: &map[string]VisibilityOverride{"github/fixture": "private"}}
	report, err := auditRepoWithConfig(testEntry("github/fixture", repo), config)
	must(t, err)
	if auditPaths(report)["AGENTS.md"] {
		t.Fatalf("%+v", report)
	}
}

func TestGeneratedCachePathsAreBlockedEvenForPrivateRepos(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.MkdirAll(filepath.Join(repo, "node_modules/pkg"), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, "node_modules/pkg/index.js"), []byte("generated\n"), 0o644))
	gitRun(t, repo, "add", "-f", "node_modules/pkg/index.js")
	config := LocalConfig{VisibilityOverrides: &map[string]VisibilityOverride{"github/fixture": "private"}}
	report, err := auditRepoWithConfig(testEntry("github/fixture", repo), config)
	must(t, err)
	if !auditPaths(report)["node_modules/pkg/index.js"] {
		t.Fatalf("%+v", report)
	}
}

func TestDeletingBlockedPathsIsAllowedInStagedAudit(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md")
	gitRun(t, repo, "commit", "-m", "add agent notes")
	must(t, os.Remove(filepath.Join(repo, "AGENTS.md")))
	gitRun(t, repo, "add", "-A")
	report, err := auditStagedWithConfig(testEntry("github/fixture", repo), LocalConfig{})
	must(t, err)
	if report.hasFatalFindings() {
		t.Fatalf("%+v", report)
	}
}

func TestPublicRemoteNameMarksRepoPublicTargeted(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	gitRun(t, repo, "remote", "add", "public", f.remote)
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md")
	report, err := auditRepoWithConfig(testEntry("github/fixture", repo), LocalConfig{})
	must(t, err)
	if report.Visibility != VisibilityPublic || !report.hasFatalFindings() {
		t.Fatalf("%+v", report)
	}
}

func TestLocalOnlyRepoIsPrivateByDefault(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	gitRun(t, repo, "remote", "remove", "origin")
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md")
	report, err := auditRepoWithConfig(testEntry("github/fixture", repo), LocalConfig{})
	must(t, err)
	if report.Visibility != VisibilityPrivate || report.hasFatalFindings() {
		t.Fatalf("%+v", report)
	}
}

func TestForgejoStyleRemoteIsPrivateByDefault(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	gitRun(t, repo, "remote", "set-url", "origin", "ssh://git@forgejo-easy/nerv/repo.git")
	must(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("private agent routing\n"), 0o644))
	gitRun(t, repo, "add", "-f", "AGENTS.md")
	report, err := auditRepoWithConfig(testEntry("forgejo/fixture", repo), LocalConfig{})
	must(t, err)
	if report.Visibility != VisibilityPrivate || report.hasFatalFindings() {
		t.Fatalf("%+v", report)
	}
}

func TestPolicyReportAndApplyRepoIgnoreBlockAreIdempotent(t *testing.T) {
	f := newGitFixture(t)
	repo := f.cloneRepo(t, "repo")
	entry := testEntry("github/fixture", repo)
	report, err := policyReportWithConfig(entry, LocalConfig{})
	must(t, err)
	if report.count(PolicyDrift) == 0 {
		t.Fatalf("%+v", report)
	}
	must(t, writeRepoIgnoreBlock(repo, report.Policy.IgnoreRules))
	after := readFile(t, filepath.Join(repo, ".gitignore"))
	must(t, writeRepoIgnoreBlock(repo, report.Policy.IgnoreRules))
	if again := readFile(t, filepath.Join(repo, ".gitignore")); again != after {
		t.Fatalf("not idempotent:\n%s\n---\n%s", after, again)
	}
	report, err = policyReportWithConfig(entry, LocalConfig{})
	must(t, err)
	if report.count(PolicyDrift) != 0 {
		t.Fatalf("%+v", report)
	}
}

func TestManagedIgnoreBlockIsIdempotent(t *testing.T) {
	once := mergeManagedIgnoreBlock("# user rule\n*.tmp\n")
	twice := mergeManagedIgnoreBlock(once)
	if once != twice || !strings.Contains(once, "AGENTS.md") || !strings.Contains(once, "node_modules/") || strings.Contains(once, "\ndata/\n") {
		t.Fatalf("%q\n%q", once, twice)
	}
}

// ---------------------------------------------------------------------------
// Fixtures

type gitFixture struct {
	root   string
	remote string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	remote := filepath.Join(root, "remote.git")
	runAt(t, root, "git", "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runAt(t, root, "git", "init", seed)
	configureUser(t, seed)
	must(t, os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("node_modules/\ntarget/\ndist/\n.env\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(seed, "README.md"), []byte("base\n"), 0o644))
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "initial")
	gitRun(t, seed, "branch", "-M", "main")
	gitRun(t, seed, "remote", "add", "origin", remote)
	gitRun(t, seed, "push", "-u", "origin", "main")
	return &gitFixture{root: root, remote: remote}
}

func (f *gitFixture) cloneRepo(t *testing.T, name string) string {
	t.Helper()
	destination := filepath.Join(f.root, name)
	runAt(t, f.root, "git", "clone", "-b", "main", f.remote, destination)
	configureUser(t, destination)
	return destination
}

func testEntry(id, path string) RepoEntry {
	return RepoEntry{ID: id, Path: path, Provider: ProviderGithub, Enabled: true, PrimaryRemote: ptr("origin"), WipSync: true}
}

func syncWithConfig(entry RepoEntry, config LocalConfig) SyncReport {
	report := SyncReport{RepoID: entry.ID}
	if err := syncRepoWithConfig(entry, &report, config); err != nil {
		report.block(err.Error())
	}
	return report
}

func configTrusting(repoID string, devices ...string) LocalConfig {
	return LocalConfig{TrustedWipDevices: &map[string][]string{repoID: devices}}
}

func configureUser(t *testing.T, repo string) {
	gitRun(t, repo, "config", "user.name", "GitDCY Test")
	gitRun(t, repo, "config", "user.email", "gitdcy@example.invalid")
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	runAt(t, repo, "git", append([]string{"-C", repo}, args...)...)
}

func runAt(t *testing.T, dir, program string, args ...string) {
	t.Helper()
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("command failed: %s %v\n%s", program, args, out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}

func hasPath(paths []ChangedPath, path string) bool {
	for _, p := range paths {
		if p.Path == path {
			return true
		}
	}
	return false
}
