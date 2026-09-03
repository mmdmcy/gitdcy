use anyhow::{anyhow, bail, Context, Result};
use directories::ProjectDirs;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use std::env;
use std::ffi::OsStr;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

mod process;

pub const APP_NAME: &str = "GitDCY";
pub const SYNC_REMOTE: &str = "sync";
const WIP_HEAD: &str = "refs/gitdcy/wip";
const WIP_REMOTE: &str = "refs/remotes/sync/wip";
const WIP_APPLIED: &str = "refs/gitdcy/applied";
const IGNORE_BLOCK_START: &str = "# BEGIN GITDCY PRIVATE DEFAULTS";
const IGNORE_BLOCK_END: &str = "# END GITDCY PRIVATE DEFAULTS";
const REPO_IGNORE_BLOCK_START: &str = "# BEGIN GITDCY REPO POLICY";
const REPO_IGNORE_BLOCK_END: &str = "# END GITDCY REPO POLICY";
const DISCOVERY_MAX_DIRECTORIES: usize = 100_000;
const DISCOVERY_MAX_REPOSITORIES: usize = 512;
const DISCOVERY_TIMEOUT: Duration = Duration::from_secs(10);
const LINUXMICE_GIT_OUTPUT_LIMIT: usize = 256 * 1024;
const LINUXMICE_GIT_TIMEOUT: Duration = Duration::from_secs(2);
const LINUXMICE_REPOSITORY_BATCH_TIMEOUT: Duration = Duration::from_secs(15);
const LINUXMICE_STATUS_WORKERS: usize = 8;
const LINUXMICE_STATUS_REPOSITORIES: usize = 128;
const CHECK_OUTPUT_LIMIT: usize = 256 * 1024;
const CHECK_OUTPUT_DISPLAY_LIMIT: usize = 16 * 1024;
const DEFAULT_CHECK_TIMEOUT_SECONDS: u64 = 15 * 60;
const MAX_CHECK_TIMEOUT_SECONDS: u64 = 60 * 60;
const DEFAULT_REPO_IGNORE_RULES: &[&str] = &[
    "AGENTS.md",
    ".env",
    ".env.*",
    "!.env.example",
    ".codex/",
    ".claude/",
    "private/",
    "docs/private/",
    "state/",
    "uploads/",
    "logs/",
    "*.log",
    "*.sqlite",
    "*.sqlite3",
    "*.db",
    "*.pem",
    "*.key",
    "id_rsa",
    "id_ed25519",
    "node_modules/",
    "target/",
    ".DS_Store",
];

