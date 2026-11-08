// Command gitdcy is GitDCY's command line: a private Git-only multi-device
// workspace client. It keeps repositories native on each device, syncs them
// through Git, and holds each repository to its rules.
package main

import (
	"fmt"
	"os"
	"strings"
)

// cli is the command line: every subcommand, argument and help text.
func cli() *cliCmd {
	all := func() *cliArg { return flagArg("all", "all") }
	repo := func() *cliArg { return posArg("repo", "REPO") }
	return newCmd("gitdcy").About("Private Git-only multi-device workspace client").SubRequired().ElseHelp().Sub(
		newCmd("doctor"),
		newCmd("status").Arg(formatArg()),
		newCmd("check").Arg(all(), repo()),
		newCmd("dashboard"),
		newCmd("identity").SubRequired().ElseHelp().Sub(newCmd("status").Arg(all(), repo())),
		newCmd("audit").Arg(all(), flagArg("explain", "explain"), repo()),
		newCmd("policy").SubRequired().ElseHelp().Sub(
			newCmd("status").Arg(all(), repo()),
			newCmd("plan").Arg(repo().Required()),
			newCmd("apply").Arg(repo().Required()),
		),
		newCmd("install-ignore").Arg(flagArg("global", "global")),
		newCmd("sync").Arg(all(), repo()),
		newCmd("commit").Arg(repo().Required(), optArg("message", "message", "MESSAGE").Short('m').Required()),
		newCmd("push").Arg(repo().Required()),
		newCmd("set-sync-remote").Arg(repo().Required(), posArg("url", "URL").Required()),
		newCmd("set-origin-remote").Arg(repo().Required(), posArg("url", "URL")),
		newCmd("trust-device").Arg(posArg("device", "DEVICE").Required(), optArg("repo", "repo", "REPO"), all()),
		newCmd("clone").Arg(
			posArg("url", "URL").Required(),
			optArg("root", "root", "ROOT").Path(),
			optArg("provider", "provider", "PROVIDER").Possible("github", "forgejo", "gitlab", "other"),
			optArg("name", "name", "NAME"),
		),
		newCmd("save-manifest"),
		rulesCommand().About("Each repository's rules: where it may be pushed, branches that take only a pull request, and the personal-data check before a push"),
		checkoutsCommand().About("Checkouts against their upstream: up to date, behind, ahead, diverged, local changes or missing; `update` fast-forwards only what is safe"),
		relateCommand().About("How two commits of a repository relate: equal, ahead, behind, diverged or unrelated, with the commits only on each side counted"),
		carryCommand().About("Moving work between two checkouts of a repository: send it, place it (branches only where the name is free), set it aside"),
	)
}

func main() {
	convertYAMLSettings()
	top := runCLI(cli())
	if err := run(top.subName, top.sub); err != nil {
		fail(err)
	}
}

