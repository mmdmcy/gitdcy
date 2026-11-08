package main

// GitDCY's settings and the reports about a repository.

import (
	"fmt"
	"strings"
)

// Provider is where a repository is hosted: its folder name under the
// workspace.
type Provider string

const (
	ProviderGithub  Provider = "github"
	ProviderForgejo Provider = "forgejo"
	ProviderGitlab  Provider = "gitlab"
	ProviderOther   Provider = "other"
)

func (Provider) enumVariants() []string { return []string{"github", "forgejo", "gitlab", "other"} }

func (p Provider) folder() string { return string(p) }

// providerFromPath is the first provider folder in a path.
func providerFromPath(path string) Provider {
	for _, part := range components(path) {
		switch asciiLower(part) {
		case "github":
			return ProviderGithub
		case "forgejo":
			return ProviderForgejo
		case "gitlab":
			return ProviderGitlab
		}
	}
	return ProviderOther
}

// remoteHost is a remote URL's host, lowercase, without user.
func remoteHost(url string) (string, bool) {
	value := asciiLower(trim(url))
	rest := value
	if _, r, ok := splitOnce(value, "://"); ok {
		rest = r
	}
	authority := rest
	if i := strings.IndexAny(rest, "/:"); i >= 0 {
		authority = rest[:i]
	}
	host := authority
	if _, h, ok := rsplitOnce(authority, "@"); ok {
		host = h
	}
	return host, host != ""
}

// providerFromURL is the provider a remote URL's host names.
func providerFromURL(url string) Provider {
	host, _ := remoteHost(url)
	switch {
	case host == "github.com" || strings.HasPrefix(host, "github-"):
		return ProviderGithub
	case host == "gitlab.com" || strings.HasPrefix(host, "gitlab-"):
		return ProviderGitlab
	case strings.Contains(host, "forgejo"):
		return ProviderForgejo
	}
	return ProviderOther
}

// VisibilityOverride is a repository marked public or private by hand.
type VisibilityOverride string

func (VisibilityOverride) enumVariants() []string { return []string{"public", "private"} }

// RepoVisibility is what GitDCY found a repository's remotes to be.
type RepoVisibility string

const (
	VisibilityPublic  RepoVisibility = "public"
	VisibilityPrivate RepoVisibility = "private"
	VisibilityUnknown RepoVisibility = "unknown"
)

func (v RepoVisibility) label() string { return string(v) }

// SafetyFinding is a path that must not be committed or pushed.
type SafetyFinding struct {
	Fatal       bool
	Path        *string
	Reason      string
	Remediation string
}

func (f SafetyFinding) severityLabel() string {
	if f.Fatal {
		return "fatal"
	}
	return "warning"
}

// RepoSafetyReport is a repository's audit.
type RepoSafetyReport struct {
	Visibility     RepoVisibility
	PublicTargeted bool
	Findings       []SafetyFinding
}

func (r RepoSafetyReport) fatalCount() int {
	n := 0
	for _, f := range r.Findings {
		if f.Fatal {
			n++
		}
	}
	return n
}

func (r RepoSafetyReport) hasFatalFindings() bool { return r.fatalCount() > 0 }

func (r RepoSafetyReport) shortState() string {
	if fatal := r.fatalCount(); fatal > 0 {
		return fmt.Sprintf("%d safety block%s", fatal, plural(fatal))
	}
	if r.PublicTargeted {
		return "public-safe"
	}
	return "private-target"
}

// PolicySeverity is how much a policy finding matters.
type PolicySeverity string

const (
	PolicyBlocker PolicySeverity = "blocker"
	PolicyDrift   PolicySeverity = "drift"
	PolicyInfo    PolicySeverity = "info"
)

type PolicyFinding struct {
	Severity PolicySeverity
	Path     *string
	Message  string
}

type PolicyAction struct{ Description string }

type RepoPolicy struct {
	Visibility     RepoVisibility
	PublicTargeted bool
	IgnoreRules    []string
}

type RepoPolicyReport struct {
	Policy   RepoPolicy
	Findings []PolicyFinding
	Actions  []PolicyAction
}

func (r RepoPolicyReport) count(s PolicySeverity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == s {
			n++
		}
	}
	return n
}

func (r RepoPolicyReport) shortState() string {
	if b := r.count(PolicyBlocker); b > 0 {
		return fmt.Sprintf("%d policy blocker%s", b, plural(b))
	}
	if d := r.count(PolicyDrift); d > 0 {
		return fmt.Sprintf("%d drift item%s", d, plural(d))
	}
	return "policy-ok"
}

// IdentityState is how a repository's git identity compares with its
// profile.
type IdentityState string

const (
	IdentityDisabled     IdentityState = "identity-unchecked"
	IdentityMatched      IdentityState = "identity-ok"
	IdentityUnconfigured IdentityState = "identity-not-configured"
	IdentityAmbiguous    IdentityState = "identity-ambiguous"
	IdentityMismatch     IdentityState = "identity-mismatch"
	IdentityInvalid      IdentityState = "identity-invalid"
	IdentityUnavailable  IdentityState = "identity-unavailable"
)