fn remote_host(url: &str) -> Option<String> {
    let value = url.trim().to_ascii_lowercase();
    let rest = value
        .split_once("://")
        .map(|(_, rest)| rest)
        .unwrap_or(value.as_str());
    let authority = rest.split(['/', ':']).next()?;
    let host = authority
        .rsplit_once('@')
        .map(|(_, host)| host)
        .unwrap_or(authority);
    (!host.is_empty()).then(|| host.to_string())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Provider {
    Github,
    Forgejo,
    Gitlab,
    Other,
}

impl Provider {
    pub fn folder(self) -> &'static str {
        match self {
            Provider::Github => "github",
            Provider::Forgejo => "forgejo",
            Provider::Gitlab => "gitlab",
            Provider::Other => "other",
        }
    }

    pub fn from_path(path: &Path) -> Self {
        for part in path.components() {
            let text = part.as_os_str().to_string_lossy().to_ascii_lowercase();
            match text.as_str() {
                "github" => return Provider::Github,
                "forgejo" => return Provider::Forgejo,
                "gitlab" => return Provider::Gitlab,
                _ => {}
            }
        }
        Provider::Other
    }

    pub fn from_url(url: &str) -> Self {
        let host = remote_host(url).unwrap_or_default();
        if host == "github.com" || host.starts_with("github-") {
            Provider::Github
        } else if host == "gitlab.com" || host.starts_with("gitlab-") {
            Provider::Gitlab
        } else if host.contains("forgejo") {
            Provider::Forgejo
        } else {
            Provider::Other
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum VisibilityOverride {
    Public,
    Private,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RepoVisibility {
    Public,
    Private,
    Unknown,
}

impl RepoVisibility {
    pub fn label(self) -> &'static str {
        match self {
            Self::Public => "public",
            Self::Private => "private",
            Self::Unknown => "unknown",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SafetySeverity {
    Fatal,
    Warning,
}

impl SafetySeverity {
    pub fn label(self) -> &'static str {
        match self {
            Self::Fatal => "fatal",
            Self::Warning => "warning",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SafetyFinding {
    pub severity: SafetySeverity,
    pub path: Option<String>,
    pub reason: String,
    pub remediation: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoSafetyReport {
    pub visibility: RepoVisibility,
    pub public_targeted: bool,
    pub findings: Vec<SafetyFinding>,
}

impl RepoSafetyReport {
    pub fn ok(visibility: RepoVisibility, public_targeted: bool) -> Self {
        Self {
            visibility,
            public_targeted,
            findings: Vec::new(),
        }
    }

    pub fn fatal_count(&self) -> usize {
        self.findings
            .iter()
            .filter(|finding| finding.severity == SafetySeverity::Fatal)
            .count()
    }

    pub fn has_fatal_findings(&self) -> bool {
        self.fatal_count() > 0
    }

    pub fn short_state(&self) -> String {
        let fatal = self.fatal_count();
        if fatal > 0 {
            return format!("{fatal} safety block{}", if fatal == 1 { "" } else { "s" });
        }
        if self.public_targeted {
            "public-safe".to_string()
        } else {
            "private-target".to_string()
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PolicySeverity {
    Blocker,
    Drift,
    Info,
}

impl PolicySeverity {
    pub fn label(self) -> &'static str {
        match self {
            Self::Blocker => "blocker",
            Self::Drift => "drift",
            Self::Info => "info",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PolicyFinding {
    pub severity: PolicySeverity,
    pub path: Option<String>,
    pub message: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PolicyAction {
    pub description: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoPolicy {
    pub visibility: RepoVisibility,
    pub public_targeted: bool,
    pub ignore_rules: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoPolicyReport {
    pub policy: RepoPolicy,
    pub findings: Vec<PolicyFinding>,
    pub actions: Vec<PolicyAction>,
}

impl RepoPolicyReport {
    pub fn drift_count(&self) -> usize {
        self.findings
            .iter()
            .filter(|finding| finding.severity == PolicySeverity::Drift)
            .count()
    }

    pub fn blocker_count(&self) -> usize {
        self.findings
            .iter()
            .filter(|finding| finding.severity == PolicySeverity::Blocker)
            .count()
    }

    pub fn short_state(&self) -> String {
        let blockers = self.blocker_count();
        if blockers > 0 {
            return format!(
                "{blockers} policy blocker{}",
                if blockers == 1 { "" } else { "s" }
            );
        }
        let drift = self.drift_count();
        if drift > 0 {
            return format!("{drift} drift item{}", if drift == 1 { "" } else { "s" });
        }
        "policy-ok".to_string()
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IdentityState {
    Disabled,
    Matched,
    Unconfigured,
    Ambiguous,
    Mismatch,
    Invalid,
    Unavailable,
}

impl IdentityState {
    pub fn label(self) -> &'static str {
        match self {
            Self::Disabled => "identity-unchecked",
            Self::Matched => "identity-ok",
            Self::Unconfigured => "identity-not-configured",
            Self::Ambiguous => "identity-ambiguous",
            Self::Mismatch => "identity-mismatch",
            Self::Invalid => "identity-invalid",
            Self::Unavailable => "identity-unavailable",
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct GitIdentityProfile {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub email: String,
    #[serde(default)]
    pub path_prefixes: Vec<PathBuf>,
    #[serde(default)]
    pub remote_patterns: Vec<String>,
    #[serde(default)]
    pub providers: Vec<Provider>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoIdentityReport {
    pub enforcement_enabled: bool,
    pub state: IdentityState,
    pub profile: Option<String>,
    pub candidates: Vec<String>,
    pub expected_name: Option<String>,
    pub expected_email: Option<String>,
    pub actual_name: Option<String>,
    pub actual_email: Option<String>,
    pub committer_name: Option<String>,
    pub committer_email: Option<String>,
    pub environment_overrides: Vec<String>,
    pub message: String,
}

impl RepoIdentityReport {
    pub fn is_blocking(&self) -> bool {
        self.enforcement_enabled && self.state != IdentityState::Matched
    }

    pub fn state_label(&self) -> &'static str {
        self.state.label()
    }

    pub fn short_state(&self) -> String {
        match self.state {
            IdentityState::Matched => self
                .profile
                .as_deref()
                .map(|profile| format!("identity:{profile}"))
                .unwrap_or_else(|| IdentityState::Matched.label().to_string()),
            state => state.label().to_string(),
        }
    }

    pub fn expected_display(&self) -> String {
        display_identity(&self.expected_name, &self.expected_email)
    }

    pub fn actual_display(&self) -> String {
        display_identity(&self.actual_name, &self.actual_email)
    }

    pub fn committer_display(&self) -> String {
        display_identity(&self.committer_name, &self.committer_email)
    }

    fn unavailable(message: impl Into<String>) -> Self {
        Self {
            enforcement_enabled: false,
            state: IdentityState::Unavailable,
            profile: None,
            candidates: Vec::new(),
            expected_name: None,
            expected_email: None,
            actual_name: None,
            actual_email: None,
            committer_name: None,
            committer_email: None,
            environment_overrides: Vec::new(),
            message: message.into(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CheckState {
    Disabled,
    Passed,
    Failed,
    TimedOut,
    Unconfigured,
    Ambiguous,
    Invalid,
    Unavailable,
}

impl CheckState {
    pub fn label(self) -> &'static str {
        match self {
            Self::Disabled => "checks-unchecked",
            Self::Passed => "checks-passed",
            Self::Failed => "checks-failed",
            Self::TimedOut => "checks-timed-out",
            Self::Unconfigured => "checks-not-configured",
            Self::Ambiguous => "checks-ambiguous",
            Self::Invalid => "checks-invalid",
            Self::Unavailable => "checks-unavailable",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CheckResultState {
    Passed,
    Failed,
    TimedOut,
    Unavailable,
}

impl CheckResultState {
    pub fn label(self) -> &'static str {
        match self {
            Self::Passed => "passed",
            Self::Failed => "failed",
            Self::TimedOut => "timed out",
            Self::Unavailable => "unavailable",
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct CheckCommand {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub program: String,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub timeout_seconds: Option<u64>,
}

impl CheckCommand {
    pub fn display_command(&self) -> String {
        std::iter::once(self.program.as_str())
            .chain(self.args.iter().map(String::as_str))
            .map(display_command_arg)
            .collect::<Vec<_>>()
            .join(" ")
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct CheckProfile {
    #[serde(default)]
    pub path_prefixes: Vec<PathBuf>,
    #[serde(default)]
    pub remote_patterns: Vec<String>,
    #[serde(default)]
    pub providers: Vec<Provider>,
    #[serde(default)]
    pub checks: Vec<CheckCommand>,
    #[serde(default = "default_true")]
    pub run_before_push: bool,
    #[serde(default = "default_true")]
    pub require_clean_worktree: bool,
}

impl Default for CheckProfile {
    fn default() -> Self {
        Self {
            path_prefixes: Vec::new(),
            remote_patterns: Vec::new(),
            providers: Vec::new(),
            checks: Vec::new(),
            run_before_push: true,
            require_clean_worktree: true,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CheckResult {
    pub name: String,
    pub command: String,
    pub state: CheckResultState,
    pub duration_ms: u128,
    pub output: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoCheckReport {
    pub enforcement_enabled: bool,
    pub state: CheckState,
    pub profile: Option<String>,
    pub candidates: Vec<String>,
    pub worktree_clean: Option<bool>,
    pub results: Vec<CheckResult>,
    pub message: String,
}

impl RepoCheckReport {
    pub fn is_blocking(&self) -> bool {
        self.enforcement_enabled && self.state != CheckState::Passed
    }

    pub fn state_label(&self) -> &'static str {
        self.state.label()
    }

    pub fn short_state(&self) -> String {
        match self.state {
            CheckState::Passed => self
                .profile
                .as_deref()
                .map(|profile| format!("checks:{profile}"))
                .unwrap_or_else(|| CheckState::Passed.label().to_string()),
            state => state.label().to_string(),
        }
    }
}

fn default_true() -> bool {
    true
}

fn display_command_arg(value: &str) -> String {
    if value
        .chars()
        .all(|ch| ch.is_ascii_alphanumeric() || ".:/_=-".contains(ch))
    {
        value.to_string()
    } else {
        format!("{:?}", value)
    }
}

fn display_identity(name: &Option<String>, email: &Option<String>) -> String {
    match (name.as_deref(), email.as_deref()) {
        (Some(name), Some(email)) => format!("{name} <{email}>"),
        (Some(name), None) => format!("{name} <email unset>"),
        (None, Some(email)) => format!("name unset <{email}>"),
        (None, None) => "unset".to_string(),
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkspaceManifest {
    pub workspace_root: PathBuf,
    pub repos: Vec<RepoEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RepoEntry {
    pub id: String,
    pub path: PathBuf,
    pub provider: Provider,
    pub enabled: bool,
    pub primary_remote: Option<String>,
    pub wip_sync: bool,
    pub review_required: bool,
}

#[derive(Debug, Clone)]
pub struct RepoStatus {
    pub entry: RepoEntry,
    pub path: PathBuf,
    pub branch: Option<String>,
    pub tracking_branch: Option<String>,
    pub remotes: BTreeMap<String, String>,
    pub dirty_paths: Vec<ChangedPath>,
    pub ahead: Option<u32>,
    pub behind: Option<u32>,
    pub incoming_wip: Option<WipRef>,
    pub incoming_wip_trusted: bool,
    pub outgoing_wip: Option<WipRef>,
    pub safety: RepoSafetyReport,
    pub identity: RepoIdentityReport,
    pub last_error: Option<String>,
}

impl RepoStatus {
    pub fn is_dirty(&self) -> bool {
        !self.dirty_paths.is_empty()
    }

    pub fn has_sync_remote(&self) -> bool {
        self.remotes.contains_key(SYNC_REMOTE)
    }

    pub fn short_state(&self) -> String {
        let mut parts = Vec::new();
        if self.is_dirty() {
            parts.push(format!("{} changed", self.dirty_paths.len()));
        } else {
            parts.push("clean".to_string());
        }
        if let Some(ahead) = self.ahead.filter(|value| *value > 0) {
            parts.push(format!("{ahead} ahead"));
        }
        if let Some(behind) = self.behind.filter(|value| *value > 0) {
            parts.push(format!("{behind} behind"));
        }
        if let Some(wip) = &self.incoming_wip {
            if self.incoming_wip_trusted {
                parts.push("incoming WIP".to_string());
            } else {
                parts.push(format!("untrusted WIP from {}", wip.device));
            }
        }
        if let Some(error) = &self.last_error {
            parts.push(format!("blocked: {error}"));
        }
        if self.safety.has_fatal_findings() {
            parts.push(self.safety.short_state());
        }
        if !matches!(
            self.identity.state,
            IdentityState::Disabled | IdentityState::Matched
        ) {
            parts.push(self.identity.short_state());
        }
        parts.join(", ")
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ChangedPath {
    pub path: String,
    pub kind: ChangeKind,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChangeKind {
    Tracked,
    New,
    Local,
}

#[derive(Debug, Clone)]
pub struct WipRef {
    pub refname: String,
    pub short_name: String,
    pub device: String,
    pub branch: String,
    pub sha: String,
    pub timestamp: i64,
}

#[derive(Debug, Clone)]
pub struct SyncReport {
    pub repo_id: String,
    pub actions: Vec<String>,
    pub blocked: Option<String>,
}

impl SyncReport {
    fn new(repo_id: impl Into<String>) -> Self {
        Self {
            repo_id: repo_id.into(),
            actions: Vec::new(),
            blocked: None,
        }
    }

    fn action(&mut self, action: impl Into<String>) {
        self.actions.push(action.into());
    }

    fn block(&mut self, reason: impl Into<String>) {
        self.blocked = Some(reason.into());
    }
}

#[derive(Debug, Clone)]
pub struct CloneRequest {
    pub url: String,
    pub workspace_root: PathBuf,
    pub provider: Option<Provider>,
    pub name: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct LocalConfig {
    pub workspace_root: Option<PathBuf>,
    pub scan_roots: Option<Vec<PathBuf>>,
    pub sync_remote_template: Option<String>,
    pub origin_remote_templates: Option<BTreeMap<String, String>>,
    pub local_sync_files: Option<BTreeMap<String, Vec<String>>>,
    pub trusted_wip_devices: Option<BTreeMap<String, Vec<String>>>,
    pub visibility_overrides: Option<BTreeMap<String, VisibilityOverride>>,
    pub private_remote_patterns: Option<Vec<String>>,
    pub public_export_remotes: Option<Vec<String>>,
    pub ignore_profiles: Option<BTreeMap<String, Vec<String>>>,
    pub require_identity: Option<bool>,
    pub identity_profiles: Option<BTreeMap<String, GitIdentityProfile>>,
    pub require_checks: Option<bool>,
    pub check_profiles: Option<BTreeMap<String, CheckProfile>>,
    #[serde(skip)]
    pub config_error: Option<String>,
}

pub fn project_dirs() -> Result<ProjectDirs> {
    ProjectDirs::from("dev", "gitdcy", "GitDCY")
        .ok_or_else(|| anyhow!("could not determine config directory"))
}

pub fn config_dir() -> Result<PathBuf> {
    Ok(project_dirs()?.config_dir().to_path_buf())
}

pub fn local_config_path() -> Result<PathBuf> {
    Ok(config_dir()?.join("local.yaml"))
}

pub fn manifest_path() -> Result<PathBuf> {
    Ok(config_dir()?.join("workspace.yaml"))
}

pub fn default_workspace_root() -> PathBuf {
    if let Some(root) = load_local_config().workspace_root {
        return expand_home(root);
    }
    home_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("Code")
}

pub fn set_workspace_root(root: PathBuf) -> Result<PathBuf> {
    let root = expand_home(root);
    let mut config = load_saved_local_config();
    config.workspace_root = Some(root.clone());
    ensure_scan_root(&mut config, root);
    save_local_config(&config)
}

pub fn default_scan_roots() -> Vec<PathBuf> {
    load_local_config()
        .scan_roots
        .filter(|roots| !roots.is_empty())
        .map(|roots| roots.into_iter().map(expand_home).collect())
        .unwrap_or_else(default_candidate_scan_roots)
}

fn default_candidate_scan_roots() -> Vec<PathBuf> {
    let mut roots = if linuxmice_read_only_status() {
        Vec::new()
    } else {
        vec![default_workspace_root()]
    };
    if let Some(home) = home_dir() {
        for provider in ["github", "forgejo", "gitlab"] {
            let root = home.join("Documents").join(provider);
            if root.exists() && !roots.iter().any(|existing| existing == &root) {
                roots.push(root);
            }
        }
    }
    roots
}

pub fn add_scan_root(root: PathBuf) -> Result<PathBuf> {
    let root = expand_home(root);
    let mut config = load_saved_local_config();
    if config
        .scan_roots
        .as_ref()
        .is_none_or(|roots| roots.is_empty())
    {
        let workspace_root = workspace_root_with_config(&config);
        ensure_scan_root(&mut config, workspace_root);
    }
    ensure_scan_root(&mut config, root);
    save_local_config(&config)
}

pub fn sync_remote_template() -> Option<String> {
    load_local_config()
        .sync_remote_template
        .filter(|value| !value.trim().is_empty())
}

pub fn local_sync_file_enabled(entry: &RepoEntry, file: &str) -> bool {
    let Some(file) = safe_relative_local_sync_path(file) else {
        return false;
    };
    configured_local_sync_files(entry, &load_local_config())
        .iter()
        .any(|path| path == &file)
}

pub fn set_local_sync_file(entry: &RepoEntry, file: &str, enabled: bool) -> Result<PathBuf> {
    let file = safe_relative_local_sync_path(file)
        .with_context(|| format!("invalid local sync file path: {file}"))?;
    let mut config = load_saved_local_config();
    let files = config.local_sync_files.get_or_insert_with(BTreeMap::new);
    let repo_files = files.entry(entry.id.clone()).or_default();

    if enabled {
        if !repo_files.iter().any(|path| path == &file) {
            repo_files.push(file);
            repo_files.sort();
        }
    } else {
        repo_files.retain(|path| path != &file);
        if repo_files.is_empty() {
            files.remove(&entry.id);
        }
        if files.is_empty() {
            config.local_sync_files = None;
        }
    }

    save_local_config(&config)
}

pub fn wip_device_trusted(entry: &RepoEntry, device: &str) -> bool {
    wip_device_trusted_with_config(entry, device, &load_local_config())
}

pub fn set_wip_device_trusted(entry: &RepoEntry, device: &str, trusted: bool) -> Result<PathBuf> {
    set_wip_device_trusted_for_key(&entry.id, device, trusted)
}

pub fn set_wip_device_trusted_globally(device: &str, trusted: bool) -> Result<PathBuf> {
    set_wip_device_trusted_for_key("*", device, trusted)
}

fn set_wip_device_trusted_for_key(key: &str, device: &str, trusted: bool) -> Result<PathBuf> {
    let device =
        normalize_device_id(device).with_context(|| format!("invalid device: {device}"))?;
    let mut config = load_saved_local_config();
    let devices = config.trusted_wip_devices.get_or_insert_with(BTreeMap::new);
    let repo_devices = devices.entry(key.to_string()).or_default();

    if trusted {
        if !repo_devices.iter().any(|value| value == &device) {
            repo_devices.push(device);
            repo_devices.sort();
        }
    } else {
        repo_devices.retain(|value| value != &device);
        if repo_devices.is_empty() {
            devices.remove(key);
        }
        if devices.is_empty() {
            config.trusted_wip_devices = None;
        }
    }

    save_local_config(&config)
}

pub fn suggested_origin_remote(entry: &RepoEntry) -> Option<String> {
    let provider = entry.provider.folder();
    let templates = load_local_config().origin_remote_templates?;
    let template = templates
        .get(provider)
        .filter(|value| !value.trim().is_empty())?;
    Some(apply_remote_template(template, entry))
}

pub fn load_or_discover_manifest() -> Result<WorkspaceManifest> {
    let path = manifest_path()?;
    if path.exists() {
        let text = fs::read_to_string(&path)
            .with_context(|| format!("read manifest {}", path.display()))?;
        let manifest: WorkspaceManifest = serde_norway::from_str(&text)
            .with_context(|| format!("parse manifest {}", path.display()))?;
        return Ok(manifest);
    }

    let scan_roots = if linuxmice_read_only_status() {
        default_candidate_scan_roots()
    } else {
        default_scan_roots()
    };
    Ok(WorkspaceManifest {
        workspace_root: default_workspace_root(),
        repos: discover_entries(&scan_roots)?,
    })
}

pub fn save_manifest(manifest: &WorkspaceManifest) -> Result<()> {
    let path = manifest_path()?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("create config directory {}", parent.display()))?;
    }
    let text = serde_norway::to_string(manifest)?;
    fs::write(&path, text).with_context(|| format!("write manifest {}", path.display()))?;
    Ok(())
}

pub fn discover_entries(roots: &[PathBuf]) -> Result<Vec<RepoEntry>> {
    let mut repos = Vec::new();
    let mut seen = BTreeSet::new();
    let mut budget = DiscoveryBudget::new();

    for root in roots {
        for repo in discover_repo_paths_with_budget(root, &mut budget)? {
            budget.check_time()?;
            let canonical = repo.canonicalize().unwrap_or_else(|_| repo.clone());
            if !seen.insert(canonical) {
                continue;
            }

            let remotes = remotes(&repo).unwrap_or_default();
            budget.check_time()?;
            let origin = remotes.get("origin").cloned();
            let provider = origin
                .as_deref()
                .map(Provider::from_url)
                .filter(|provider| *provider != Provider::Other)
                .unwrap_or_else(|| Provider::from_path(&repo));
            let id = repo_id(&repo, provider);
            let mut entry = RepoEntry {
                id,
                path: repo,
                provider,
                enabled: true,
                primary_remote: origin,
                wip_sync: true,
                review_required: false,
            };

            entry.review_required =
                entry.primary_remote.is_none() && suggested_origin_remote(&entry).is_none();
            budget.check_time()?;

            repos.push(entry);
        }
    }

    repos.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(repos)
}

pub fn discover_repo_paths(root: &Path) -> Result<Vec<PathBuf>> {
    discover_repo_paths_with_budget(root, &mut DiscoveryBudget::new())
}

struct DiscoveryBudget {
    directories: usize,
    repositories: usize,
    deadline: Instant,
    max_depth: usize,
    max_repositories: usize,
}

impl DiscoveryBudget {
    fn new() -> Self {
        Self::for_mode(linuxmice_read_only_status())
    }

    fn for_mode(linuxmice_read_only: bool) -> Self {
        Self {
            directories: 0,
            repositories: 0,
            deadline: Instant::now() + DISCOVERY_TIMEOUT,
            max_depth: if linuxmice_read_only { 1 } else { usize::MAX },
            max_repositories: if linuxmice_read_only {
                LINUXMICE_STATUS_REPOSITORIES
            } else {
                DISCOVERY_MAX_REPOSITORIES
            },
        }
    }

    fn check_time(&self) -> Result<()> {
        if Instant::now() >= self.deadline {
            bail!("repository discovery exceeded its time safety limit");
        }
        Ok(())
    }

    fn enter_directory(&mut self) -> Result<()> {
        self.check_time()?;
        self.directories = self
            .directories
            .checked_add(1)
            .ok_or_else(|| anyhow!("repository discovery directory count overflow"))?;
        if self.directories > DISCOVERY_MAX_DIRECTORIES {
            bail!("repository discovery exceeded its directory safety limit");
        }
        Ok(())
    }

    fn record_repository(&mut self) -> Result<()> {
        self.repositories = self
            .repositories
            .checked_add(1)
            .ok_or_else(|| anyhow!("repository discovery count overflow"))?;
        if self.repositories > self.max_repositories {
            bail!("repository discovery exceeded its repository safety limit");
        }
        Ok(())
    }
}

fn discover_repo_paths_with_budget(
    root: &Path,
    budget: &mut DiscoveryBudget,
) -> Result<Vec<PathBuf>> {
    let mut found = Vec::new();
    if !root.exists() {
        return Ok(found);
    }

    fn visit(
        dir: &Path,
        depth: usize,
        found: &mut Vec<PathBuf>,
        budget: &mut DiscoveryBudget,
    ) -> Result<()> {
        budget.enter_directory()?;
        let metadata = match fs::symlink_metadata(dir) {
            Ok(metadata) => metadata,
            Err(_) => return Ok(()),
        };
        if metadata.file_type().is_symlink() || !metadata.is_dir() {
            return Ok(());
        }
        if dir.join(".git").exists() {
            budget.record_repository()?;
            found.push(dir.to_path_buf());
            return Ok(());
        }
        if depth >= budget.max_depth {
            return Ok(());
        }

        let entries = match fs::read_dir(dir) {
            Ok(entries) => entries,
            Err(_) => return Ok(()),
        };

        for entry in entries {
            budget.check_time()?;
            let entry = match entry {
                Ok(entry) => entry,
                Err(_) => continue,
            };
            let path = entry.path();
            let file_type = match entry.file_type() {
                Ok(file_type) => file_type,
                Err(_) => continue,
            };
            if !file_type.is_dir() || should_skip_dir(&path) {
                continue;
            }
            visit(&path, depth + 1, found, budget)?;
        }
        Ok(())
    }

    visit(root, 0, &mut found, budget)?;
    Ok(found)
}

fn should_skip_dir(path: &Path) -> bool {
    matches!(
        path.file_name().and_then(OsStr::to_str),
        Some(".git")
            | Some("node_modules")
            | Some("target")
            | Some("dist")
            | Some("build")
            | Some(".venv")
            | Some("vendor")
            | Some("tmp")
            | Some("log")
            | Some(".cache")
            | Some(".next")
            | Some(".turbo")
    )
}

pub fn repo_status(entry: &RepoEntry) -> RepoStatus {
    match repo_status_result(entry) {
        Ok(status) => status,
        Err(error) => failed_repo_status(entry, error.to_string()),
    }
}

fn failed_repo_status(entry: &RepoEntry, error: String) -> RepoStatus {
    RepoStatus {
        entry: entry.clone(),
        path: entry.path.clone(),
        branch: None,
        tracking_branch: None,
        remotes: BTreeMap::new(),
        dirty_paths: Vec::new(),
        ahead: None,
        behind: None,
        incoming_wip: None,
        incoming_wip_trusted: true,
        outgoing_wip: None,
        safety: RepoSafetyReport::ok(RepoVisibility::Unknown, true),
        identity: RepoIdentityReport::unavailable(error.clone()),
        last_error: Some(error),
    }
}

pub fn repo_status_result(entry: &RepoEntry) -> Result<RepoStatus> {
    let config = load_local_config();
    let branch = current_branch(&entry.path)?;
    let tracking_branch = tracking_branch(&entry.path).ok();
    let remotes = remotes(&entry.path)?;
    let identity = identity_report_with_config(entry, &remotes, &config);
    let dirty_paths = sync_paths_with_config(entry, &config)?;
    let (ahead, behind) = if let Some(tracking_branch) = &tracking_branch {
        ahead_behind(&entry.path, tracking_branch).unwrap_or((None, None))
    } else {
        (None, None)
    };
    let incoming_wip = latest_incoming_wip(&entry.path, branch.as_deref().unwrap_or("HEAD"))
        .ok()
        .flatten();
    let incoming_wip_trusted = incoming_wip
        .as_ref()
        .map(|wip| wip_device_trusted_with_config(entry, &wip.device, &config))
        .unwrap_or(true);
    let outgoing_wip = local_wip(&entry.path, branch.as_deref().unwrap_or("HEAD"))
        .ok()
        .flatten();
    let safety = audit_repo_with_config(entry, &config)?;

    Ok(RepoStatus {
        entry: entry.clone(),
        path: entry.path.clone(),
        branch,
        tracking_branch,
        remotes,
        dirty_paths,
        ahead,
        behind,
        incoming_wip,
        incoming_wip_trusted,
        outgoing_wip,
        safety,
        identity,
        last_error: None,
    })
}

pub fn status_all(manifest: &WorkspaceManifest) -> Vec<RepoStatus> {
    status_all_for_mode(manifest, linuxmice_read_only_status())
}

fn status_all_for_mode(manifest: &WorkspaceManifest, linuxmice_read_only: bool) -> Vec<RepoStatus> {
    let entries = manifest
        .repos
        .iter()
        .filter(|repo| repo.enabled)
        .cloned()
        .collect::<Vec<_>>();
    if !linuxmice_read_only {
        return entries.iter().map(repo_status).collect();
    }

    let mut statuses = std::iter::repeat_with(|| None)
        .take(entries.len())
        .collect::<Vec<Option<RepoStatus>>>();
    let processable = entries.len().min(LINUXMICE_STATUS_REPOSITORIES);
    for (batch_number, batch) in entries[..processable]
        .chunks(LINUXMICE_STATUS_WORKERS)
        .enumerate()
    {
        let first_index = batch_number * LINUXMICE_STATUS_WORKERS;
        let (sender, receiver) = std::sync::mpsc::channel();
        for (offset, entry) in batch.iter().cloned().enumerate() {
            let sender = sender.clone();
            std::thread::spawn(move || {
                let _ = sender.send((first_index + offset, repo_status(&entry)));
            });
        }
        drop(sender);

        let deadline = Instant::now() + LINUXMICE_REPOSITORY_BATCH_TIMEOUT;
        let mut batch_timed_out = false;
        for _ in 0..batch.len() {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                batch_timed_out = true;
                break;
            }
            match receiver.recv_timeout(remaining) {
                Ok((index, status)) => statuses[index] = Some(status),
                Err(_) => {
                    batch_timed_out = true;
                    break;
                }
            }
        }
        for (offset, entry) in batch.iter().enumerate() {
            let index = first_index + offset;
            if statuses[index].is_none() {
                statuses[index] = Some(failed_repo_status(
                    entry,
                    "repository status exceeded its LinuxMice time safety limit".to_string(),
                ));
            }
        }
        if batch_timed_out {
            for (index, entry) in entries.iter().enumerate().skip(first_index + batch.len()) {
                statuses[index] = Some(failed_repo_status(
                    entry,
                    "repository status was not started after a prior batch exceeded its LinuxMice time safety limit"
                        .to_string(),
                ));
            }
            break;
        }
    }
    for (index, entry) in entries.iter().enumerate().skip(processable) {
        if statuses[index].is_none() {
            statuses[index] = Some(failed_repo_status(
                entry,
                "repository status was not started because the LinuxMice repository safety limit was reached"
                    .to_string(),
            ));
        }
    }
    statuses.into_iter().map(Option::unwrap).collect()
}

pub fn sync_repo(entry: &RepoEntry) -> SyncReport {
    let mut report = SyncReport::new(entry.id.clone());
    if let Err(error) = sync_repo_inner(entry, &mut report) {
        report.block(error.to_string());
    }
    report
}

fn sync_repo_inner(entry: &RepoEntry, report: &mut SyncReport) -> Result<()> {
    let config = load_local_config();
    sync_repo_inner_with_config(entry, report, &config)
}

fn sync_repo_inner_with_config(
    entry: &RepoEntry,
    report: &mut SyncReport,
    config: &LocalConfig,
) -> Result<()> {
    let remotes = remotes(&entry.path)?;
    let identity = enforce_identity(entry, &remotes, config)?;
    let branch = current_branch(&entry.path)?.unwrap_or_else(|| "HEAD".to_string());
    let before_dirty = sync_paths_with_config(entry, config)?;

    let wip_remote = wip_remote_name(&remotes);

    if entry.wip_sync && wip_remote.is_some() && !before_dirty.is_empty() {
        let sha = create_wip_snapshot(&entry.path, &branch, &before_dirty, identity.as_ref())?;
        push_wip_snapshot(&entry.path, wip_remote.as_deref().unwrap(), &branch, &sha)?;
        report.action(format!("pushed WIP snapshot {}", short_sha(&sha)));
    } else if !before_dirty.is_empty() {
        report.action("dirty; skipped WIP snapshot because no private WIP remote is configured");
    }

    for remote in remotes.keys() {
        git(&entry.path, ["fetch", "--prune", "--tags", remote])?;
        report.action(format!("fetched {remote}"));
    }
    if let Some(wip_remote) = &wip_remote {
        fetch_wip_refs(&entry.path, wip_remote)?;
        report.action(format!("fetched WIP refs from {wip_remote}"));
    }

    let dirty_after_fetch = sync_paths_with_config(entry, config)?;
    if dirty_after_fetch.is_empty() {
        if let Ok(tracking_branch) = tracking_branch(&entry.path) {
            let (_, behind) = ahead_behind(&entry.path, &tracking_branch)?;
            if behind.unwrap_or(0) > 0 {
                git(&entry.path, ["pull", "--ff-only"])?;
                report.action("fast-forward pulled tracking branch");
            }
        }
    } else {
        report.action("skipped branch pull because working tree is dirty");
    }

    if entry.wip_sync && wip_remote.is_some() {
        let applied = apply_latest_incoming_wip(entry, &branch, config)?;
        if let Some(wip) = applied {
            report.action(format!("applied incoming WIP from {}", wip.device));
            let combined_dirty = sync_paths_with_config(entry, config)?;
            if !combined_dirty.is_empty() {
                let sha =
                    create_wip_snapshot(&entry.path, &branch, &combined_dirty, identity.as_ref())?;
                push_wip_snapshot(&entry.path, wip_remote.as_deref().unwrap(), &branch, &sha)?;
                report.action(format!("pushed combined WIP {}", short_sha(&sha)));
            }
        }
    }

    Ok(())
}

pub fn clone_repo(request: &CloneRequest) -> Result<PathBuf> {
    let provider = request
        .provider
        .unwrap_or_else(|| Provider::from_url(&request.url));
    let name = request
        .name
        .clone()
        .unwrap_or_else(|| repo_name_from_url(&request.url));
    let destination = request
        .workspace_root
        .join(provider.folder())
        .join(sanitize_component(&name));

    let config = load_local_config();
    let mut candidate_remotes = BTreeMap::new();
    candidate_remotes.insert("origin".to_string(), request.url.clone());
    let candidate = RepoEntry {
        id: repo_id(&destination, provider),
        path: destination.clone(),
        provider,
        enabled: true,
        primary_remote: Some(request.url.clone()),
        wip_sync: true,
        review_required: false,
    };
    let _identity = enforce_identity(&candidate, &candidate_remotes, &config)?;

    if destination.exists() {
        bail!("destination already exists: {}", destination.display());
    }
    if let Some(parent) = destination.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("create destination parent {}", parent.display()))?;
    }

    let status = Command::new("git")
        .args(["clone", &request.url])
        .arg(&destination)
        .status()
        .context("run git clone")?;
    if !status.success() {
        bail!("git clone failed with status {status}");
    }
    Ok(destination)
}

pub fn commit(repo: &Path, message: &str, paths: &[String]) -> Result<()> {
    if message.trim().is_empty() {
        bail!("commit message is required");
    }
    let entry = entry_for_repo(repo)?;
    let config = load_local_config();
    let remotes = remotes(repo)?;
    let identity = enforce_identity(&entry, &remotes, &config)?;
    if paths.is_empty() {
        git(repo, ["add", "-A"])?;
    } else {
        git_paths(repo, ["add", "-A"], paths)?;
    }
    let report = audit_staged_with_config(&entry, &config)?;
    if report.has_fatal_findings() {
        let _ = git(repo, ["reset", "-q"]);
        bail!("{}", format_audit_block(&entry, &report));
    }
    if let Some(identity) = identity {
        git_env(
            repo,
            ["commit", "-m", message],
            [
                ("GIT_AUTHOR_NAME", identity.name.as_str()),
                ("GIT_AUTHOR_EMAIL", identity.email.as_str()),
                ("GIT_COMMITTER_NAME", identity.name.as_str()),
                ("GIT_COMMITTER_EMAIL", identity.email.as_str()),
            ],
        )?;
    } else {
        git(repo, ["commit", "-m", message])?;
    }
    Ok(())
}

pub fn push(repo: &Path) -> Result<()> {
    let entry = entry_for_repo(repo)?;
    let config = load_local_config();
    let remotes = remotes(repo)?;
    let _identity = enforce_identity(&entry, &remotes, &config)?;
    let report = audit_repo_with_config(&entry, &config)?;
    if report.has_fatal_findings() {
        bail!("{}", format_audit_block(&entry, &report));
    }
    let checks = check_repo_with_config(&entry, &remotes, &config, CheckTrigger::BeforePush);
    if checks.is_blocking() {
        bail!("{}", format_check_block(&entry, &checks));
    }
    git(repo, ["push"])?;
    Ok(())
}

fn entry_for_repo(repo: &Path) -> Result<RepoEntry> {
    let remotes = remotes(repo)?;
    let origin = remotes.get("origin").cloned();
    let provider = origin
        .as_deref()
        .map(Provider::from_url)
        .filter(|provider| *provider != Provider::Other)
        .unwrap_or_else(|| Provider::from_path(repo));
    let id = repo_id(repo, provider);
    Ok(RepoEntry {
        id,
        path: repo.to_path_buf(),
        provider,
        enabled: true,
        primary_remote: origin,
        wip_sync: true,
        review_required: false,
    })
}

pub fn format_audit_block(entry: &RepoEntry, report: &RepoSafetyReport) -> String {
    let mut lines = vec![format!(
        "GitDCY safety audit blocked {} (visibility: {})",
        entry.id,
        report.visibility.label()
    )];
    for finding in report
        .findings
        .iter()
        .filter(|finding| finding.severity == SafetySeverity::Fatal)
    {
        let path = finding.path.as_deref().unwrap_or("-");
        lines.push(format!(
            "- [{}] {}: {} ({})",
            finding.severity.label(),
            path,
            finding.reason,
            finding.remediation
        ));
    }
    lines.join("\n")
}

pub fn install_global_ignore_template() -> Result<PathBuf> {
    let path = global_excludes_file_path()?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("create git ignore directory {}", parent.display()))?;
    }
    let existing = fs::read_to_string(&path).unwrap_or_default();
    let merged = merge_managed_ignore_block(&existing);
    fs::write(&path, merged).with_context(|| format!("write {}", path.display()))?;

    if git_config_global_excludes_file()?.is_none() {
        let value = path.to_string_lossy().to_string();
        let output = Command::new("git")
            .args(["config", "--global", "core.excludesfile", &value])
            .output()
            .context("set git global core.excludesfile")?;
        if !output.status.success() {
            bail!("{}", command_error("git config", &output));
        }
    }

    Ok(path)
}

fn global_excludes_file_path() -> Result<PathBuf> {
    if let Some(path) = git_config_global_excludes_file()? {
        return Ok(expand_home(PathBuf::from(path)));
    }
    Ok(config_dir()?.join("git-ignore"))
}

fn git_config_global_excludes_file() -> Result<Option<String>> {
    let output = Command::new("git")
        .args(["config", "--global", "--get", "core.excludesfile"])
        .output()
        .context("read git global core.excludesfile")?;
    if output.status.success() {
        let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
        return Ok((!value.is_empty()).then_some(value));
    }
    Ok(None)
}

fn merge_managed_ignore_block(existing: &str) -> String {
    let rules: Vec<String> = DEFAULT_REPO_IGNORE_RULES
        .iter()
        .map(|rule| rule.to_string())
        .collect();
    merge_managed_block(existing, IGNORE_BLOCK_START, IGNORE_BLOCK_END, &rules)
}

fn merge_managed_block(existing: &str, start: &str, end: &str, rules: &[String]) -> String {
    let mut output = String::new();
    let mut in_managed_block = false;
    for line in existing.lines() {
        if line.trim() == start {
            in_managed_block = true;
            continue;
        }
        if line.trim() == end {
            in_managed_block = false;
            continue;
        }
        if !in_managed_block {
            output.push_str(line);
            output.push('\n');
        }
    }
    while output.ends_with("\n\n") {
        output.pop();
    }
    if !output.is_empty() && !output.ends_with('\n') {
        output.push('\n');
    }
    if !output.is_empty() {
        output.push('\n');
    }
    output.push_str(start);
    output.push('\n');
    for rule in rules {
        output.push_str(rule);
        output.push('\n');
    }
    output.push_str(end);
    output.push('\n');
    output
}

pub fn set_remote(repo: &Path, name: &str, url: &str) -> Result<()> {
    if name.trim().is_empty() || url.trim().is_empty() {
        bail!("remote name and URL are required");
    }
    let current_remotes = remotes(repo)?;
    if name != SYNC_REMOTE {
        let config = load_local_config();
        let provider = if name == "origin" {
            let from_url = Provider::from_url(url);
            if from_url == Provider::Other {
                Provider::from_path(repo)
            } else {
                from_url
            }
        } else {
            Provider::from_path(repo)
        };
        let mut candidate_remotes = current_remotes.clone();
        candidate_remotes.insert(name.to_string(), url.to_string());
        let candidate = RepoEntry {
            id: repo_id(repo, provider),
            path: repo.to_path_buf(),
            provider,
            enabled: true,
            primary_remote: candidate_remotes.get("origin").cloned(),
            wip_sync: true,
            review_required: false,
        };
        let _identity = enforce_identity(&candidate, &candidate_remotes, &config)?;
    }
    if current_remotes.contains_key(name) {
        git(repo, ["remote", "set-url", name, url])?;
    } else {
        git(repo, ["remote", "add", name, url])?;
    }
    Ok(())
}

pub fn set_suggested_origin_remote(entry: &RepoEntry) -> Result<String> {
    let url = suggested_origin_remote(entry).with_context(|| {
        format!(
            "no origin remote template configured for {}",
            entry.provider.folder()
        )
    })?;
    set_remote(&entry.path, "origin", &url)?;
    Ok(url)
}

pub fn current_branch(repo: &Path) -> Result<Option<String>> {
    let output = git_output(repo, ["branch", "--show-current"])?;
    let branch = output.trim();
    Ok((!branch.is_empty()).then(|| branch.to_string()))
}

pub fn tracking_branch(repo: &Path) -> Result<String> {
    Ok(git_output(
        repo,
        ["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"],
    )?
    .trim()
    .to_string())
}

pub fn remotes(repo: &Path) -> Result<BTreeMap<String, String>> {
    let output = git_output(repo, ["remote", "-v"])?;
    let mut remotes = BTreeMap::new();
    for line in output.lines() {
        let mut fields = line.split_whitespace();
        let Some(name) = fields.next() else { continue };
        let Some(url) = fields.next() else { continue };
        remotes
            .entry(name.to_string())
            .or_insert_with(|| url.to_string());
    }
    Ok(remotes)
}

pub fn identity_report(entry: &RepoEntry) -> RepoIdentityReport {
    let config = load_local_config();
    let remotes = remotes(&entry.path).unwrap_or_default();
    identity_report_with_config(entry, &remotes, &config)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum CheckTrigger {
    Manual,
    BeforePush,
}

pub fn check_repo(entry: &RepoEntry) -> RepoCheckReport {
    let config = load_local_config();
    let remotes = match remotes(&entry.path) {
        Ok(remotes) => remotes,
        Err(error) => {
            return RepoCheckReport {
                enforcement_enabled: false,
                state: CheckState::Unavailable,
                profile: None,
                candidates: Vec::new(),
                worktree_clean: None,
                results: Vec::new(),
                message: format!("could not read repository remotes: {error}"),
            }
        }
    };
    check_repo_with_config(entry, &remotes, &config, CheckTrigger::Manual)
}

fn check_repo_with_config(
    entry: &RepoEntry,
    remotes: &BTreeMap<String, String>,
    config: &LocalConfig,
    trigger: CheckTrigger,
) -> RepoCheckReport {
    let require_checks = config.require_checks.unwrap_or(false);
    let profiles = config.check_profiles.as_ref();
    let has_profiles = profiles.is_some_and(|profiles| !profiles.is_empty());

    if !has_profiles {
        return RepoCheckReport {
            enforcement_enabled: require_checks,
            state: if require_checks {
                CheckState::Unconfigured
            } else {
                CheckState::Disabled
            },
            profile: None,
            candidates: Vec::new(),
            worktree_clean: None,
            results: Vec::new(),
            message: if require_checks {
                "check enforcement is enabled but no check profiles are configured".to_string()
            } else {
                "no local check profile is configured for this repository".to_string()
            },
        };
    }

    let profiles = profiles.expect("check profile presence checked above");
    let mut invalid = profiles
        .iter()
        .filter_map(|(id, profile)| {
            invalid_check_profile_reason(id, profile).map(|reason| format!("{id}: {reason}"))
        })
        .collect::<Vec<_>>();
    if let Some(error) = &config.config_error {
        invalid.insert(0, error.clone());
    }
    if !invalid.is_empty() {
        return RepoCheckReport {
            enforcement_enabled: true,
            state: CheckState::Invalid,
            profile: None,
            candidates: Vec::new(),
            worktree_clean: None,
            results: Vec::new(),
            message: format!(
                "invalid check profile configuration: {}",
                invalid.join("; ")
            ),
        };
    }

    let matching = profiles
        .iter()
        .filter(|(_, profile)| {
            profile_selectors_match(
                &profile.path_prefixes,
                &profile.remote_patterns,
                &profile.providers,
                &entry.path,
                entry.provider,
                remotes,
            )
        })
        .map(|(id, _)| id.clone())
        .collect::<Vec<_>>();
    let candidates = matching
        .iter()
        .filter(|id| {
            trigger == CheckTrigger::Manual
                || profiles
                    .get(*id)
                    .is_some_and(|profile| profile.run_before_push)
        })
        .cloned()
        .collect::<Vec<_>>();
    let enforcement_enabled = require_checks || !candidates.is_empty();

    if candidates.len() != 1 {
        let state = if candidates.is_empty() {
            if enforcement_enabled {
                CheckState::Unconfigured
            } else {
                CheckState::Disabled
            }
        } else {
            CheckState::Ambiguous
        };
        let message = if candidates.is_empty() {
            if trigger == CheckTrigger::BeforePush && !matching.is_empty() {
                "matching check profiles are disabled before push".to_string()
            } else {
                "no local check profile matches this repository path, provider, and remotes"
                    .to_string()
            }
        } else {
            format!(
                "multiple local check profiles match this repository: {}",
                candidates.join(", ")
            )
        };
        return RepoCheckReport {
            enforcement_enabled,
            state,
            profile: None,
            candidates,
            worktree_clean: None,
            results: Vec::new(),
            message,
        };
    }

    let profile_id = candidates[0].clone();
    let profile = profiles
        .get(&profile_id)
        .expect("check candidate must exist");
    let worktree_clean = worktree_is_clean(&entry.path).ok();
    if trigger == CheckTrigger::BeforePush && profile.require_clean_worktree {
        match worktree_clean {
            Some(true) => {}
            Some(false) => {
                return RepoCheckReport {
                    enforcement_enabled: true,
                    state: CheckState::Failed,
                    profile: Some(profile_id),
                    candidates,
                    worktree_clean,
                    results: Vec::new(),
                    message: "working tree is not clean; checks would not exactly match the commit being pushed".to_string(),
                }
            }
            None => {
                return RepoCheckReport {
                    enforcement_enabled: true,
                    state: CheckState::Unavailable,
                    profile: Some(profile_id),
                    candidates,
                    worktree_clean,
                    results: Vec::new(),
                    message: "could not determine whether the working tree is clean".to_string(),
                }
            }
        }
    }

    let mut results = Vec::new();
    for check in &profile.checks {
        let result = run_check_command(&entry.path, check);
        let result_state = result.state;
        let result_name = result.name.clone();
        results.push(result);
        if result_state != CheckResultState::Passed {
            let state = match result_state {
                CheckResultState::TimedOut => CheckState::TimedOut,
                CheckResultState::Unavailable => CheckState::Unavailable,
                CheckResultState::Failed => CheckState::Failed,
                CheckResultState::Passed => CheckState::Passed,
            };
            return RepoCheckReport {
                enforcement_enabled: true,
                state,
                profile: Some(profile_id.clone()),
                candidates,
                worktree_clean,
                results,
                message: format!("check {result_name} failed in profile {profile_id}"),
            };
        }
    }

    RepoCheckReport {
        enforcement_enabled: true,
        state: CheckState::Passed,
        profile: Some(profile_id.clone()),
        candidates,
        worktree_clean,
        results,
        message: format!(
            "all {} configured checks passed in profile {profile_id}",
            profile.checks.len()
        ),
    }
}

fn invalid_check_profile_reason(id: &str, profile: &CheckProfile) -> Option<String> {
    if id.trim().is_empty() {
        return Some("profile id is empty".to_string());
    }
    if profile.path_prefixes.is_empty()
        && profile.remote_patterns.is_empty()
        && profile.providers.is_empty()
    {
        return Some(
            "at least one path_prefixes, remote_patterns, or providers selector is required"
                .to_string(),
        );
    }
    if profile
        .path_prefixes
        .iter()
        .map(|path| expand_home(path.clone()))
        .any(|path| !path.is_absolute())
    {
        return Some("path_prefixes must be absolute paths or use ~/".to_string());
    }
    if profile
        .remote_patterns
        .iter()
        .any(|pattern| pattern.trim().is_empty())
    {
        return Some("remote_patterns cannot contain empty values".to_string());
    }
    if profile.checks.is_empty() {
        return Some("at least one check command is required".to_string());
    }
    for check in &profile.checks {
        if check.name.trim().is_empty() {
            return Some("check names cannot be empty".to_string());
        }
        if check.program.trim().is_empty() {
            return Some(format!("check {} has no program", check.name));
        }
        if check.program.contains('\0') || check.args.iter().any(|arg| arg.contains('\0')) {
            return Some(format!("check {} contains a NUL character", check.name));
        }
        if check
            .timeout_seconds
            .is_some_and(|seconds| seconds == 0 || seconds > MAX_CHECK_TIMEOUT_SECONDS)
        {
            return Some(format!(
                "check {} timeout_seconds must be between 1 and {MAX_CHECK_TIMEOUT_SECONDS}",
                check.name
            ));
        }
    }
    None
}

fn profile_selectors_match(
    path_prefixes: &[PathBuf],
    remote_patterns: &[String],
    providers: &[Provider],
    repo: &Path,
    provider: Provider,
    remotes: &BTreeMap<String, String>,
) -> bool {
    let path_matches = path_prefixes.is_empty()
        || path_prefixes.iter().any(|prefix| {
            let prefix = expand_home(prefix.clone());
            let canonical_repo = repo.canonicalize().unwrap_or_else(|_| repo.to_path_buf());
            let canonical_prefix = prefix.canonicalize().unwrap_or(prefix);
            canonical_repo.starts_with(canonical_prefix)
        });
    let remote_matches = remote_patterns.is_empty()
        || remote_patterns.iter().any(|pattern| {
            let primary = remotes
                .get("origin")
                .map(|url| ("origin", url))
                .or_else(|| remotes.get(SYNC_REMOTE).map(|url| (SYNC_REMOTE, url)));
            primary.is_some_and(|(name, url)| remote_selector_matches(pattern, name, url))
        });
    let provider_matches = providers.is_empty() || providers.contains(&provider);
    path_matches && remote_matches && provider_matches
}

fn worktree_is_clean(repo: &Path) -> Result<bool> {
    Ok(
        git_output(repo, ["status", "--porcelain=v1", "--untracked-files=all"])?
            .trim()
            .is_empty(),
    )
}

fn run_check_command(repo: &Path, check: &CheckCommand) -> CheckResult {
    let timeout = Duration::from_secs(
        check
            .timeout_seconds
            .unwrap_or(DEFAULT_CHECK_TIMEOUT_SECONDS),
    );
    let mut command = Command::new(&check.program);
    command
        .args(&check.args)
        .current_dir(repo)
        .env("CI", "true")
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("CARGO_TERM_COLOR", "never");
    let started = Instant::now();
    let command_display = check.display_command();
    match process::run_bounded_command(command, CHECK_OUTPUT_LIMIT, timeout) {
        Ok(output) => {
            let duration_ms = started.elapsed().as_millis();
            let timed_out = !output.status.success()
                && output.status.code().is_none()
                && started.elapsed() >= timeout;
            let state = if output.status.success() {
                CheckResultState::Passed
            } else if timed_out {
                CheckResultState::TimedOut
            } else {
                CheckResultState::Failed
            };
            let mut output_text = combined_check_output(&output);
            if output_text.trim().is_empty() && !output.status.success() {
                output_text = format!("process exited with {}", output.status);
            }
            CheckResult {
                name: check.name.clone(),
                command: command_display,
                state,
                duration_ms,
                output: truncate_check_output(output_text),
            }
        }
        Err(error) => CheckResult {
            name: check.name.clone(),
            command: command_display,
            state: CheckResultState::Unavailable,
            duration_ms: started.elapsed().as_millis(),
            output: truncate_check_output(error.to_string()),
        },
    }
}

fn combined_check_output(output: &std::process::Output) -> String {
    let stdout = String::from_utf8_lossy(&output.stdout);
    let stderr = String::from_utf8_lossy(&output.stderr);
    match (stdout.trim(), stderr.trim()) {
        ("", "") => String::new(),
        (stdout, "") => stdout.to_string(),
        ("", stderr) => stderr.to_string(),
        (stdout, stderr) => format!("{stdout}\n{stderr}"),
    }
}

fn truncate_check_output(output: String) -> String {
    if output.len() <= CHECK_OUTPUT_DISPLAY_LIMIT {
        return output;
    }
    let mut end = CHECK_OUTPUT_DISPLAY_LIMIT;
    while !output.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}\n[check output truncated]", &output[..end])
}

pub fn format_check_block(entry: &RepoEntry, report: &RepoCheckReport) -> String {
    let mut lines = vec![format!(
        "GitDCY checks blocked {} ({})",
        entry.id,
        report.state_label()
    )];
    for result in &report.results {
        if result.state == CheckResultState::Passed {
            continue;
        }
        lines.push(format!(
            "- [{}] {} ({})",
            result.state.label(),
            result.name,
            result.command
        ));
        if !result.output.trim().is_empty() {
            for line in result.output.lines() {
                lines.push(format!("  {line}"));
            }
        }
    }
    lines.push(format!("- {}", report.message));
    lines.join("\n")
}

#[derive(Debug, Clone, Default)]
struct EffectiveGitIdentity {
    author_name: Option<String>,
    author_email: Option<String>,
    committer_name: Option<String>,
    committer_email: Option<String>,
    environment_overrides: Vec<String>,
}

fn identity_report_with_config(
    entry: &RepoEntry,
    remotes: &BTreeMap<String, String>,
    config: &LocalConfig,
) -> RepoIdentityReport {
    let enforcement_enabled = identity_enforcement_enabled(config);
    let actual = effective_git_identity(&entry.path);
    let profiles = config.identity_profiles.as_ref();
    let has_profiles = profiles.is_some_and(|profiles| !profiles.is_empty());

    if !has_profiles {
        return RepoIdentityReport {
            enforcement_enabled,
            state: if enforcement_enabled {
                IdentityState::Unconfigured
            } else {
                IdentityState::Disabled
            },
            profile: None,
            candidates: Vec::new(),
            expected_name: None,
            expected_email: None,
            actual_name: actual.author_name,
            actual_email: actual.author_email,
            committer_name: actual.committer_name,
            committer_email: actual.committer_email,
            environment_overrides: actual.environment_overrides,
            message: if enforcement_enabled {
                "identity enforcement is enabled but no identity profiles are configured"
                    .to_string()
            } else {
                "identity enforcement is disabled; no identity profiles are configured".to_string()
            },
        };
    }

    let profiles = profiles.expect("identity profile presence checked above");
    let mut invalid = profiles
        .iter()
        .filter_map(|(id, profile)| {
            invalid_identity_profile_reason(id, profile).map(|reason| format!("{id}: {reason}"))
        })
        .collect::<Vec<_>>();
    if let Some(error) = &config.config_error {
        invalid.insert(0, error.clone());
    }
    if !invalid.is_empty() {
        return RepoIdentityReport {
            enforcement_enabled,
            state: IdentityState::Invalid,
            profile: None,
            candidates: Vec::new(),
            expected_name: None,
            expected_email: None,
            actual_name: actual.author_name,
            actual_email: actual.author_email,
            committer_name: actual.committer_name,
            committer_email: actual.committer_email,
            environment_overrides: actual.environment_overrides,
            message: format!(
                "invalid identity profile configuration: {}",
                invalid.join("; ")
            ),
        };
    }

    let candidates = profiles
        .iter()
        .filter(|(_, profile)| {
            identity_profile_matches(profile, &entry.path, entry.provider, remotes)
        })
        .map(|(id, _)| id.clone())
        .collect::<Vec<_>>();

    if candidates.len() != 1 {
        let state = if candidates.is_empty() {
            IdentityState::Unconfigured
        } else {
            IdentityState::Ambiguous
        };
        let message = if candidates.is_empty() {
            "no identity profile matches this repository path, provider, and remotes".to_string()
        } else {
            format!(
                "multiple identity profiles match this repository: {}",
                candidates.join(", ")
            )
        };
        return RepoIdentityReport {
            enforcement_enabled,
            state,
            profile: None,
            candidates,
            expected_name: None,
            expected_email: None,
            actual_name: actual.author_name,
            actual_email: actual.author_email,
            committer_name: actual.committer_name,
            committer_email: actual.committer_email,
            environment_overrides: actual.environment_overrides,
            message,
        };
    }

    let profile_id = candidates[0].clone();
    let profile = profiles
        .get(&profile_id)
        .expect("identity candidate must exist");
    let expected_name = profile.name.trim().to_string();
    let expected_email = profile.email.trim().to_string();
    let mut differences = Vec::new();
    if actual.author_name.as_deref() != Some(expected_name.as_str())
        || actual.author_email.as_deref() != Some(expected_email.as_str())
    {
        differences.push(format!(
            "author is {}",
            display_identity(&actual.author_name, &actual.author_email)
        ));
    }
    if actual.committer_name.as_deref() != Some(expected_name.as_str())
        || actual.committer_email.as_deref() != Some(expected_email.as_str())
    {
        differences.push(format!(
            "committer is {}",
            display_identity(&actual.committer_name, &actual.committer_email)
        ));
    }
    let state = if differences.is_empty() {
        IdentityState::Matched
    } else {
        IdentityState::Mismatch
    };
    let message = if differences.is_empty() {
        format!("Git author and committer match identity profile {profile_id}")
    } else {
        format!(
            "Git identity does not match profile {profile_id} (expected {expected_name} <{expected_email}>; {})",
            differences.join("; ")
        )
    };

    RepoIdentityReport {
        enforcement_enabled,
        state,
        profile: Some(profile_id),
        candidates,
        expected_name: Some(expected_name),
        expected_email: Some(expected_email),
        actual_name: actual.author_name,
        actual_email: actual.author_email,
        committer_name: actual.committer_name,
        committer_email: actual.committer_email,
        environment_overrides: actual.environment_overrides,
        message,
    }
}

fn identity_enforcement_enabled(config: &LocalConfig) -> bool {
    config.require_identity.unwrap_or_else(|| {
        config
            .identity_profiles
            .as_ref()
            .is_some_and(|profiles| !profiles.is_empty())
    })
}

fn invalid_identity_profile_reason(id: &str, profile: &GitIdentityProfile) -> Option<String> {
    if id.trim().is_empty() {
        return Some("profile id is empty".to_string());
    }
    if profile.name.trim().is_empty() {
        return Some("name is empty".to_string());
    }
    if profile.email.trim().is_empty() {
        return Some("email is empty".to_string());
    }
    if profile.path_prefixes.is_empty()
        && profile.remote_patterns.is_empty()
        && profile.providers.is_empty()
    {
        return Some(
            "at least one path_prefixes, remote_patterns, or providers selector is required"
                .to_string(),
        );
    }
    if profile
        .path_prefixes
        .iter()
        .map(|path| expand_home(path.clone()))
        .any(|path| !path.is_absolute())
    {
        return Some("path_prefixes must be absolute paths or use ~/".to_string());
    }
    if profile
        .remote_patterns
        .iter()
        .any(|pattern| pattern.trim().is_empty())
    {
        return Some("remote_patterns cannot contain empty values".to_string());
    }
    None
}

fn identity_profile_matches(
    profile: &GitIdentityProfile,
    repo: &Path,
    provider: Provider,
    remotes: &BTreeMap<String, String>,
) -> bool {
    profile_selectors_match(
        &profile.path_prefixes,
        &profile.remote_patterns,
        &profile.providers,
        repo,
        provider,
        remotes,
    )
}

fn remote_selector_matches(pattern: &str, name: &str, url: &str) -> bool {
    let pattern = pattern.trim().to_ascii_lowercase();
    if pattern.is_empty() {
        return false;
    }
    if name.to_ascii_lowercase() == pattern {
        return true;
    }
    let pattern = normalize_remote_selector(&pattern);
    let url = normalize_remote_selector(url);
    url == pattern || url.starts_with(&(pattern + "/"))
}

fn normalize_remote_selector(value: &str) -> String {
    let value = value.trim().to_ascii_lowercase();
    let mut rest = value
        .split_once("://")
        .map(|(_, rest)| rest.to_string())
        .unwrap_or(value);

    if let Some(separator) = rest
        .find(['/', ':'])
        .and_then(|index| rest[..index].rfind('@').map(|_| index))
    {
        let authority = &rest[..separator];
        let without_user = authority
            .rsplit_once('@')
            .map(|(_, without_user)| without_user)
            .unwrap_or(authority);
        rest = format!("{without_user}{}", &rest[separator..]);
    } else if let Some((_, without_user)) = rest.rsplit_once('@') {
        rest = without_user.to_string();
    }

    let normalized = if let Some((host, path)) = rest.split_once(':') {
        format!("{host}/{path}")
    } else if let Some((host, path)) = rest.split_once('/') {
        format!("{host}/{path}")
    } else {
        rest.to_string()
    };
    let normalized = normalized.trim_matches('/');
    normalized
        .strip_suffix(".git")
        .unwrap_or(normalized)
        .to_string()
}

fn effective_git_identity(repo: &Path) -> EffectiveGitIdentity {
    let configured_name = git_config_value(repo, "user.name");
    let configured_email = git_config_value(repo, "user.email");
    let author_name_override = env::var_os("GIT_AUTHOR_NAME");
    let author_email_override = env::var_os("GIT_AUTHOR_EMAIL");
    let committer_name_override = env::var_os("GIT_COMMITTER_NAME");
    let committer_email_override = env::var_os("GIT_COMMITTER_EMAIL");

    let mut environment_overrides = Vec::new();
    for (name, value) in [
        ("GIT_AUTHOR_NAME", &author_name_override),
        ("GIT_AUTHOR_EMAIL", &author_email_override),
        ("GIT_COMMITTER_NAME", &committer_name_override),
        ("GIT_COMMITTER_EMAIL", &committer_email_override),
    ] {
        if value.is_some() {
            environment_overrides.push(name.to_string());
        }
    }

    EffectiveGitIdentity {
        author_name: author_name_override
            .map(|value| value.to_string_lossy().into_owned())
            .or_else(|| configured_name.clone()),
        author_email: author_email_override
            .map(|value| value.to_string_lossy().into_owned())
            .or_else(|| configured_email.clone()),
        committer_name: committer_name_override
            .map(|value| value.to_string_lossy().into_owned())
            .or(configured_name),
        committer_email: committer_email_override
            .map(|value| value.to_string_lossy().into_owned())
            .or(configured_email),
        environment_overrides,
    }
}

fn git_config_value(repo: &Path, key: &str) -> Option<String> {
    let output = if repo.is_dir() {
        git_command_output(git_command(repo, ["config", "--get", key])).ok()?
    } else {
        let mut command = Command::new("git");
        command.args(["config", "--get", key]);
        command.output().ok()?
    };
    if !output.status.success() {
        return None;
    }
    let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
    (!value.is_empty()).then_some(value)
}

fn enforce_identity(
    entry: &RepoEntry,
    remotes: &BTreeMap<String, String>,
    config: &LocalConfig,
) -> Result<Option<GitIdentityProfile>> {
    let report = identity_report_with_config(entry, remotes, config);
    if report.is_blocking() {
        bail!("identity check blocked {}: {}", entry.id, report.message);
    }
    if !report.enforcement_enabled || report.state != IdentityState::Matched {
        return Ok(None);
    }
    let Some(profile_id) = report.profile else {
        bail!("identity check matched without a profile for {}", entry.id);
    };
    let profile = config
        .identity_profiles
        .as_ref()
        .and_then(|profiles| profiles.get(&profile_id))
        .cloned()
        .with_context(|| {
            format!(
                "identity profile disappeared while checking {entry_id}",
                entry_id = entry.id
            )
        })?;
    Ok(Some(profile))
}

pub fn dirty_paths(repo: &Path) -> Result<Vec<ChangedPath>> {
    let output = git_bytes(
        repo,
        ["status", "--porcelain=v2", "-z", "--untracked-files=all"],
    )?;
    Ok(parse_porcelain_v2_z(&output))
}

pub fn sync_paths(entry: &RepoEntry) -> Result<Vec<ChangedPath>> {
    sync_paths_with_config(entry, &load_local_config())
}

pub fn audit_repo(entry: &RepoEntry) -> Result<RepoSafetyReport> {
    audit_repo_with_config(entry, &load_local_config())
}

pub fn audit_all(manifest: &WorkspaceManifest) -> Vec<(RepoEntry, Result<RepoSafetyReport>)> {
    manifest
        .repos
        .iter()
        .filter(|repo| repo.enabled)
        .map(|entry| (entry.clone(), audit_repo(entry)))
        .collect()
}

pub fn policy_report(entry: &RepoEntry) -> Result<RepoPolicyReport> {
    policy_report_with_config(entry, &load_local_config())
}

pub fn policy_all(manifest: &WorkspaceManifest) -> Vec<(RepoEntry, Result<RepoPolicyReport>)> {
    manifest
        .repos
        .iter()
        .filter(|repo| repo.enabled)
        .map(|entry| (entry.clone(), policy_report(entry)))
        .collect()
}

pub fn apply_policy(entry: &RepoEntry) -> Result<Vec<PolicyAction>> {
    let config = load_local_config();
    let report = policy_report_with_config(entry, &config)?;
    let mut actions = Vec::new();
    if report
        .findings
        .iter()
        .any(|finding| finding.message.contains("missing repo ignore rule"))
    {
        write_repo_ignore_block(&entry.path, &report.policy.ignore_rules)?;
        actions.push(PolicyAction {
            description: "updated .gitignore with GitDCY repo policy block".to_string(),
        });
    }
    Ok(actions)
}

fn policy_report_with_config(entry: &RepoEntry, config: &LocalConfig) -> Result<RepoPolicyReport> {
    let remotes = remotes(&entry.path)?;
    let visibility = classify_repo_visibility(entry, &remotes, config);
    let public_targeted = visibility != RepoVisibility::Private;
    let ignore_rules = repo_ignore_rules(entry, config);
    let mut findings = Vec::new();
    let mut actions = Vec::new();

    let safety = audit_repo_with_config(entry, config)?;
    for finding in safety.findings {
        findings.push(PolicyFinding {
            severity: PolicySeverity::Blocker,
            path: finding.path,
            message: finding.reason,
        });
    }

    let identity = identity_report_with_config(entry, &remotes, config);
    if identity.is_blocking() {
        findings.push(PolicyFinding {
            severity: PolicySeverity::Blocker,
            path: None,
            message: format!("identity policy: {}", identity.message),
        });
    }

    let existing_ignore = repo_ignore_lines(&entry.path)?;
    for rule in &ignore_rules {
        if !existing_ignore.contains(rule) {
            findings.push(PolicyFinding {
                severity: PolicySeverity::Drift,
                path: Some(".gitignore".to_string()),
                message: format!("missing repo ignore rule `{rule}`"),
            });
        }
    }

    if !remotes.contains_key(SYNC_REMOTE) && entry.provider != Provider::Forgejo {
        findings.push(PolicyFinding {
            severity: PolicySeverity::Info,
            path: None,
            message: "no private sync remote configured for WIP refs".to_string(),
        });
    }

    if findings
        .iter()
        .any(|finding| finding.message.contains("missing repo ignore rule"))
    {
        actions.push(PolicyAction {
            description: "write or refresh GitDCY repo policy block in .gitignore".to_string(),
        });
    }

    Ok(RepoPolicyReport {
        policy: RepoPolicy {
            visibility,
            public_targeted,
            ignore_rules,
        },
        findings,
        actions,
    })
}

fn repo_ignore_rules(entry: &RepoEntry, config: &LocalConfig) -> Vec<String> {
    let mut rules: Vec<String> = DEFAULT_REPO_IGNORE_RULES
        .iter()
        .map(|rule| rule.to_string())
        .collect();
    if let Some(profiles) = &config.ignore_profiles {
        let repo_name = entry
            .path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("");
        for key in ["*", entry.provider.folder(), entry.id.as_str(), repo_name] {
            if let Some(values) = profiles.get(key) {
                rules.extend(values.iter().cloned());
            }
        }
    }
    rules.sort();
    rules.dedup();
    rules
}

fn repo_ignore_lines(repo: &Path) -> Result<BTreeSet<String>> {
    let path = repo.join(".gitignore");
    let text = fs::read_to_string(&path).unwrap_or_default();
    Ok(text
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty() && !line.starts_with('#'))
        .map(ToOwned::to_owned)
        .collect())
}

fn write_repo_ignore_block(repo: &Path, rules: &[String]) -> Result<()> {
    let path = repo.join(".gitignore");
    let existing = fs::read_to_string(&path).unwrap_or_default();
    let merged = merge_managed_block(
        &existing,
        REPO_IGNORE_BLOCK_START,
        REPO_IGNORE_BLOCK_END,
        rules,
    );
    fs::write(&path, merged).with_context(|| format!("write {}", path.display()))?;
    Ok(())
}

fn audit_repo_with_config(entry: &RepoEntry, config: &LocalConfig) -> Result<RepoSafetyReport> {
    let remotes = remotes(&entry.path)?;
    let visibility = classify_repo_visibility(entry, &remotes, config);
    let public_targeted = visibility != RepoVisibility::Private;
    let mut report = RepoSafetyReport::ok(visibility, public_targeted);

    for path in tracked_paths(&entry.path)? {
        if let Some(finding) = current_tree_finding(&path, public_targeted) {
            report.findings.push(finding);
        }
    }

    Ok(report)
}

fn audit_staged_with_config(entry: &RepoEntry, config: &LocalConfig) -> Result<RepoSafetyReport> {
    let remotes = remotes(&entry.path)?;
    let visibility = classify_repo_visibility(entry, &remotes, config);
    let public_targeted = visibility != RepoVisibility::Private;
    let mut report = RepoSafetyReport::ok(visibility, public_targeted);

    for change in staged_paths(&entry.path)? {
        if change.deleted {
            continue;
        }
        if let Some(finding) = staged_path_finding(&change.path, public_targeted) {
            report.findings.push(finding);
        }
    }

    Ok(report)
}

fn classify_repo_visibility(
    entry: &RepoEntry,
    remotes: &BTreeMap<String, String>,
    config: &LocalConfig,
) -> RepoVisibility {
    if let Some(override_) = visibility_override(entry, config) {
        return match override_ {
            VisibilityOverride::Public => RepoVisibility::Public,
            VisibilityOverride::Private => RepoVisibility::Private,
        };
    }

    if remotes
        .keys()
        .any(|name| public_export_remote_name(name, config))
    {
        return RepoVisibility::Public;
    }

    if remotes.is_empty() {
        return RepoVisibility::Private;
    }

    let mut saw_unknown = false;
    for url in remotes.values() {
        if private_remote_url(url, config) {
            continue;
        }
        match remote_url_visibility(url) {
            Some(RepoVisibility::Public) => return RepoVisibility::Public,
            Some(RepoVisibility::Private) => {}
            _ => saw_unknown = true,
        }
    }

    if saw_unknown {
        RepoVisibility::Unknown
    } else {
        RepoVisibility::Private
    }
}

fn public_export_remote_name(name: &str, config: &LocalConfig) -> bool {
    if name == "public" {
        return true;
    }
    config
        .public_export_remotes
        .as_ref()
        .is_some_and(|remotes| remotes.iter().any(|remote| remote == name))
}

fn private_remote_url(url: &str, config: &LocalConfig) -> bool {
    let lower = url.to_ascii_lowercase();
    if Provider::from_url(url) == Provider::Forgejo {
        return true;
    }
    private_remote_patterns(config)
        .iter()
        .any(|pattern| lower.contains(&pattern.to_ascii_lowercase()))
}

fn private_remote_patterns(config: &LocalConfig) -> Vec<String> {
    let mut patterns = vec!["forgejo".to_string(), "forgejo-easy".to_string()];
    if let Some(configured) = &config.private_remote_patterns {
        patterns.extend(configured.iter().cloned());
    }
    patterns.sort();
    patterns.dedup();
    patterns
}

fn visibility_override(entry: &RepoEntry, config: &LocalConfig) -> Option<VisibilityOverride> {
    let overrides = config.visibility_overrides.as_ref()?;
    let repo_name = entry
        .path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("");
    ["*", entry.id.as_str(), repo_name]
        .into_iter()
        .find_map(|key| overrides.get(key).copied())
}

fn remote_url_visibility(url: &str) -> Option<RepoVisibility> {
    if linuxmice_read_only_status() {
        return None;
    }
    if let Some(slug) = repo_slug_for_host(url, "github.com") {
        return github_repo_visibility(&slug);
    }
    if let Some(slug) = repo_slug_for_host(url, "gitlab.com") {
        return gitlab_repo_visibility(&slug);
    }
    None
}

fn github_repo_visibility(slug: &str) -> Option<RepoVisibility> {
    let output = Command::new("gh")
        .args([
            "repo",
            "view",
            slug,
            "--json",
            "visibility",
            "--jq",
            ".visibility",
        ])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    visibility_from_text(&String::from_utf8_lossy(&output.stdout))
}

fn gitlab_repo_visibility(slug: &str) -> Option<RepoVisibility> {
    let output = Command::new("glab")
        .args(["repo", "view", slug, "--output", "json"])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    visibility_from_text(&String::from_utf8_lossy(&output.stdout))
}

fn visibility_from_text(text: &str) -> Option<RepoVisibility> {
    let lower = text.to_ascii_lowercase();
    if lower.contains("public") {
        Some(RepoVisibility::Public)
    } else if lower.contains("private") {
        Some(RepoVisibility::Private)
    } else {
        None
    }
}

fn repo_slug_for_host(url: &str, host: &str) -> Option<String> {
    let trimmed = url.trim().trim_end_matches(".git").trim_end_matches('/');
    let host_marker = format!("{host}/");
    if let Some(rest) = trimmed.split_once(&host_marker).map(|(_, rest)| rest) {
        return normalize_remote_slug(rest);
    }
    let scp_marker = format!("{host}:");
    if let Some(rest) = trimmed.split_once(&scp_marker).map(|(_, rest)| rest) {
        return normalize_remote_slug(rest);
    }
    None
}

fn normalize_remote_slug(value: &str) -> Option<String> {
    let slug = value.trim_matches('/').trim_end_matches(".git");
    let mut parts = slug.split('/').filter(|part| !part.is_empty());
    let owner = parts.next()?;
    let repo = parts.next()?;
    Some(format!("{owner}/{repo}"))
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct StagedPath {
    path: String,
    deleted: bool,
}

fn staged_paths(repo: &Path) -> Result<Vec<StagedPath>> {
    let output = git_bytes(repo, ["diff", "--cached", "--name-status", "-z"])?;
    let parts: Vec<String> = output
        .split(|byte| *byte == 0)
        .filter(|part| !part.is_empty())
        .map(|part| String::from_utf8_lossy(part).to_string())
        .collect();
    let mut staged = Vec::new();
    let mut index = 0;
    while index < parts.len() {
        let status = &parts[index];
        index += 1;
        if status.starts_with('R') || status.starts_with('C') {
            if index + 1 >= parts.len() {
                break;
            }
            let _old_path = &parts[index];
            let new_path = parts[index + 1].clone();
            index += 2;
            staged.push(StagedPath {
                path: new_path,
                deleted: false,
            });
            continue;
        }
        if index >= parts.len() {
            break;
        }
        let path = parts[index].clone();
        index += 1;
        staged.push(StagedPath {
            path,
            deleted: status.starts_with('D'),
        });
    }
    Ok(staged)
}

fn tracked_paths(repo: &Path) -> Result<Vec<String>> {
    let output = git_bytes(repo, ["ls-files", "-z"])?;
    Ok(output
        .split(|byte| *byte == 0)
        .filter(|part| !part.is_empty())
        .map(|part| String::from_utf8_lossy(part).to_string())
        .collect())
}

fn current_tree_finding(path: &str, public_targeted: bool) -> Option<SafetyFinding> {
    if generated_cache_path(path) {
        return Some(finding(
            path,
            "generated dependency/build cache is tracked",
            "remove it from Git and add the cache path to .gitignore",
        ));
    }
    public_only_path_finding(path, public_targeted)
}

fn staged_path_finding(path: &str, public_targeted: bool) -> Option<SafetyFinding> {
    if generated_cache_path(path) {
        return Some(finding(
            path,
            "generated dependency/build cache is staged",
            "unstage it and add the cache path to .gitignore",
        ));
    }
    public_only_path_finding(path, public_targeted)
}

fn public_only_path_finding(path: &str, public_targeted: bool) -> Option<SafetyFinding> {
    if !public_targeted {
        return None;
    }

    if agent_notes_path(path) {
        return Some(finding(
            path,
            "agent operating notes are private-by-default for public repos",
            "remove the file from the public tree or mark the repo private in GitDCY local config",
        ));
    }
    if private_env_path(path) {
        return Some(finding(
            path,
            "private environment file is not public source",
            "remove it from Git and keep only sanitized .env.example files",
        ));
    }
    if private_runtime_path(path) {
        return Some(finding(
            path,
            "private/runtime path is not public source",
            "remove it from Git, move it to ignored local state, or confirm the repo is private",
        ));
    }
    if private_key_or_database_path(path) {
        return Some(finding(
            path,
            "private key or local database path is not public source",
            "remove it from Git and rotate credentials if a real secret was committed",
        ));
    }
    None
}

fn finding(path: &str, reason: &str, remediation: &str) -> SafetyFinding {
    SafetyFinding {
        severity: SafetySeverity::Fatal,
        path: Some(path.to_string()),
        reason: reason.to_string(),
        remediation: remediation.to_string(),
    }
}

fn generated_cache_path(path: &str) -> bool {
    path_has_component(path, "node_modules") || path_has_component(path, "target")
}

fn agent_notes_path(path: &str) -> bool {
    path.rsplit('/').next() == Some("AGENTS.md")
}

fn private_env_path(path: &str) -> bool {
    let name = path.rsplit('/').next().unwrap_or(path);
    name.starts_with(".env") && name != ".env.example"
}

fn private_runtime_path(path: &str) -> bool {
    path.starts_with("private/")
        || path.starts_with("docs/private/")
        || path.starts_with(".codex/")
        || path.starts_with(".claude/")
        || path.starts_with("state/")
        || path.starts_with("uploads/")
        || path.starts_with("logs/")
}

fn private_key_or_database_path(path: &str) -> bool {
    let name = path.rsplit('/').next().unwrap_or(path);
    matches!(name, "id_rsa" | "id_ed25519")
        || name.ends_with(".pem")
        || name.ends_with(".key")
        || name.ends_with(".sqlite")
        || name.ends_with(".sqlite3")
        || name.ends_with(".db")
}

fn path_has_component(path: &str, component: &str) -> bool {
    path.split('/').any(|part| part == component)
}

fn sync_paths_with_config(entry: &RepoEntry, config: &LocalConfig) -> Result<Vec<ChangedPath>> {
    let mut paths = dirty_paths(&entry.path)?;
    let mut seen: BTreeSet<String> = paths.iter().map(|path| path.path.clone()).collect();

    for path in configured_local_sync_files(entry, config) {
        if seen.contains(&path) || !local_sync_file_exists(&entry.path, &path) {
            continue;
        }
        if git_tracked_path(&entry.path, &path) {
            continue;
        }
        seen.insert(path.clone());
        paths.push(ChangedPath {
            path,
            kind: ChangeKind::Local,
        });
    }

    paths.sort_by(|a, b| a.path.cmp(&b.path));
    Ok(paths)
}

fn configured_local_sync_files(entry: &RepoEntry, config: &LocalConfig) -> Vec<String> {
    let Some(map) = &config.local_sync_files else {
        return Vec::new();
    };
    let repo_name = entry
        .path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("");
    let keys = ["*", entry.id.as_str(), repo_name];
    let mut files = Vec::new();
    let mut seen = BTreeSet::new();

    for key in keys {
        let Some(values) = map.get(key) else { continue };
        for value in values {
            let Some(path) = safe_relative_local_sync_path(value) else {
                continue;
            };
            if seen.insert(path.clone()) {
                files.push(path);
            }
        }
    }
    files
}

fn configured_trusted_wip_devices(entry: &RepoEntry, config: &LocalConfig) -> Vec<String> {
    let Some(map) = &config.trusted_wip_devices else {
        return Vec::new();
    };
    let repo_name = entry
        .path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("");
    let keys = ["*", entry.id.as_str(), repo_name];
    let mut devices = Vec::new();
    let mut seen = BTreeSet::new();

    for key in keys {
        let Some(values) = map.get(key) else { continue };
        for value in values {
            let Some(device) = normalize_device_id(value) else {
                continue;
            };
            if seen.insert(device.clone()) {
                devices.push(device);
            }
        }
    }
    devices
}

fn wip_device_trusted_with_config(entry: &RepoEntry, device: &str, config: &LocalConfig) -> bool {
    let Some(device) = normalize_device_id(device) else {
        return false;
    };
    configured_trusted_wip_devices(entry, config)
        .iter()
        .any(|trusted| trusted == &device)
}

fn safe_relative_local_sync_path(value: &str) -> Option<String> {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return None;
    }
    let path = Path::new(trimmed);
    if path.is_absolute() {
        return None;
    }

    let mut parts = Vec::new();
    for component in path.components() {
        match component {
            std::path::Component::CurDir => {}
            std::path::Component::Normal(part) if part != OsStr::new(".git") => {
                parts.push(part.to_string_lossy().to_string());
            }
            _ => return None,
        }
    }

    (!parts.is_empty()).then(|| parts.join("/"))
}

fn normalize_device_id(value: &str) -> Option<String> {
    let device = sanitize_ref_component(value.trim());
    if device.is_empty() || device.contains('/') {
        return None;
    }
    Some(device)
}

fn local_sync_file_exists(repo: &Path, path: &str) -> bool {
    fs::symlink_metadata(repo.join(path))
        .map(|metadata| metadata.file_type().is_file())
        .unwrap_or(false)
}

fn git_tracked_path(repo: &Path, path: &str) -> bool {
    let output = git_command_output(git_command_with_paths(
        repo,
        ["ls-files", "--error-unmatch"],
        &[path.to_string()],
    ));
    output
        .map(|output| output.status.success())
        .unwrap_or(false)
}

fn parse_porcelain_v2_z(output: &[u8]) -> Vec<ChangedPath> {
    let mut paths = Vec::new();
    let mut parts = output
        .split(|byte| *byte == 0)
        .filter(|part| !part.is_empty());

    while let Some(part) = parts.next() {
        let text = String::from_utf8_lossy(part);
        if let Some(path) = text.strip_prefix("? ") {
            paths.push(ChangedPath {
                path: path.to_string(),
                kind: ChangeKind::New,
            });
            continue;
        }
        if text.starts_with("1 ") || text.starts_with("u ") {
            if let Some(path) = text.rsplit_once(' ').map(|(_, path)| path) {
                paths.push(ChangedPath {
                    path: path.to_string(),
                    kind: ChangeKind::Tracked,
                });
            }
            continue;
        }
        if text.starts_with("2 ") {
            if let Some(path) = text.rsplit_once(' ').map(|(_, path)| path) {
                paths.push(ChangedPath {
                    path: path.to_string(),
                    kind: ChangeKind::Tracked,
                });
            }
            let _ = parts.next();
        }
    }

    paths.sort_by(|a, b| a.path.cmp(&b.path));
    paths.dedup_by(|a, b| a.path == b.path);
    paths
}

fn ahead_behind(repo: &Path, tracking_branch: &str) -> Result<(Option<u32>, Option<u32>)> {
    let output = git_output(
        repo,
        [
            "rev-list",
            "--left-right",
            "--count",
            &format!("{tracking_branch}...HEAD"),
        ],
    )?;
    let mut fields = output.split_whitespace();
    let behind = fields.next().and_then(|value| value.parse().ok());
    let ahead = fields.next().and_then(|value| value.parse().ok());
    Ok((ahead, behind))
}

fn create_wip_snapshot(
    repo: &Path,
    branch: &str,
    dirty: &[ChangedPath],
    identity: Option<&GitIdentityProfile>,
) -> Result<String> {
    let temp_index = temp_index_path(repo)?;
    let mut cleanup = CleanupFile(temp_index.clone());

    git_env(
        repo,
        ["read-tree", "HEAD"],
        [("GIT_INDEX_FILE", temp_index.as_path())],
    )?;
    let paths: Vec<String> = dirty.iter().map(|path| path.path.clone()).collect();
    git_paths_env(
        repo,
        ["add", "-A", "-f"],
        &paths,
        [("GIT_INDEX_FILE", temp_index.as_path())],
    )?;

    let tree = git_output_env(
        repo,
        ["write-tree"],
        [("GIT_INDEX_FILE", temp_index.as_path())],
    )?
    .trim()
    .to_string();
    if let Some(existing) = local_wip(repo, branch)? {
        if commit_tree(repo, &existing.sha)
            .map(|existing_tree| existing_tree == tree)
            .unwrap_or(false)
        {
            return Ok(existing.sha);
        }
    }
    let device = repo_device_id(repo);
    let message = format!("GitDCY WIP from {device} on {branch}");
    let parent = git_output(repo, ["rev-parse", "HEAD"])?.trim().to_string();
    let sha = if let Some(identity) = identity {
        git_output_env(
            repo,
            ["commit-tree", &tree, "-p", &parent, "-m", &message],
            [
                ("GIT_AUTHOR_NAME", identity.name.as_str()),
                ("GIT_AUTHOR_EMAIL", identity.email.as_str()),
                ("GIT_COMMITTER_NAME", identity.name.as_str()),
                ("GIT_COMMITTER_EMAIL", identity.email.as_str()),
            ],
        )?
        .trim()
        .to_string()
    } else {
        git_output(repo, ["commit-tree", &tree, "-p", &parent, "-m", &message])?
            .trim()
            .to_string()
    };
    let local_ref = local_wip_ref(&device, branch);
    git(repo, ["update-ref", &local_ref, &sha])?;

    cleanup.0 = PathBuf::new();
    let _ = fs::remove_file(temp_index);
    Ok(sha)
}

fn push_wip_snapshot(repo: &Path, remote: &str, branch: &str, sha: &str) -> Result<()> {
    let device = repo_device_id(repo);
    let refname = local_wip_ref(&device, branch);
    git(repo, ["update-ref", &refname, sha])?;
    let remote_ref = format!(
        "refs/gitdcy/wip/{}/{}",
        device,
        sanitize_ref_component(branch)
    );
    let refspec = format!("{refname}:{remote_ref}");
    git(repo, ["push", remote, &refspec])?;
    Ok(())
}

fn fetch_wip_refs(repo: &Path, remote: &str) -> Result<()> {
    let refspec = format!("+refs/gitdcy/wip/*:{WIP_REMOTE}/*");
    git(repo, vec!["fetch", "--prune", remote, refspec.as_str()])?;
    Ok(())
}

fn latest_incoming_wip(repo: &Path, branch: &str) -> Result<Option<WipRef>> {
    let current_device = repo_device_id(repo);
    let refs = wip_refs(repo, branch, WIP_REMOTE)?;
    Ok(refs
        .into_iter()
        .filter(|wip| wip.device != current_device)
        .max_by_key(|wip| wip.timestamp))
}

fn local_wip(repo: &Path, branch: &str) -> Result<Option<WipRef>> {
    let refs = wip_refs(repo, branch, WIP_HEAD)?;
    Ok(refs.into_iter().max_by_key(|wip| wip.timestamp))
}

fn wip_refs(repo: &Path, branch: &str, prefix: &str) -> Result<Vec<WipRef>> {
    let output = git_output(
        repo,
        ["for-each-ref", "--format=%(refname) %(objectname)", prefix],
    )?;
    let branch_component = sanitize_ref_component(branch);
    let mut refs = Vec::new();

    for line in output.lines() {
        let Some((refname, sha)) = line.split_once(' ') else {
            continue;
        };
        let Some(short) = refname.strip_prefix(&(prefix.to_string() + "/")) else {
            continue;
        };
        let Some((device, ref_branch)) = short.split_once('/') else {
            continue;
        };
        if ref_branch != branch_component {
            continue;
        }
        let timestamp = commit_timestamp(repo, sha).unwrap_or(0);
        refs.push(WipRef {
            refname: refname.to_string(),
            short_name: short.to_string(),
            device: device.to_string(),
            branch: ref_branch.to_string(),
            sha: sha.to_string(),
            timestamp,
        });
    }

    Ok(refs)
}

fn apply_latest_incoming_wip(
    entry: &RepoEntry,
    branch: &str,
    config: &LocalConfig,
) -> Result<Option<WipRef>> {
    let repo = &entry.path;
    let Some(wip) = latest_incoming_wip(repo, branch)? else {
        return Ok(None);
    };

    if !wip_device_trusted_with_config(entry, &wip.device, config) {
        bail!(
            "incoming WIP from untrusted device {}; approve it before syncing",
            wip.device
        );
    }

    let applied_ref = applied_wip_ref(&wip.device, branch);
    if git_output(repo, ["rev-parse", "--verify", "--quiet", &applied_ref])
        .map(|sha| sha.trim() == wip.sha)
        .unwrap_or(false)
    {
        return Ok(None);
    }

    let parent = git_output(repo, ["rev-parse", &format!("{}^", wip.sha)])?
        .trim()
        .to_string();
    let incoming_files = changed_files_between(repo, &parent, &wip.sha)?;
    let local_dirty = sync_paths_with_config(entry, config)?;
    if !local_dirty.is_empty() {
        let local_files: BTreeSet<_> = local_dirty.iter().map(|path| path.path.as_str()).collect();
        if let Some(overlap) = incoming_files
            .iter()
            .find(|path| local_files.contains(path.as_str()))
        {
            bail!("incoming WIP from {} also changes {overlap}", wip.device);
        }
    }

    let diff = git_bytes(repo, ["diff", "--binary", &parent, &wip.sha])?;
    git_apply(repo, &diff, true)?;
    git_apply(repo, &diff, false)?;
    unstage_ignored_paths(repo, &incoming_files)?;
    git(repo, ["update-ref", &applied_ref, &wip.sha])?;
    Ok(Some(wip))
}

fn changed_files_between(repo: &Path, from: &str, to: &str) -> Result<Vec<String>> {
    let output = git_output(repo, ["diff", "--name-only", from, to])?;
    Ok(output
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(ToOwned::to_owned)
        .collect())
}

fn unstage_ignored_paths(repo: &Path, paths: &[String]) -> Result<()> {
    let ignored: Vec<String> = paths
        .iter()
        .filter(|path| git_ignored_path(repo, path))
        .cloned()
        .collect();
    if !ignored.is_empty() {
        git_paths(repo, ["reset", "-q"], &ignored)?;
    }
    Ok(())
}

fn git_ignored_path(repo: &Path, path: &str) -> bool {
    let output = git_command_output(git_command_with_paths(
        repo,
        ["check-ignore", "--no-index", "-q"],
        &[path.to_string()],
    ));
    output
        .map(|output| output.status.success())
        .unwrap_or(false)
}

fn commit_timestamp(repo: &Path, sha: &str) -> Result<i64> {
    Ok(git_output(repo, ["show", "-s", "--format=%ct", sha])?
        .trim()
        .parse()
        .unwrap_or(0))
}

fn commit_tree(repo: &Path, sha: &str) -> Result<String> {
    Ok(git_output(repo, ["show", "-s", "--format=%T", sha])?
        .trim()
        .to_string())
}

fn git_apply(repo: &Path, diff: &[u8], check: bool) -> Result<()> {
    let mut command = Command::new("git");
    command.arg("-C").arg(repo).arg("apply").arg("--3way");
    if check {
        command.arg("--check");
    }
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .context("spawn git apply")?;
    child
        .stdin
        .as_mut()
        .context("open git apply stdin")?
        .write_all(diff)
        .context("write patch to git apply")?;
    let output = child.wait_with_output().context("wait for git apply")?;
    if !output.status.success() {
        bail!("{}", command_error("git apply", &output));
    }
    Ok(())
}

fn git<I, S>(repo: &Path, args: I) -> Result<()>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output = git_command_output(git_command(repo, args)).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(())
}

fn git_output<I, S>(repo: &Path, args: I) -> Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output = git_command_output(git_command(repo, args)).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(String::from_utf8_lossy(&output.stdout).to_string())
}

fn git_bytes<I, S>(repo: &Path, args: I) -> Result<Vec<u8>>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output = git_command_output(git_command(repo, args)).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(output.stdout)
}

fn git_env<I, S, E, K, V>(repo: &Path, args: I, envs: E) -> Result<()>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
    E: IntoIterator<Item = (K, V)>,
    K: AsRef<OsStr>,
    V: AsRef<OsStr>,
{
    let mut command = git_command(repo, args);
    command.envs(envs);
    let output = git_command_output(command).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(())
}

fn git_output_env<I, S, E, K, V>(repo: &Path, args: I, envs: E) -> Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
    E: IntoIterator<Item = (K, V)>,
    K: AsRef<OsStr>,
    V: AsRef<OsStr>,
{
    let mut command = git_command(repo, args);
    command.envs(envs);
    let output = git_command_output(command).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(String::from_utf8_lossy(&output.stdout).to_string())
}

fn git_paths<I, S>(repo: &Path, args: I, paths: &[String]) -> Result<()>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output =
        git_command_output(git_command_with_paths(repo, args, paths)).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(())
}

fn git_paths_env<I, S, E, K, V>(repo: &Path, args: I, paths: &[String], envs: E) -> Result<()>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
    E: IntoIterator<Item = (K, V)>,
    K: AsRef<OsStr>,
    V: AsRef<OsStr>,
{
    let mut command = git_command_with_paths(repo, args, paths);
    command.envs(envs);
    let output = git_command_output(command).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(())
}

fn git_command<I, S>(repo: &Path, args: I) -> Command
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let mut command = Command::new("git");
    command.arg("-C").arg(repo);
    for arg in args {
        command.arg(arg);
    }
    command
}

fn linuxmice_read_only_status() -> bool {
    std::env::var_os("LINUXMICE_READ_ONLY_STATUS").as_deref() == Some(OsStr::new("1"))
}

fn git_command_output(mut command: Command) -> Result<std::process::Output> {
    if linuxmice_read_only_status() {
        process::run_bounded_command(command, LINUXMICE_GIT_OUTPUT_LIMIT, LINUXMICE_GIT_TIMEOUT)
    } else {
        command.output().context("run git command")
    }
}

fn git_command_with_paths<I, S>(repo: &Path, args: I, paths: &[String]) -> Command
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let mut command = git_command(repo, args);
    command.arg("--");
    for path in paths {
        command.arg(path);
    }
    command
}

fn command_error(name: &str, output: &std::process::Output) -> String {
    let stderr = String::from_utf8_lossy(&output.stderr);
    let stdout = String::from_utf8_lossy(&output.stdout);
    let detail = if !stderr.trim().is_empty() {
        stderr.trim()
    } else {
        stdout.trim()
    };
    if detail.is_empty() {
        format!("{name} failed with {}", output.status)
    } else {
        detail.to_string()
    }
}

fn temp_index_path(repo: &Path) -> Result<PathBuf> {
    let git_dir = git_output(repo, ["rev-parse", "--git-dir"])?
        .trim()
        .to_string();
    let git_dir = if Path::new(&git_dir).is_absolute() {
        PathBuf::from(git_dir)
    } else {
        repo.join(git_dir)
    };
    let unique = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    Ok(git_dir.join(format!("gitdcy-index-{unique}")))
}

struct CleanupFile(PathBuf);

impl Drop for CleanupFile {
    fn drop(&mut self) {
        if !self.0.as_os_str().is_empty() {
            let _ = fs::remove_file(&self.0);
        }
    }
}

fn repo_id(path: &Path, provider: Provider) -> String {
    let name = path
        .file_name()
        .and_then(OsStr::to_str)
        .unwrap_or("repo")
        .to_string();
    format!("{}/{}", provider.folder(), name)
}

fn repo_name_from_url(url: &str) -> String {
    let trimmed = url.trim_end_matches('/');
    let last = trimmed
        .rsplit(['/', ':'])
        .next()
        .filter(|value| !value.is_empty())
        .unwrap_or("repo");
    last.strip_suffix(".git").unwrap_or(last).to_string()
}

fn sanitize_component(value: &str) -> String {
    value
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '.' | '-' | '_') {
                ch
            } else {
                '-'
            }
        })
        .collect()
}

fn sanitize_ref_component(value: &str) -> String {
    let sanitized = value
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '.' | '-' | '_' | '/') {
                ch
            } else {
                '-'
            }
        })
        .collect::<String>();
    sanitized
        .trim_matches('/')
        .replace("//", "/")
        .trim_end_matches(".lock")
        .to_string()
}

fn local_wip_ref(device: &str, branch: &str) -> String {
    format!(
        "{WIP_HEAD}/{}/{}",
        sanitize_ref_component(device),
        sanitize_ref_component(branch)
    )
}

fn applied_wip_ref(device: &str, branch: &str) -> String {
    format!(
        "{WIP_APPLIED}/{}/{}",
        sanitize_ref_component(device),
        sanitize_ref_component(branch)
    )
}

fn wip_remote_name(remotes: &BTreeMap<String, String>) -> Option<String> {
    if remotes.contains_key(SYNC_REMOTE) {
        return Some(SYNC_REMOTE.to_string());
    }
    remotes
        .get("origin")
        .filter(|url| Provider::from_url(url) == Provider::Forgejo)
        .map(|_| "origin".to_string())
}

fn apply_remote_template(template: &str, entry: &RepoEntry) -> String {
    let repo_name = entry
        .path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("repo");
    template
        .replace("{repo}", repo_name)
        .replace("{id}", &entry.id)
        .replace("{provider}", entry.provider.folder())
}

fn short_sha(sha: &str) -> String {
    sha.chars().take(8).collect()
}

pub fn device_id() -> String {
    let raw = env::var("GITDCY_DEVICE")
        .or_else(|_| env::var("COMPUTERNAME"))
        .or_else(|_| env::var("HOSTNAME"))
        .ok()
        .filter(|value| !value.trim().is_empty())
        .or_else(|| {
            if linuxmice_read_only_status() {
                Some("linuxmice-status".to_string())
            } else {
                Command::new("hostname")
                    .output()
                    .ok()
                    .and_then(|output| String::from_utf8(output.stdout).ok())
            }
        })
        .unwrap_or_else(|| "device".to_string());
    sanitize_ref_component(raw.trim())
}

fn repo_device_id(repo: &Path) -> String {
    git_output(repo, ["config", "--get", "gitdcy.device"])
        .ok()
        .map(|value| sanitize_ref_component(value.trim()))
        .filter(|value| !value.is_empty())
        .unwrap_or_else(device_id)
}

fn home_dir() -> Option<PathBuf> {
    if cfg!(windows) {
        env::var_os("USERPROFILE").map(PathBuf::from)
    } else {
        env::var_os("HOME").map(PathBuf::from)
    }
}

pub fn load_local_config() -> LocalConfig {
    let mut config = load_saved_local_config();
    if let Ok(current_dir) = env::current_dir() {
        config = merge_local_config(
            config,
            read_local_config_file(&current_dir.join(".gitdcy.local.yaml")),
        );
    }
    config
}

fn load_saved_local_config() -> LocalConfig {
    local_config_path()
        .ok()
        .map(|path| read_local_config_file(&path))
        .unwrap_or_default()
}

fn save_local_config(config: &LocalConfig) -> Result<PathBuf> {
    let path = local_config_path()?;
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("create config directory {}", parent.display()))?;
    }
    let text = serde_norway::to_string(config)?;
    fs::write(&path, text).with_context(|| format!("write local config {}", path.display()))?;
    Ok(path)
}

fn read_local_config_file(path: &Path) -> LocalConfig {
    let text = match fs::read_to_string(path) {
        Ok(text) => text,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return LocalConfig::default();
        }
        Err(error) => {
            return invalid_local_config(format!(
                "could not read GitDCY config {}: {error}",
                path.display()
            ));
        }
    };
    match serde_norway::from_str::<LocalConfig>(&text) {
        Ok(config) => config,
        Err(error) => invalid_local_config(format!(
            "could not parse GitDCY config {}: {error}",
            path.display()
        )),
    }
}

fn invalid_local_config(error: String) -> LocalConfig {
    let mut identity_profiles = BTreeMap::new();
    identity_profiles.insert(
        "__invalid_local_config__".to_string(),
        GitIdentityProfile::default(),
    );
    LocalConfig {
        require_identity: Some(true),
        identity_profiles: Some(identity_profiles),
        config_error: Some(error),
        ..LocalConfig::default()
    }
}

fn merge_local_config(mut base: LocalConfig, next: LocalConfig) -> LocalConfig {
    if next.workspace_root.is_some() {
        base.workspace_root = next.workspace_root;
    }
    if next.scan_roots.is_some() {
        base.scan_roots = next.scan_roots;
    }
    if next.sync_remote_template.is_some() {
        base.sync_remote_template = next.sync_remote_template;
    }
    if let Some(next_templates) = next.origin_remote_templates {
        base.origin_remote_templates
            .get_or_insert_with(BTreeMap::new)
            .extend(next_templates);
    }
    if let Some(next_files) = next.local_sync_files {
        merge_list_map(&mut base.local_sync_files, next_files);
    }
    if let Some(next_devices) = next.trusted_wip_devices {
        merge_list_map(&mut base.trusted_wip_devices, next_devices);
    }
    if let Some(next_overrides) = next.visibility_overrides {
        base.visibility_overrides
            .get_or_insert_with(BTreeMap::new)
            .extend(next_overrides);
    }
    if next.private_remote_patterns.is_some() {
        merge_optional_list(
            &mut base.private_remote_patterns,
            next.private_remote_patterns.unwrap_or_default(),
        );
    }
    if next.public_export_remotes.is_some() {
        merge_optional_list(
            &mut base.public_export_remotes,
            next.public_export_remotes.unwrap_or_default(),
        );
    }
    if let Some(next_profiles) = next.ignore_profiles {
        merge_list_map(&mut base.ignore_profiles, next_profiles);
    }
    if next.require_identity.is_some() {
        base.require_identity = next.require_identity;
    }
    if let Some(next_identities) = next.identity_profiles {
        base.identity_profiles
            .get_or_insert_with(BTreeMap::new)
            .extend(next_identities);
    }
    if next.require_checks.is_some() {
        base.require_checks = next.require_checks;
    }
    if let Some(next_checks) = next.check_profiles {
        base.check_profiles
            .get_or_insert_with(BTreeMap::new)
            .extend(next_checks);
    }
    if next.config_error.is_some() {
        base.config_error = next.config_error;
    }
    base
}

fn merge_optional_list(base: &mut Option<Vec<String>>, mut next: Vec<String>) {
    let base = base.get_or_insert_with(Vec::new);
    base.append(&mut next);
    base.sort();
    base.dedup();
}

fn merge_list_map(
    base: &mut Option<BTreeMap<String, Vec<String>>>,
    next: BTreeMap<String, Vec<String>>,
) {
    let base = base.get_or_insert_with(BTreeMap::new);
    for (key, mut values) in next {
        let existing = base.entry(key).or_default();
        existing.append(&mut values);
        existing.sort();
        existing.dedup();
    }
}

fn ensure_scan_root(config: &mut LocalConfig, root: PathBuf) {
    let roots = config.scan_roots.get_or_insert_with(Vec::new);
    if !roots
        .iter()
        .any(|configured| expand_home(configured.clone()) == root)
    {
        roots.push(root);
    }
    roots.sort_by(|a, b| a.to_string_lossy().cmp(&b.to_string_lossy()));
}

fn workspace_root_with_config(config: &LocalConfig) -> PathBuf {
    config
        .workspace_root
        .clone()
        .map(expand_home)
        .unwrap_or_else(|| {
            home_dir()
                .unwrap_or_else(|| PathBuf::from("."))
                .join("Code")
        })
}

fn expand_home(path: PathBuf) -> PathBuf {
    let text = path.to_string_lossy();
    if text == "~" {
        return home_dir().unwrap_or(path);
    }
    if let Some(rest) = text.strip_prefix("~/") {
        if let Some(home) = home_dir() {
            return home.join(rest);
        }
    }
    path
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::Command;

    #[test]
    fn linuxmice_read_only_status_workers_preserve_manifest_order() {
        let manifest = WorkspaceManifest {
            workspace_root: PathBuf::from("/nonexistent"),
            repos: vec![
                entry("first", Path::new("/nonexistent/first")),
                entry("second", Path::new("/nonexistent/second")),
            ],
        };
        let statuses = status_all_for_mode(&manifest, true);
        assert_eq!(statuses.len(), 2);
        assert_eq!(statuses[0].entry.id, "first");
        assert_eq!(statuses[1].entry.id, "second");
        assert!(statuses.iter().all(|status| status.last_error.is_some()));
    }

    #[test]
    fn linuxmice_fallback_discovery_is_one_level() {
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = env::temp_dir().join(format!("gitdcy-shallow-discovery-{unique}"));
        let direct = root.join("direct");
        let nested = root.join("group").join("nested");
        fs::create_dir_all(direct.join(".git")).unwrap();
        fs::create_dir_all(nested.join(".git")).unwrap();
        let mut budget = DiscoveryBudget::for_mode(true);
        let found = discover_repo_paths_with_budget(&root, &mut budget).unwrap();
        assert_eq!(found, vec![direct]);
        fs::remove_dir_all(root).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn repository_discovery_does_not_follow_directory_symlinks() {
        use std::os::unix::fs::symlink;

        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = env::temp_dir().join(format!("gitdcy-discovery-{unique}"));
        let repository = root.join("repository");
        fs::create_dir_all(repository.join(".git")).unwrap();
        symlink(&root, root.join("loop")).unwrap();
        let found = discover_repo_paths(&root).unwrap();
        assert_eq!(found, vec![repository]);
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn parses_porcelain_tracked_and_new_paths() {
        let input = b"1 .M N... 100644 100644 100644 abc abc src/main.rs\0? src/new.rs\0";
        let parsed = parse_porcelain_v2_z(input);
        assert_eq!(parsed.len(), 2);
        assert_eq!(parsed[0].path, "src/main.rs");
        assert_eq!(parsed[0].kind, ChangeKind::Tracked);
        assert_eq!(parsed[1].path, "src/new.rs");
        assert_eq!(parsed[1].kind, ChangeKind::New);
    }

    #[test]
    fn derives_repo_name_from_common_urls() {
        assert_eq!(
            repo_name_from_url("https://github.com/example/gitdcy.git"),
            "gitdcy"
        );
        assert_eq!(
            repo_name_from_url("git@gitlab.com:example/orbit.git"),
            "orbit"
        );
    }

    #[test]
    fn routes_providers_from_urls() {
        assert_eq!(
            Provider::from_url("https://github.com/a/b.git"),
            Provider::Github
        );
        assert_eq!(
            Provider::from_url("git@github-special:owner/example.git"),
            Provider::Github
        );
        assert_eq!(
            Provider::from_url("git@gitlab.com:a/b.git"),
            Provider::Gitlab
        );
        assert_eq!(
            Provider::from_url("ssh://git@forgejo.example/a/b.git"),
            Provider::Forgejo
        );
    }

    #[test]
    fn remote_identity_selector_matches_host_alias_not_an_unrelated_path_fragment() {
        let profile = GitIdentityProfile {
            name: "Profile".to_string(),
            email: "profile@example.invalid".to_string(),
            remote_patterns: vec!["github-special".to_string()],
            providers: vec![Provider::Github],
            ..GitIdentityProfile::default()
        };
        let wrong_remote = BTreeMap::from([(
            "origin".to_string(),
            "ssh://git@other-host/team/github-special.git".to_string(),
        )]);
        assert!(!identity_profile_matches(
            &profile,
            Path::new("/tmp/repo"),
            Provider::Github,
            &wrong_remote
        ));

        let right_remote = BTreeMap::from([(
            "origin".to_string(),
            "git@github-special:owner/repo.git".to_string(),
        )]);
        assert!(identity_profile_matches(
            &profile,
            Path::new("/tmp/repo"),
            Provider::Github,
            &right_remote
        ));
    }

    #[test]
    fn identity_profiles_deserialize_from_local_yaml() {
        let config: LocalConfig = serde_norway::from_str(
            r#"require_identity: true
identity_profiles:
  forgejo-selfhosted:
    name: Forgejo User
    email: forgejo@example.invalid
    providers:
      - forgejo
"#,
        )
        .unwrap();

        assert_eq!(config.require_identity, Some(true));
        let profile = config
            .identity_profiles
            .as_ref()
            .and_then(|profiles| profiles.get("forgejo-selfhosted"))
            .unwrap();
        assert_eq!(profile.name, "Forgejo User");
        assert_eq!(profile.email, "forgejo@example.invalid");
        assert_eq!(profile.providers, vec![Provider::Forgejo]);
    }

    #[test]
    fn incomplete_identity_profile_is_invalid_and_fail_closed() {
        let config: LocalConfig = serde_norway::from_str(
            r#"require_identity: true
identity_profiles:
  incomplete:
    providers:
      - forgejo
"#,
        )
        .unwrap();
        let entry = entry("forgejo/incomplete", Path::new("/tmp/incomplete"));
        let report = identity_report_with_config(&entry, &BTreeMap::new(), &config);

        assert_eq!(report.state, IdentityState::Invalid, "{report:?}");
        assert!(report.is_blocking(), "{report:?}");
    }

    #[test]
    fn identity_profile_requires_all_declared_selectors_and_matches_git_identity() {
        let fixture = GitFixture::new("identity_match");
        let repo = fixture.clone_repo("repo");
        let mut identity_profiles = BTreeMap::new();
        identity_profiles.insert(
            "fixture-github".to_string(),
            GitIdentityProfile {
                name: "GitDCY Test".to_string(),
                email: "gitdcy@example.invalid".to_string(),
                path_prefixes: vec![fixture.root.clone()],
                remote_patterns: vec!["origin".to_string()],
                providers: vec![Provider::Github],
            },
        );
        let config = LocalConfig {
            require_identity: Some(true),
            identity_profiles: Some(identity_profiles),
            ..LocalConfig::default()
        };
        let entry = entry("github/fixture", &repo);
        let report = identity_report_with_config(&entry, &remotes(&repo).unwrap(), &config);

        assert_eq!(report.state, IdentityState::Matched, "{report:?}");
        assert!(!report.is_blocking(), "{report:?}");
        assert_eq!(report.profile.as_deref(), Some("fixture-github"));
        assert_eq!(
            report.expected_display(),
            "GitDCY Test <gitdcy@example.invalid>"
        );
        assert_eq!(
            report.actual_display(),
            "GitDCY Test <gitdcy@example.invalid>"
        );
    }

    #[test]
    fn identity_profile_mismatch_blocks_sync_before_worktree_actions() {
        let fixture = GitFixture::new("identity_mismatch");
        let repo = fixture.clone_repo("repo");
        let mut identity_profiles = BTreeMap::new();
        identity_profiles.insert(
            "wrong-profile".to_string(),
            GitIdentityProfile {
                name: "Wrong User".to_string(),
                email: "wrong@example.invalid".to_string(),
                path_prefixes: vec![fixture.root.clone()],
                remote_patterns: vec!["origin".to_string()],
                providers: vec![Provider::Github],
            },
        );
        let config = LocalConfig {
            require_identity: Some(true),
            identity_profiles: Some(identity_profiles),
            ..LocalConfig::default()
        };
        let report = sync_repo_with_config(&entry("github/fixture", &repo), &config);

        assert!(
            report
                .blocked
                .as_deref()
                .is_some_and(|reason| reason.contains("identity check blocked")),
            "{report:?}"
        );
        assert!(
            report.actions.is_empty(),
            "identity failure must happen before sync actions: {report:?}"
        );
    }

    #[test]
    fn identity_profile_ambiguity_is_blocking() {
        let fixture = GitFixture::new("identity_ambiguous");
        let repo = fixture.clone_repo("repo");
        let profile = GitIdentityProfile {
            name: "GitDCY Test".to_string(),
            email: "gitdcy@example.invalid".to_string(),
            path_prefixes: vec![fixture.root.clone()],
            remote_patterns: vec!["origin".to_string()],
            providers: vec![Provider::Github],
        };
        let mut identity_profiles = BTreeMap::new();
        identity_profiles.insert("first".to_string(), profile.clone());
        identity_profiles.insert("second".to_string(), profile);
        let config = LocalConfig {
            require_identity: Some(true),
            identity_profiles: Some(identity_profiles),
            ..LocalConfig::default()
        };
        let report = identity_report_with_config(
            &entry("github/fixture", &repo),
            &remotes(&repo).unwrap(),
            &config,
        );

        assert_eq!(report.state, IdentityState::Ambiguous, "{report:?}");
        assert!(report.is_blocking(), "{report:?}");
        assert_eq!(report.candidates, vec!["first", "second"]);
    }

    #[test]
    fn forgejo_provider_profile_covers_repos_without_an_origin_remote() {
        let fixture = GitFixture::new("identity_forgejo");
        let repo = fixture.clone_repo("repo");
        run(&repo, ["remote", "remove", "origin"]);
        let mut identity_profiles = BTreeMap::new();
        identity_profiles.insert(
            "forgejo-selfhosted".to_string(),
            GitIdentityProfile {
                name: "GitDCY Test".to_string(),
                email: "gitdcy@example.invalid".to_string(),
                providers: vec![Provider::Forgejo],
                ..GitIdentityProfile::default()
            },
        );
        let config = LocalConfig {
            require_identity: Some(true),
            identity_profiles: Some(identity_profiles),
            ..LocalConfig::default()
        };
        let mut forgejo_entry = entry("forgejo/fixture", &repo);
        forgejo_entry.provider = Provider::Forgejo;
        let report = identity_report_with_config(&forgejo_entry, &BTreeMap::new(), &config);

        assert_eq!(report.state, IdentityState::Matched, "{report:?}");
        assert!(!report.is_blocking(), "{report:?}");
    }

    #[test]
    fn wip_snapshot_uses_the_selected_identity_explicitly() {
        let fixture = GitFixture::new("identity_wip");
        let repo = fixture.clone_repo("repo");
        fs::write(repo.join("README.md"), "profile-owned WIP\n").unwrap();
        let dirty = dirty_paths(&repo).unwrap();
        let profile = GitIdentityProfile {
            name: "WIP Profile".to_string(),
            email: "wip-profile@example.invalid".to_string(),
            ..GitIdentityProfile::default()
        };

        let sha = create_wip_snapshot(&repo, "main", &dirty, Some(&profile)).unwrap();
        let identity = git_output(&repo, ["show", "-s", "--format=%an <%ae> %cn <%ce>", &sha])
            .unwrap()
            .trim()
            .to_string();

        assert_eq!(
            identity,
            "WIP Profile <wip-profile@example.invalid> WIP Profile <wip-profile@example.invalid>"
        );
    }

    #[test]
    fn check_profiles_deserialize_with_direct_ci_commands() {
        let config: LocalConfig = serde_norway::from_str(
            r#"check_profiles:
  rust-project:
    path_prefixes:
      - ~/Documents/example
    remote_patterns:
      - github.com/example/project
    checks:
      - name: format
        program: cargo
        args: [fmt, --all, --check]
        timeout_seconds: 900
"#,
        )
        .unwrap();

        let profile = config
            .check_profiles
            .as_ref()
            .and_then(|profiles| profiles.get("rust-project"))
            .unwrap();
        assert!(profile.run_before_push);
        assert!(profile.require_clean_worktree);
        assert_eq!(
            profile.checks[0].display_command(),
            "cargo fmt --all --check"
        );
        assert_eq!(profile.checks[0].timeout_seconds, Some(900));
    }

    #[test]
    fn configured_checks_run_and_report_failures_without_a_shell() {
        let fixture = GitFixture::new("checks_run");
        let repo = fixture.clone_repo("repo");
        let profile = CheckProfile {
            path_prefixes: vec![fixture.root.clone()],
            remote_patterns: vec!["origin".to_string()],
            providers: vec![Provider::Github],
            checks: vec![CheckCommand {
                name: "git-status".to_string(),
                program: "git".to_string(),
                args: vec!["status".to_string(), "--porcelain".to_string()],
                ..CheckCommand::default()
            }],
            ..CheckProfile::default()
        };
        let config = LocalConfig {
            check_profiles: Some(BTreeMap::from([("checks".to_string(), profile)])),
            ..LocalConfig::default()
        };
        let entry = entry("github/fixture", &repo);
        let report = check_repo_with_config(
            &entry,
            &remotes(&repo).unwrap(),
            &config,
            CheckTrigger::Manual,
        );

        assert_eq!(report.state, CheckState::Passed, "{report:?}");
        assert!(!report.is_blocking(), "{report:?}");
        assert_eq!(report.results.len(), 1);
        assert_eq!(report.results[0].state, CheckResultState::Passed);
        assert_eq!(report.results[0].command, "git status --porcelain");

        let failing_profile = CheckProfile {
            checks: vec![CheckCommand {
                name: "missing-branch".to_string(),
                program: "git".to_string(),
                args: vec![
                    "rev-parse".to_string(),
                    "--verify".to_string(),
                    "refs/heads/branch-that-does-not-exist".to_string(),
                ],
                ..CheckCommand::default()
            }],
            path_prefixes: vec![fixture.root.clone()],
            remote_patterns: vec!["origin".to_string()],
            providers: vec![Provider::Github],
            ..CheckProfile::default()
        };
        let failing_config = LocalConfig {
            check_profiles: Some(BTreeMap::from([("checks".to_string(), failing_profile)])),
            ..LocalConfig::default()
        };
        let failing_report = check_repo_with_config(
            &entry,
            &remotes(&repo).unwrap(),
            &failing_config,
            CheckTrigger::Manual,
        );
        assert_eq!(
            failing_report.state,
            CheckState::Failed,
            "{failing_report:?}"
        );
        assert!(failing_report.is_blocking(), "{failing_report:?}");
        assert!(!failing_report.results[0].output.trim().is_empty());
    }

    #[test]
    fn push_checks_fail_closed_on_a_dirty_worktree() {
        let fixture = GitFixture::new("checks_dirty");
        let repo = fixture.clone_repo("repo");
        fs::write(repo.join("pending.txt"), "not committed\n").unwrap();
        let profile = CheckProfile {
            path_prefixes: vec![fixture.root.clone()],
            remote_patterns: vec!["origin".to_string()],
            providers: vec![Provider::Github],
            checks: vec![CheckCommand {
                name: "status".to_string(),
                program: "git".to_string(),
                args: vec!["status".to_string(), "--porcelain".to_string()],
                ..CheckCommand::default()
            }],
            ..CheckProfile::default()
        };
        let config = LocalConfig {
            check_profiles: Some(BTreeMap::from([("checks".to_string(), profile)])),
            ..LocalConfig::default()
        };
        let report = check_repo_with_config(
            &entry("github/fixture", &repo),
            &remotes(&repo).unwrap(),
            &config,
            CheckTrigger::BeforePush,
        );

        assert_eq!(report.state, CheckState::Failed, "{report:?}");
        assert!(report.is_blocking(), "{report:?}");
        assert_eq!(report.worktree_clean, Some(false));
        assert!(report.results.is_empty());
        assert!(report.message.contains("not clean"));
    }

    #[test]
    fn dirty_wip_moves_between_clones_without_ignored_files() {
        let fixture = GitFixture::new("dirty_wip_moves");
        let first = fixture.clone_repo("first");
        let second = fixture.clone_repo("second");
        run(
            &first,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(
            &second,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(&first, ["config", "gitdcy.device", "first-device"]);
        run(&second, ["config", "gitdcy.device", "second-device"]);

        fs::write(first.join("README.md"), "changed on first\n").unwrap();
        fs::write(first.join("new-source.rs"), "fn main() {}\n").unwrap();
        fs::create_dir_all(first.join("node_modules/pkg")).unwrap();
        fs::write(first.join("node_modules/pkg/ignored.js"), "ignored\n").unwrap();

        let first_entry = entry("github/fixture", &first);
        let second_entry = entry("github/fixture", &second);
        let config = config_trusting("github/fixture", &["first-device"]);

        let first_report = sync_repo_with_config(&first_entry, &config);
        assert!(first_report.blocked.is_none(), "{first_report:?}");

        let second_report = sync_repo_with_config(&second_entry, &config);
        assert!(second_report.blocked.is_none(), "{second_report:?}");
        assert_eq!(
            fs::read_to_string(second.join("README.md")).unwrap(),
            "changed on first\n"
        );
        assert!(second.join("new-source.rs").exists());
        assert!(!second.join("node_modules/pkg/ignored.js").exists());
        assert!(dirty_paths(&second)
            .unwrap()
            .iter()
            .any(|path| path.path == "new-source.rs"));
    }

    #[test]
    fn local_allowlist_moves_ignored_env_file() {
        let fixture = GitFixture::new("local_allowlist");
        let first = fixture.clone_repo("first");
        let second = fixture.clone_repo("second");
        run(
            &first,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(
            &second,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(&first, ["config", "gitdcy.device", "first-device"]);
        run(&second, ["config", "gitdcy.device", "second-device"]);

        fs::write(first.join(".env"), "APP_SECRET=local-only\n").unwrap();
        assert!(dirty_paths(&first)
            .unwrap()
            .iter()
            .all(|path| path.path != ".env"));

        let mut local_sync_files = BTreeMap::new();
        local_sync_files.insert("github/fixture".to_string(), vec![".env".to_string()]);
        let mut config = config_trusting("github/fixture", &["first-device"]);
        config.local_sync_files = Some(local_sync_files);
        let first_entry = entry("github/fixture", &first);
        let second_entry = entry("github/fixture", &second);

        assert!(sync_paths_with_config(&first_entry, &config)
            .unwrap()
            .iter()
            .any(|path| path.path == ".env" && path.kind == ChangeKind::Local));

        let first_report = sync_repo_with_config(&first_entry, &config);
        assert!(first_report.blocked.is_none(), "{first_report:?}");

        let second_report = sync_repo_with_config(&second_entry, &config);
        assert!(second_report.blocked.is_none(), "{second_report:?}");
        assert_eq!(
            fs::read_to_string(second.join(".env")).unwrap(),
            "APP_SECRET=local-only\n"
        );
        assert!(dirty_paths(&second)
            .unwrap()
            .iter()
            .all(|path| path.path != ".env"));
    }

    #[test]
    fn incoming_wip_blocks_on_same_dirty_file() {
        let fixture = GitFixture::new("dirty_wip_conflict");
        let first = fixture.clone_repo("first");
        let second = fixture.clone_repo("second");
        run(
            &first,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(
            &second,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(&first, ["config", "gitdcy.device", "first-device"]);
        run(&second, ["config", "gitdcy.device", "second-device"]);

        fs::write(first.join("README.md"), "changed on first\n").unwrap();
        let first_entry = entry("github/fixture", &first);
        let second_entry = entry("github/fixture", &second);
        let config = config_trusting("github/fixture", &["first-device"]);

        let first_report = sync_repo_with_config(&first_entry, &config);
        assert!(first_report.blocked.is_none(), "{first_report:?}");

        fs::write(second.join("README.md"), "changed on second\n").unwrap();
        let second_report = sync_repo_with_config(&second_entry, &config);
        assert!(
            second_report
                .blocked
                .as_deref()
                .is_some_and(|reason| reason.contains("README.md")),
            "{second_report:?}"
        );
        assert_eq!(
            fs::read_to_string(second.join("README.md")).unwrap(),
            "changed on second\n"
        );
    }

    #[test]
    fn incoming_wip_waits_for_device_trust() {
        let fixture = GitFixture::new("wip_device_trust");
        let first = fixture.clone_repo("first");
        let second = fixture.clone_repo("second");
        run(
            &first,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(
            &second,
            [
                "remote",
                "add",
                SYNC_REMOTE,
                fixture.remote.to_str().unwrap(),
            ],
        );
        run(&first, ["config", "gitdcy.device", "first-device"]);
        run(&second, ["config", "gitdcy.device", "second-device"]);

        let first_entry = entry("github/fixture", &first);
        let second_entry = entry("github/fixture", &second);
        fs::write(first.join("README.md"), "changed on first\n").unwrap();

        let first_report = sync_repo_with_config(&first_entry, &LocalConfig::default());
        assert!(first_report.blocked.is_none(), "{first_report:?}");

        let blocked_report = sync_repo_with_config(&second_entry, &LocalConfig::default());
        assert!(
            blocked_report
                .blocked
                .as_deref()
                .is_some_and(|reason| reason.contains("untrusted device first-device")),
            "{blocked_report:?}"
        );
        assert_eq!(
            fs::read_to_string(second.join("README.md")).unwrap(),
            "base\n"
        );

        let trusted_report =
            sync_repo_with_config(&second_entry, &config_trusting("*", &["first-device"]));
        assert!(trusted_report.blocked.is_none(), "{trusted_report:?}");
        assert_eq!(
            fs::read_to_string(second.join("README.md")).unwrap(),
            "changed on first\n"
        );
    }

    #[test]
    fn public_audit_blocks_agent_notes_and_private_env_files() {
        let fixture = GitFixture::new("public_audit_agents");
        let repo = fixture.clone_repo("repo");
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        fs::write(repo.join(".env.local"), "SECRET=value\n").unwrap();
        fs::write(repo.join(".env.example"), "SECRET=\n").unwrap();
        run(
            &repo,
            ["add", "-f", "AGENTS.md", ".env.local", ".env.example"],
        );

        let report =
            audit_repo_with_config(&entry("github/fixture", &repo), &LocalConfig::default())
                .unwrap();
        let paths: BTreeSet<_> = report
            .findings
            .iter()
            .filter_map(|finding| finding.path.as_deref())
            .collect();
        assert!(paths.contains("AGENTS.md"), "{report:?}");
        assert!(paths.contains(".env.local"), "{report:?}");
        assert!(!paths.contains(".env.example"), "{report:?}");
    }

    #[test]
    fn private_override_allows_agent_notes() {
        let fixture = GitFixture::new("private_override_agents");
        let repo = fixture.clone_repo("repo");
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        run(&repo, ["add", "-f", "AGENTS.md"]);

        let mut overrides = BTreeMap::new();
        overrides.insert("github/fixture".to_string(), VisibilityOverride::Private);
        let config = LocalConfig {
            visibility_overrides: Some(overrides),
            ..LocalConfig::default()
        };
        let report = audit_repo_with_config(&entry("github/fixture", &repo), &config).unwrap();
        assert!(
            report
                .findings
                .iter()
                .all(|finding| finding.path.as_deref() != Some("AGENTS.md")),
            "{report:?}"
        );
    }

    #[test]
    fn generated_cache_paths_are_blocked_even_for_private_repos() {
        let fixture = GitFixture::new("cache_block");
        let repo = fixture.clone_repo("repo");
        fs::create_dir_all(repo.join("node_modules/pkg")).unwrap();
        fs::write(repo.join("node_modules/pkg/index.js"), "generated\n").unwrap();
        run(&repo, ["add", "-f", "node_modules/pkg/index.js"]);

        let mut overrides = BTreeMap::new();
        overrides.insert("github/fixture".to_string(), VisibilityOverride::Private);
        let config = LocalConfig {
            visibility_overrides: Some(overrides),
            ..LocalConfig::default()
        };
        let report = audit_repo_with_config(&entry("github/fixture", &repo), &config).unwrap();
        assert!(
            report
                .findings
                .iter()
                .any(|finding| finding.path.as_deref() == Some("node_modules/pkg/index.js")),
            "{report:?}"
        );
    }

    #[test]
    fn deleting_blocked_paths_is_allowed_in_staged_audit() {
        let fixture = GitFixture::new("delete_blocked_path");
        let repo = fixture.clone_repo("repo");
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        run(&repo, ["add", "-f", "AGENTS.md"]);
        run(&repo, ["commit", "-m", "add agent notes"]);
        fs::remove_file(repo.join("AGENTS.md")).unwrap();
        run(&repo, ["add", "-A"]);

        let report =
            audit_staged_with_config(&entry("github/fixture", &repo), &LocalConfig::default())
                .unwrap();
        assert!(!report.has_fatal_findings(), "{report:?}");
    }

    #[test]
    fn public_remote_name_marks_repo_public_targeted() {
        let fixture = GitFixture::new("public_remote");
        let repo = fixture.clone_repo("repo");
        run(
            &repo,
            ["remote", "add", "public", fixture.remote.to_str().unwrap()],
        );
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        run(&repo, ["add", "-f", "AGENTS.md"]);

        let report =
            audit_repo_with_config(&entry("github/fixture", &repo), &LocalConfig::default())
                .unwrap();
        assert_eq!(report.visibility, RepoVisibility::Public);
        assert!(report.has_fatal_findings(), "{report:?}");
    }

    #[test]
    fn local_only_repo_is_private_by_default() {
        let fixture = GitFixture::new("local_only_private");
        let repo = fixture.clone_repo("repo");
        run(&repo, ["remote", "remove", "origin"]);
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        run(&repo, ["add", "-f", "AGENTS.md"]);

        let report =
            audit_repo_with_config(&entry("github/fixture", &repo), &LocalConfig::default())
                .unwrap();
        assert_eq!(report.visibility, RepoVisibility::Private);
        assert!(!report.has_fatal_findings(), "{report:?}");
    }

    #[test]
    fn forgejo_style_remote_is_private_by_default() {
        let fixture = GitFixture::new("forgejo_private");
        let repo = fixture.clone_repo("repo");
        run(
            &repo,
            [
                "remote",
                "set-url",
                "origin",
                "ssh://git@forgejo-easy/nerv/repo.git",
            ],
        );
        fs::write(repo.join("AGENTS.md"), "private agent routing\n").unwrap();
        run(&repo, ["add", "-f", "AGENTS.md"]);

        let report =
            audit_repo_with_config(&entry("forgejo/fixture", &repo), &LocalConfig::default())
                .unwrap();
        assert_eq!(report.visibility, RepoVisibility::Private);
        assert!(!report.has_fatal_findings(), "{report:?}");
    }

    #[test]
    fn policy_report_and_apply_repo_ignore_block_are_idempotent() {
        let fixture = GitFixture::new("policy_ignore");
        let repo = fixture.clone_repo("repo");
        let entry = entry("github/fixture", &repo);

        let report = policy_report_with_config(&entry, &LocalConfig::default()).unwrap();
        assert!(report.drift_count() > 0, "{report:?}");

        write_repo_ignore_block(&repo, &report.policy.ignore_rules).unwrap();
        let after = fs::read_to_string(repo.join(".gitignore")).unwrap();
        write_repo_ignore_block(&repo, &report.policy.ignore_rules).unwrap();
        let after_second = fs::read_to_string(repo.join(".gitignore")).unwrap();
        assert_eq!(after, after_second);

        let report = policy_report_with_config(&entry, &LocalConfig::default()).unwrap();
        assert_eq!(report.drift_count(), 0, "{report:?}");
    }

    #[test]
    fn managed_ignore_block_is_idempotent() {
        let existing = "# user rule\n*.tmp\n";
        let once = merge_managed_ignore_block(existing);
        let twice = merge_managed_ignore_block(&once);
        assert_eq!(once, twice);
        assert!(once.contains("AGENTS.md"));
        assert!(once.contains("node_modules/"));
        assert!(!once.contains("\ndata/\n"));
    }

    struct GitFixture {
        root: PathBuf,
        remote: PathBuf,
    }

    impl GitFixture {
        fn new(name: &str) -> Self {
            let unique = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let root = env::temp_dir().join(format!("gitdcy-{name}-{unique}"));
            fs::create_dir_all(&root).unwrap();
            let remote = root.join("remote.git");
            run_at(&root, ["git", "init", "--bare", remote.to_str().unwrap()]);

            let seed = root.join("seed");
            run_at(&root, ["git", "init", seed.to_str().unwrap()]);
            configure_user(&seed);
            fs::write(
                seed.join(".gitignore"),
                "node_modules/\ntarget/\ndist/\n.env\n",
            )
            .unwrap();
            fs::write(seed.join("README.md"), "base\n").unwrap();
            run(&seed, ["add", "."]);
            run(&seed, ["commit", "-m", "initial"]);
            run(&seed, ["branch", "-M", "main"]);
            run(&seed, ["remote", "add", "origin", remote.to_str().unwrap()]);
            run(&seed, ["push", "-u", "origin", "main"]);

            Self { root, remote }
        }

        fn clone_repo(&self, name: &str) -> PathBuf {
            let destination = self.root.join(name);
            run_at(
                &self.root,
                [
                    "git",
                    "clone",
                    "-b",
                    "main",
                    self.remote.to_str().unwrap(),
                    destination.to_str().unwrap(),
                ],
            );
            configure_user(&destination);
            destination
        }
    }

    fn entry(id: &str, path: &Path) -> RepoEntry {
        RepoEntry {
            id: id.to_string(),
            path: path.to_path_buf(),
            provider: Provider::Github,
            enabled: true,
            primary_remote: Some("origin".to_string()),
            wip_sync: true,
            review_required: false,
        }
    }

    fn sync_repo_with_config(entry: &RepoEntry, config: &LocalConfig) -> SyncReport {
        let mut report = SyncReport::new(entry.id.clone());
        if let Err(error) = sync_repo_inner_with_config(entry, &mut report, config) {
            report.block(error.to_string());
        }
        report
    }

    fn config_trusting(repo_id: &str, devices: &[&str]) -> LocalConfig {
        let mut trusted_wip_devices = BTreeMap::new();
        trusted_wip_devices.insert(
            repo_id.to_string(),
            devices.iter().map(|device| device.to_string()).collect(),
        );
        LocalConfig {
            trusted_wip_devices: Some(trusted_wip_devices),
            ..LocalConfig::default()
        }
    }

    fn configure_user(repo: &Path) {
        run(repo, ["config", "user.name", "GitDCY Test"]);
        run(repo, ["config", "user.email", "gitdcy@example.invalid"]);
    }

    fn run<const N: usize>(repo: &Path, args: [&str; N]) {
        run_command(Command::new("git").arg("-C").arg(repo).args(args));
    }

    fn run_at<const N: usize>(cwd: &Path, args: [&str; N]) {
        let mut command = Command::new(args[0]);
        command.current_dir(cwd).args(&args[1..]);
        run_command(&mut command);
    }

    fn run_command(command: &mut Command) {
        let output = command.output().unwrap();
        assert!(
            output.status.success(),
            "command failed: {:?}\nstdout:\n{}\nstderr:\n{}",
            command,
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
}