// run runs the subcommand name with its matches m.
func run(name string, m *cliMatches) error {
	opt := m.optStr
	given := func(id string) string { v, _ := m.str(id); return v }
	switch name {
	case "doctor":
		return doctor()
	case "status":
		return status(isJSON(m))
	case "check":
		return check(m.flag("all"), opt("repo"))
	case "dashboard":
		return runDashboard()
	case "identity":
		return identityStatus(m.sub.flag("all"), m.sub.optStr("repo"))
	case "audit":
		return audit(m.flag("all"), m.flag("explain"), opt("repo"))
	case "policy":
		return policy(m.subName, m.sub)
	case "install-ignore":
		return installIgnore(m.flag("global"))
	case "sync":
		return syncCommand(m.flag("all"), opt("repo"))
	case "commit":
		entry, err := findRepo(given("repo"))
		if err != nil {
			return err
		}
		if err := commit(entry.Path, given("message"), nil); err != nil {
			return err
		}
		fmt.Printf("committed %s\n", entry.ID)
	case "push":
		entry, err := findRepo(given("repo"))
		if err != nil {
			return err
		}
		if err := push(entry.Path); err != nil {
			return err
		}
		fmt.Printf("pushed %s\n", entry.ID)
	case "set-sync-remote":
		entry, err := findRepo(given("repo"))
		if err != nil {
			return err
		}
		if err := setRemote(entry.Path, syncRemote, given("url")); err != nil {
			return err
		}
		fmt.Printf("set sync remote for %s\n", entry.ID)
	case "set-origin-remote":
		entry, err := findRepo(given("repo"))
		if err != nil {
			return err
		}
		var url string
		if u := opt("url"); u != nil {
			url = *u
			err = setRemote(entry.Path, "origin", url)
		} else {
			url, err = setSuggestedOriginRemote(entry)
		}
		if err != nil {
			return err
		}
		fmt.Printf("set origin for %s -> %s\n", entry.ID, url)
	case "trust-device":
		return trustDevice(given("device"), opt("repo"), m.flag("all"))
	case "clone":
		root := opt("root")
		request := CloneRequest{URL: given("url"), Name: opt("name")}
		if root != nil {
			request.WorkspaceRoot = *root
		} else {
			request.WorkspaceRoot = defaultWorkspaceRoot()
		}
		if p := opt("provider"); p != nil {
			provider := Provider(*p)
			request.Provider = &provider
		}
		destination, err := cloneRepo(request)
		if err != nil {
			return err
		}
		fmt.Println(destination)
	case "save-manifest":
		manifest, err := loadOrDiscoverManifest()
		if err != nil {
			return err
		}
		if err := saveManifest(manifest); err != nil {
			return err
		}
		fmt.Printf("saved manifest with %d repos\n", len(manifest.Repos))
	case "rules":
		return runRules(m)
	case "checkouts":
		return runCheckouts(m)
	case "relate":
		return runRelate(m)
	case "carry":
		return runCarry(m)
	}
	return nil
}