// GitIdentityProfile is one identity and the repositories it is for.
type GitIdentityProfile struct {
	Name           string     `json:"name"`
	Email          string     `json:"email"`
	PathPrefixes   []string   `json:"path_prefixes" toml:",path"`
	RemotePatterns []string   `json:"remote_patterns"`
	Providers      []Provider `json:"providers"`
}

// RepoIdentityReport is a repository's identity check.
type RepoIdentityReport struct {
	EnforcementEnabled   bool
	State                IdentityState
	Profile              *string
	Candidates           []string
	ExpectedName         *string
	ExpectedEmail        *string
	ActualName           *string
	ActualEmail          *string
	CommitterName        *string
	CommitterEmail       *string
	EnvironmentOverrides []string
	Message              string
}

func (r RepoIdentityReport) isBlocking() bool {
	return r.EnforcementEnabled && r.State != IdentityMatched
}

func (r RepoIdentityReport) stateLabel() string { return string(r.State) }

func (r RepoIdentityReport) shortState() string {
	if r.State == IdentityMatched {
		if r.Profile != nil {
			return "identity:" + *r.Profile
		}
		return string(IdentityMatched)
	}
	return string(r.State)
}

func (r RepoIdentityReport) expectedDisplay() string {
	return displayIdentity(r.ExpectedName, r.ExpectedEmail)
}
func (r RepoIdentityReport) actualDisplay() string {
	return displayIdentity(r.ActualName, r.ActualEmail)
}
func (r RepoIdentityReport) committerDisplay() string {
	return displayIdentity(r.CommitterName, r.CommitterEmail)
}

func unavailableIdentity(message string) RepoIdentityReport {
	return RepoIdentityReport{State: IdentityUnavailable, Message: message}
}

func displayIdentity(name, email *string) string {
	switch {
	case name != nil && email != nil:
		return *name + " <" + *email + ">"
	case name != nil:
		return *name + " <email unset>"
	case email != nil:
		return "name unset <" + *email + ">"
	}
	return "unset"
}

// CheckState is a repository's local checks, as a whole.
type CheckState string

const (
	CheckDisabled     CheckState = "checks-unchecked"
	CheckPassed       CheckState = "checks-passed"
	CheckFailed       CheckState = "checks-failed"
	CheckTimedOut     CheckState = "checks-timed-out"
	CheckUnconfigured CheckState = "checks-not-configured"
	CheckAmbiguous    CheckState = "checks-ambiguous"
	CheckInvalid      CheckState = "checks-invalid"
	CheckUnavailable  CheckState = "checks-unavailable"
)

// CheckResultState is one check's result.
type CheckResultState string

const (
	ResultPassed      CheckResultState = "passed"
	ResultFailed      CheckResultState = "failed"
	ResultTimedOut    CheckResultState = "timed out"
	ResultUnavailable CheckResultState = "unavailable"
)

// CheckCommand is one configured check: a program and its arguments.
type CheckCommand struct {
	Name           string   `json:"name"`
	Program        string   `json:"program"`
	Args           []string `json:"args"`
	TimeoutSeconds *uint64  `json:"timeout_seconds"`
}

func (c CheckCommand) displayCommand() string {
	parts := []string{displayCommandArg(c.Program)}
	for _, a := range c.Args {
		parts = append(parts, displayCommandArg(a))
	}
	return strings.Join(parts, " ")
}

func displayCommandArg(value string) string {
	for _, ch := range value {
		if !isASCIIAlnum(ch) && !strings.ContainsRune(".:/_=-", ch) {
			return rustDebug(value)
		}
	}
	return value
}

// CheckProfile is the checks for the repositories its selectors match.
type CheckProfile struct {
	PathPrefixes         []string       `json:"path_prefixes" toml:",path"`
	RemotePatterns       []string       `json:"remote_patterns"`
	Providers            []Provider     `json:"providers"`
	Checks               []CheckCommand `json:"checks"`
	RunBeforePush        bool           `json:"run_before_push" toml:",default=true"`
	RequireCleanWorktree bool           `json:"require_clean_worktree" toml:",default=true"`
}

func defaultCheckProfile() CheckProfile {
	return CheckProfile{RunBeforePush: true, RequireCleanWorktree: true}
}

type CheckResult struct {
	Name       string
	Command    string
	State      CheckResultState
	DurationMS int64
	Output     string
}

type RepoCheckReport struct {
	EnforcementEnabled bool
	State              CheckState
	Profile            *string
	Candidates         []string
	WorktreeClean      *bool
	Results            []CheckResult
	Message            string
}

func (r RepoCheckReport) isBlocking() bool {
	return r.EnforcementEnabled && r.State != CheckPassed
}

func (r RepoCheckReport) stateLabel() string { return string(r.State) }

// WorkspaceManifest is the workspace: its root and its repositories
// (workspace.toml).
type WorkspaceManifest struct {
	WorkspaceRoot string      `json:"workspace_root" toml:",required,path"`
	Repos         []RepoEntry `json:"repos" toml:",required"`
}

