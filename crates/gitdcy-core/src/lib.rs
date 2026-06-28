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
use std::time::{SystemTime, UNIX_EPOCH};

pub const APP_NAME: &str = "GitDCY";
pub const SYNC_REMOTE: &str = "sync";
const WIP_HEAD: &str = "refs/gitdcy/wip";
const WIP_REMOTE: &str = "refs/remotes/sync/wip";
const WIP_APPLIED: &str = "refs/gitdcy/applied";
const IGNORE_BLOCK_START: &str = "# BEGIN GITDCY PRIVATE DEFAULTS";
const IGNORE_BLOCK_END: &str = "# END GITDCY PRIVATE DEFAULTS";
const GLOBAL_IGNORE_BLOCK: &str = r#"# BEGIN GITDCY PRIVATE DEFAULTS
AGENTS.md
.env
.env.*
!.env.example
.codex/
.claude/
private/
docs/private/
state/
uploads/
logs/
*.log
*.sqlite
*.sqlite3
*.db
*.pem
*.key
id_rsa
id_ed25519
node_modules/
target/
.DS_Store
# END GITDCY PRIVATE DEFAULTS
"#;

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
        let lower = url.to_ascii_lowercase();
        if lower.contains("github.com") {
            Provider::Github
        } else if lower.contains("gitlab.com") {
            Provider::Gitlab
        } else if lower.contains("forgejo") {
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
        .unwrap_or_else(|| vec![default_workspace_root()])
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

    Ok(WorkspaceManifest {
        workspace_root: default_workspace_root(),
        repos: discover_entries(&default_scan_roots())?,
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

    for root in roots {
        for repo in discover_repo_paths(root)? {
            let canonical = repo.canonicalize().unwrap_or_else(|_| repo.clone());
            if !seen.insert(canonical) {
                continue;
            }

            let remotes = remotes(&repo).unwrap_or_default();
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

            repos.push(entry);
        }
    }

    repos.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(repos)
}

pub fn discover_repo_paths(root: &Path) -> Result<Vec<PathBuf>> {
    let mut found = Vec::new();
    if !root.exists() {
        return Ok(found);
    }

    fn visit(dir: &Path, found: &mut Vec<PathBuf>) -> Result<()> {
        if dir.join(".git").exists() {
            found.push(dir.to_path_buf());
            return Ok(());
        }

        let entries = match fs::read_dir(dir) {
            Ok(entries) => entries,
            Err(_) => return Ok(()),
        };

        for entry in entries {
            let entry = match entry {
                Ok(entry) => entry,
                Err(_) => continue,
            };
            let path = entry.path();
            if !path.is_dir() || should_skip_dir(&path) {
                continue;
            }
            visit(&path, found)?;
        }
        Ok(())
    }

    visit(root, &mut found)?;
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
        Err(error) => RepoStatus {
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
            last_error: Some(error.to_string()),
        },
    }
}

pub fn repo_status_result(entry: &RepoEntry) -> Result<RepoStatus> {
    let config = load_local_config();
    let branch = current_branch(&entry.path)?;
    let tracking_branch = tracking_branch(&entry.path).ok();
    let remotes = remotes(&entry.path)?;
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
        last_error: None,
    })
}

pub fn status_all(manifest: &WorkspaceManifest) -> Vec<RepoStatus> {
    manifest
        .repos
        .iter()
        .filter(|repo| repo.enabled)
        .map(repo_status)
        .collect()
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
    let branch = current_branch(&entry.path)?.unwrap_or_else(|| "HEAD".to_string());
    let before_dirty = sync_paths_with_config(entry, config)?;
    let remotes = remotes(&entry.path)?;

    let wip_remote = wip_remote_name(&remotes);

    if entry.wip_sync && wip_remote.is_some() && !before_dirty.is_empty() {
        let sha = create_wip_snapshot(&entry.path, &branch, &before_dirty)?;
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
                let sha = create_wip_snapshot(&entry.path, &branch, &combined_dirty)?;
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
    git(repo, ["commit", "-m", message])?;
    Ok(())
}

pub fn push(repo: &Path) -> Result<()> {
    let entry = entry_for_repo(repo)?;
    let report = audit_repo(&entry)?;
    if report.has_fatal_findings() {
        bail!("{}", format_audit_block(&entry, &report));
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
    let mut output = String::new();
    let mut in_managed_block = false;
    for line in existing.lines() {
        if line.trim() == IGNORE_BLOCK_START {
            in_managed_block = true;
            continue;
        }
        if line.trim() == IGNORE_BLOCK_END {
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
    output.push_str(GLOBAL_IGNORE_BLOCK);
    output
}

pub fn set_remote(repo: &Path, name: &str, url: &str) -> Result<()> {
    if name.trim().is_empty() || url.trim().is_empty() {
        bail!("remote name and URL are required");
    }
    if remotes(repo)?.contains_key(name) {
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

    if remotes.keys().any(|name| name == "public") {
        return RepoVisibility::Public;
    }

    let mut saw_unknown = remotes.is_empty();
    for url in remotes.values() {
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
    let output =
        git_command_with_paths(repo, ["ls-files", "--error-unmatch"], &[path.to_string()]).output();
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

fn create_wip_snapshot(repo: &Path, branch: &str, dirty: &[ChangedPath]) -> Result<String> {
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
    let sha = git_output(repo, ["commit-tree", &tree, "-p", &parent, "-m", &message])?
        .trim()
        .to_string();
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
    let output = git_command_with_paths(
        repo,
        ["check-ignore", "--no-index", "-q"],
        &[path.to_string()],
    )
    .output();
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
    let output = git_command(repo, args).output().context("run git")?;
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
    let output = git_command(repo, args).output().context("run git")?;
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
    let output = git_command(repo, args).output().context("run git")?;
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
    let output = git_command(repo, args)
        .envs(envs)
        .output()
        .context("run git")?;
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
    let output = git_command(repo, args)
        .envs(envs)
        .output()
        .context("run git")?;
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
    let output = git_command_with_paths(repo, args, paths)
        .output()
        .context("run git")?;
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
    let output = git_command_with_paths(repo, args, paths)
        .envs(envs)
        .output()
        .context("run git")?;
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
            Command::new("hostname")
                .output()
                .ok()
                .and_then(|output| String::from_utf8(output.stdout).ok())
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
    let Ok(text) = fs::read_to_string(path) else {
        return LocalConfig::default();
    };
    serde_norway::from_str::<LocalConfig>(&text).unwrap_or_default()
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
    base
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
            Provider::from_url("git@gitlab.com:a/b.git"),
            Provider::Gitlab
        );
        assert_eq!(
            Provider::from_url("ssh://git@forgejo.example/a/b.git"),
            Provider::Forgejo
        );
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