func trustDevice(device string, repo *string, all bool) error {
	if all && repo != nil {
		return errf("use either --all or --repo, not both")
	}
	scope := "all repos"
	if !all {
		if repo == nil {
			return errf("pass --all or --repo <repo>")
		}
		scope = *repo
	}
	var path string
	var err error
	if all {
		path, err = setWipDeviceTrustedGlobally(device, true)
	} else {
		var entry RepoEntry
		if entry, err = findRepo(*repo); err == nil {
			path, err = setWipDeviceTrusted(entry, device, true)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("trusted %s for %s (%s)\n", device, scope, path)
	return nil
}

func doctor() error {
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	fmt.Printf("workspace root: %s\n", manifest.WorkspaceRoot)
	fmt.Printf("repos: %d\n", len(manifest.Repos))
	path, err := manifestPath()
	if err != nil {
		return err
	}
	fmt.Printf("config: %s\n", path)
	for _, note := range configNotes() {
		fmt.Printf("config note: %s\n", note)
	}
	fmt.Printf("device: %s\n", deviceID())
	fmt.Println()
	statuses := statusAll(manifest)
	review, blocked := 0, 0
	for _, s := range statuses {
		if s.Entry.ReviewRequired || s.LastError != nil || s.Identity.isBlocking() {
			review++
		}
		if s.Identity.isBlocking() {
			blocked++
		}
	}
	fmt.Printf("review required: %d\n", review)
	fmt.Printf("identity blocked: %d\n", blocked)
	for _, s := range statuses {
		if s.Entry.ReviewRequired || s.LastError != nil || s.Identity.isBlocking() {
			fmt.Printf("- %s | %s | %s\n", s.Entry.ID, s.Path, s.shortState())
		}
	}
	return nil
}

// selectRepo is the repository named by id or by its last part.
func matchesRepo(id, name string) bool {
	return id == name || strings.HasSuffix(id, "/"+name)
}

func identityStatus(all bool, repo *string) error {
	if all && repo != nil {
		return errf("use either --all or a repo name, not both")
	}
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	profiles := 0
	if p := loadLocalConfig().IdentityProfiles; p != nil {
		profiles = len(*p)
	}
	fmt.Printf("configured identity profiles: %d\n", profiles)
	statuses := statusAll(manifest)
	var selected []RepoStatus
	if all {
		selected = statuses
	} else {
		if repo == nil {
			return errf("pass --all or a repo id/name")
		}
		found := false
		for _, s := range statuses {
			if matchesRepo(s.Entry.ID, *repo) {
				selected, found = []RepoStatus{s}, true
				break
			}
		}
		if !found {
			return errf("repo not found: %s", *repo)
		}
	}
	blocked := 0
	for _, s := range selected {
		printIdentityStatus(s)
		if s.Identity.isBlocking() {
			blocked++
		}
	}
	if blocked > 0 {
		return errf("identity check found %d blocked repo%s", blocked, plural(blocked))
	}
	return nil
}

func printIdentityStatus(s RepoStatus) {
	id := s.Identity
	profile := "-"
	if id.Profile != nil {
		profile = *id.Profile
	}
	fmt.Printf("%s | state=%s | enforced=%t | profile=%s | expected=%s | actual=%s\n",
		s.Entry.ID, id.stateLabel(), id.EnforcementEnabled, profile, id.expectedDisplay(), id.actualDisplay())
	if !sameOpt(id.CommitterName, id.ActualName) || !sameOpt(id.CommitterEmail, id.ActualEmail) {
		fmt.Printf("  committer: %s\n", id.committerDisplay())
	}
	if len(id.EnvironmentOverrides) > 0 {
		fmt.Printf("  environment overrides: %s\n", strings.Join(id.EnvironmentOverrides, ", "))
	}
	if id.stateLabel() != "identity-ok" {
		fmt.Printf("  %s\n", id.Message)
	}
}

func sameOpt(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func findInManifest(manifest WorkspaceManifest, repo *string) (RepoEntry, error) {
	if repo == nil {
		return RepoEntry{}, errf("pass --all or a repo id/name")
	}
	for _, e := range manifest.Repos {
		if matchesRepo(e.ID, *repo) {
			return e, nil
		}
	}
	return RepoEntry{}, errf("repo not found: %s", *repo)
}

func check(all bool, repo *string) error {
	if all && repo != nil {
		return errf("use either --all or a repo name, not both")
	}
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	failures := 0
	if all {
		for _, e := range manifest.Repos {
			if !e.Enabled {
				continue
			}
			report := checkRepo(e)
			printCheckReport(e, report)
			if report.isBlocking() {
				failures++
			}
		}
	} else {
		e, err := findInManifest(manifest, repo)
		if err != nil {
			return err
		}
		report := checkRepo(e)
		printCheckReport(e, report)
		if report.isBlocking() {
			failures++
		}
	}
	if failures > 0 {
		return errf("checks blocked %d repo%s", failures, plural(failures))
	}
	return nil
}

func printCheckReport(e RepoEntry, r RepoCheckReport) {
	profile := "-"
	if r.Profile != nil {
		profile = *r.Profile
	}
	worktree := "unknown"
	if r.WorktreeClean != nil {
		worktree = "dirty"
		if *r.WorktreeClean {
			worktree = "clean"
		}
	}
	fmt.Printf("%s | state=%s | profile=%s | worktree=%s\n", e.ID, r.stateLabel(), profile, worktree)
	for _, result := range r.Results {
		fmt.Printf("  [%s] %s (%d ms): %s\n", result.State, result.Name, result.DurationMS, result.Command)
		if result.State != ResultPassed && trim(result.Output) != "" {
			for _, line := range lines(result.Output) {
				fmt.Printf("    %s\n", line)
			}
		}
	}
	fmt.Printf("  %s\n", r.Message)
}

func audit(all, explain bool, repo *string) error {
	if all && repo != nil {
		return errf("use either --all or a repo name, not both")
	}
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	fatal := 0
	if all {
		for _, r := range auditAll(manifest) {
			if r.err != nil {
				fatal++
				fmt.Printf("%s: audit failed: %s\n", r.entry.ID, r.err)
				continue
			}
			printAuditReport(r.entry, r.report, explain)
			fatal += r.report.fatalCount()
		}
	} else {
		e, err := findInManifest(manifest, repo)
		if err != nil {
			return err
		}
		report, err := auditRepo(e)
		if err != nil {
			return err
		}
		printAuditReport(e, report, explain)
		fatal += report.fatalCount()
	}
	if fatal > 0 {
		return errf("audit found %d blocking finding%s", fatal, plural(fatal))
	}
	return nil
}

func printAuditReport(e RepoEntry, r RepoSafetyReport, explain bool) {
	fmt.Printf("%s | visibility=%s | %s\n", e.ID, r.Visibility.label(), r.shortState())
	if len(r.Findings) == 0 {
		return
	}
	fmt.Println(formatAuditBlock(e, r))
	if explain {
		for _, f := range r.Findings {
			path := "-"
			if f.Path != nil {
				path = *f.Path
			}
			fmt.Printf("  fix %s: %s\n", path, f.Remediation)
		}
	}
}

func policy(name string, m *cliMatches) error {
	switch name {
	case "status":
		return policyStatus(m.flag("all"), m.optStr("repo"))
	case "plan":
		repo, _ := m.str("repo")
		entry, err := findRepo(repo)
		if err != nil {
			return err
		}
		report, err := policyReport(entry)
		if err != nil {
			return err
		}
		printPolicyReport(entry, report, true)
		return nil
	}
	repo, _ := m.str("repo")
	entry, err := findRepo(repo)
	if err != nil {
		return err
	}
	actions, err := applyPolicy(entry)
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		fmt.Printf("%s policy already applied\n", entry.ID)
	}
	for _, a := range actions {
		fmt.Printf("%s: %s\n", entry.ID, a.Description)
	}
	return nil
}

func policyStatus(all bool, repo *string) error {
	if all && repo != nil {
		return errf("use either --all or a repo name, not both")
	}
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	if all {
		for _, r := range policyAll(manifest) {
			if r.err != nil {
				fmt.Printf("%s | policy failed: %s\n", r.entry.ID, r.err)
				continue
			}
			printPolicyReport(r.entry, r.report, false)
		}
		return nil
	}
	e, err := findInManifest(manifest, repo)
	if err != nil {
		return err
	}
	report, err := policyReport(e)
	if err != nil {
		return err
	}
	printPolicyReport(e, report, true)
	return nil
}

func printPolicyReport(e RepoEntry, r RepoPolicyReport, verbose bool) {
	fmt.Printf("%s | visibility=%s | %s\n", e.ID, r.Policy.Visibility.label(), r.shortState())
	if !verbose {
		return
	}
	for _, f := range r.Findings {
		path := "-"
		if f.Path != nil {
			path = *f.Path
		}
		fmt.Printf("  [%s] %s: %s\n", f.Severity, path, f.Message)
	}
	if len(r.Actions) == 0 {
		fmt.Println("  no policy actions")
	}
	for _, a := range r.Actions {
		fmt.Printf("  action: %s\n", a.Description)
	}
}

func installIgnore(global bool) error {
	if !global {
		return errf("pass --global to install the managed global Git ignore block")
	}
	path, err := installGlobalIgnoreTemplate()
	if err != nil {
		return err
	}
	fmt.Printf("installed GitDCY global ignore block in %s\n", path)
	return nil
}

func syncCommand(all bool, repo *string) error {
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	if all {
		for _, e := range manifest.Repos {
			if e.Enabled {
				printSyncReport(syncRepo(e))
			}
		}
		return nil
	}
	e, err := findInManifest(manifest, repo)
	if err != nil {
		return err
	}
	printSyncReport(syncRepo(e))
	return nil
}

func printSyncReport(r SyncReport) {
	fmt.Println(r.RepoID)
	for _, a := range r.Actions {
		fmt.Printf("  - %s\n", a)
	}
	if r.Blocked != nil {
		fmt.Printf("  blocked: %s\n", *r.Blocked)
	}
}

func findRepo(name string) (RepoEntry, error) {
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return RepoEntry{}, err
	}
	for _, e := range manifest.Repos {
		if matchesRepo(e.ID, name) {
			return e, nil
		}
	}
	return RepoEntry{}, errf("repo not found: %s", name)
}

// ---------------------------------------------------------------------------
// status

func status(asJSON bool) error {
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	statuses := statusAll(manifest)
	if !asJSON {
		for _, s := range statuses {
			branch := "-"
			if s.Branch != nil {
				branch = *s.Branch
			}
			fmt.Printf("%-34s %-28s %s\n", s.Entry.ID, branch, s.shortState())
		}
		return nil
	}
	fmt.Println(jsonPretty(statusEnvelopeFrom(statuses)))
	return nil
}

type statusEnvelope struct {
	SchemaVersion string     `json:"schema_version"`
	Component     string     `json:"component"`
	Operation     string     `json:"operation"`
	Outcome       string     `json:"outcome"`
	Data          statusData `json:"data"`
	Warnings      []string   `json:"warnings"`
	Error         *string    `json:"error"`
	CorrelationID string     `json:"correlation_id"`
}

type statusData struct {
	Repos []statusRepo `json:"repos"`
}

type statusRepo struct {
	ID                 string  `json:"id"`
	Path               string  `json:"path"`
	Branch             *string `json:"branch"`
	TrackingBranch     *string `json:"tracking_branch"`
	Dirty              bool    `json:"dirty"`
	ChangedPaths       int     `json:"changed_paths"`
	Ahead              *uint32 `json:"ahead"`
	Behind             *uint32 `json:"behind"`
	IncomingWip        bool    `json:"incoming_wip"`
	IncomingWipTrusted bool    `json:"incoming_wip_trusted"`
	OutgoingWip        bool    `json:"outgoing_wip"`
	ReviewRequired     bool    `json:"review_required"`
	Safety             string  `json:"safety"`
	IdentityState      string  `json:"identity_state"`
	IdentityProfile    *string `json:"identity_profile"`
	IdentityEnforced   bool    `json:"identity_enforced"`
	IdentityBlocked    bool    `json:"identity_blocked"`
	LastError          *string `json:"last_error"`
	State              string  `json:"state"`
}

func statusEnvelopeFrom(statuses []RepoStatus) statusEnvelope {
	warnings := []string{}
	repos := []statusRepo{}
	for _, s := range statuses {
		if s.Entry.ReviewRequired {
			warnings = append(warnings, s.Entry.ID+" requires review")
		}
		if s.LastError != nil {
			warnings = append(warnings, s.Entry.ID+": "+*s.LastError)
		}
		if s.Identity.isBlocking() {
			warnings = append(warnings, s.Entry.ID+": "+s.Identity.stateLabel())
		}
		repos = append(repos, statusRepo{
			ID: s.Entry.ID, Path: s.Path, Branch: s.Branch, TrackingBranch: s.TrackingBranch,
			Dirty: s.isDirty(), ChangedPaths: len(s.DirtyPaths), Ahead: s.Ahead, Behind: s.Behind,
			IncomingWip: s.IncomingWip != nil, IncomingWipTrusted: s.IncomingWipTrusted, OutgoingWip: s.OutgoingWip != nil,
			ReviewRequired: s.Entry.ReviewRequired, Safety: s.Safety.shortState(), IdentityState: s.Identity.stateLabel(),
			IdentityProfile: s.Identity.Profile, IdentityEnforced: s.Identity.EnforcementEnabled,
			IdentityBlocked: s.Identity.isBlocking(), LastError: s.LastError, State: s.shortState(),
		})
	}
	outcome := "success"
	if len(warnings) > 0 {
		outcome = "partial"
	}
	return statusEnvelope{
		SchemaVersion: "gitdcy.response.v1", Component: "katteke.gitdcy", Operation: "status",
		Outcome: outcome, Data: statusData{Repos: repos}, Warnings: warnings, CorrelationID: correlationID(),
	}
}

var _ = os.Exit