// RepoEntry is one repository of the workspace.
type RepoEntry struct {
	ID             string   `json:"id" toml:",required"`
	Path           string   `json:"path" toml:",required,path"`
	Provider       Provider `json:"provider" toml:",required"`
	Enabled        bool     `json:"enabled" toml:",required"`
	PrimaryRemote  *string  `json:"primary_remote"`
	WipSync        bool     `json:"wip_sync" toml:",required"`
	ReviewRequired bool     `json:"review_required" toml:",required"`
}

// LocalConfig is this device's settings (local.toml, merged with the
// current folder's .gitdcy.local.toml). A nil field is not set.
type LocalConfig struct {
	WorkspaceRoot         *string                        `json:"workspace_root" toml:",path"`
	ScanRoots             *[]string                      `json:"scan_roots" toml:",path"`
	SyncRemoteTemplate    *string                        `json:"sync_remote_template"`
	OriginRemoteTemplates *map[string]string             `json:"origin_remote_templates"`
	LocalSyncFiles        *map[string][]string           `json:"local_sync_files"`
	TrustedWipDevices     *map[string][]string           `json:"trusted_wip_devices"`
	VisibilityOverrides   *map[string]VisibilityOverride `json:"visibility_overrides"`
	PrivateRemotePatterns *[]string                      `json:"private_remote_patterns"`
	PublicExportRemotes   *[]string                      `json:"public_export_remotes"`
	IgnoreProfiles        *map[string][]string           `json:"ignore_profiles"`
	RequireIdentity       *bool                          `json:"require_identity"`
	IdentityProfiles      *map[string]GitIdentityProfile `json:"identity_profiles"`
	RequireChecks         *bool                          `json:"require_checks"`
	CheckProfiles         *map[string]CheckProfile       `json:"check_profiles"`
	// The repository rules file (`gitdcy rules`); read from this device's
	// own config only, never from a .gitdcy.local.toml in the current folder.
	RulesFile *string `json:"rules_file,omitempty" toml:",path"`
	// Set when a settings file could not be read: GitDCY then fails closed.
	ConfigError *string `json:"-"`
}

// ChangedPath is a path with local changes.
type ChangedPath struct {
	Path string
	Kind ChangeKind
}

type ChangeKind int

const (
	ChangeTracked ChangeKind = iota
	ChangeNew
	ChangeLocal
)

// WipRef is a work-in-progress snapshot ref.
type WipRef struct {
	Refname   string
	ShortName string
	Device    string
	Branch    string
	SHA       string
	Timestamp int64
}

// RepoStatus is a repository's state, as status and the dashboard show it.
type RepoStatus struct {
	Entry              RepoEntry
	Path               string
	Branch             *string
	TrackingBranch     *string
	Remotes            map[string]string
	DirtyPaths         []ChangedPath
	Ahead              *uint32
	Behind             *uint32
	IncomingWip        *WipRef
	IncomingWipTrusted bool
	OutgoingWip        *WipRef
	Safety             RepoSafetyReport
	Identity           RepoIdentityReport
	LastError          *string
}

func (s RepoStatus) isDirty() bool { return len(s.DirtyPaths) > 0 }

func (s RepoStatus) hasSyncRemote() bool {
	_, ok := s.Remotes[syncRemote]
	return ok
}

func (s RepoStatus) shortState() string {
	var parts []string
	if s.isDirty() {
		parts = append(parts, fmt.Sprintf("%d changed", len(s.DirtyPaths)))
	} else {
		parts = append(parts, "clean")
	}
	if s.Ahead != nil && *s.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d ahead", *s.Ahead))
	}
	if s.Behind != nil && *s.Behind > 0 {
		parts = append(parts, fmt.Sprintf("%d behind", *s.Behind))
	}
	if s.IncomingWip != nil {
		if s.IncomingWipTrusted {
			parts = append(parts, "incoming WIP")
		} else {
			parts = append(parts, "untrusted WIP from "+s.IncomingWip.Device)
		}
	}
	if s.LastError != nil {
		parts = append(parts, "blocked: "+*s.LastError)
	}
	if s.Safety.hasFatalFindings() {
		parts = append(parts, s.Safety.shortState())
	}
	if s.Identity.State != IdentityDisabled && s.Identity.State != IdentityMatched {
		parts = append(parts, s.Identity.shortState())
	}
	return strings.Join(parts, ", ")
}

// SyncReport is what sync did for a repository, or why it stopped.
type SyncReport struct {
	RepoID  string
	Actions []string
	Blocked *string
}

func (r *SyncReport) action(text string)  { r.Actions = append(r.Actions, text) }
func (r *SyncReport) block(reason string) { r.Blocked = &reason }

// CloneRequest is what `gitdcy clone` was asked.
type CloneRequest struct {
	URL           string
	WorkspaceRoot string
	Provider      *Provider
	Name          *string
}

// ptr is a pointer to v (an Option's Some).
func ptr[T any](v T) *T { return &v }
