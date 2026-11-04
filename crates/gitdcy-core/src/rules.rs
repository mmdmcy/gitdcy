//! Repository rules: where a repository may be pushed, which branches take
//! only a pull request, and the personal-data check on what a push sends.
//!
//! Rules are declared once per repository, in the owner's rules file (TOML),
//! and found by the repository's identity: its first (root) commit, with its
//! remote URLs as a hint. They do not depend on the folder a checkout is in,
//! so they hold wherever the repository goes. `install` writes a pre-push hook
//! and a copy of that one repository's rule into its git folder, so the rule
//! also holds on a machine without the rules file.
//!
//! ```toml
//! # Hosts that count as self-hosted (names or addresses; an SSH alias that
//! # resolves to one of them counts too). Local paths and localhost always do.
//! selfhosted = ["forge.example.com", "192.0.2.10"]
//!
//! # The personal-data check: words that never leave in a pushed commit, and
//! # private addresses that do belong in commits.
//! personal = ["Jane Doe"]
//! allow = []
//!
//! # Reminders about work left lying around (scan and the push check); a
//! # repository whose remote starts with a `skip` prefix keeps its own rules.
//! [hygiene]
//! branch_days = 3
//! skip = ["github.com/example-employer/"]
//!
//! [[repo]]
//! name = "example-app"
//! first_commit = "0123456789abcdef0123456789abcdef01234567"
//! remotes = ["ssh://git@forge.example.com:2222/team/example-app.git"]
//! push = "selfhosted"     # or "nowhere", "anywhere", or ["github.com/example-org/"]
//! protected = ["main"]    # only through a pull request
//! note = "Customer data: it stays on our own forge."
//! ```

use super::hygiene::{self, GitHub, HygieneRules, Reminder};
use super::personal::{self, Finding, ScanConfig};
use super::{config_dir, expand_home, git_output, load_saved_local_config};
use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{SystemTime, UNIX_EPOCH};

/// The rules file's name in GitDCY's config folder, when `local.yaml` names no other.
pub const RULES_FILE_NAME: &str = "rules.toml";
/// One repository's rule, copied into its git folder by `install`.
pub const RULES_COPY_NAME: &str = "gitdcy-rules.toml";
/// The line that makes a pre-push hook gitdcy's own.
const HOOK_MARK: &str = "# gitdcy rules check: before every push";
const LOCAL_HOSTS: &[&str] = &["localhost", "127.0.0.1", "::1"];
const QUESTION: &str =
    "This refusal is a question for the repository's owner: ask them before trying another way.";

/// Where a repository may be pushed.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "PushText", into = "PushText")]
pub enum Push {
    #[default]
    Anywhere,
    /// Only remotes on hosts listed under `selfhosted`, localhost or a local path.
    Selfhosted,
    /// No push at all.
    Nowhere,
    /// Only remote URLs that start with one of these (`github.com/example-org/`).
    Remotes(Vec<String>),
}

impl Push {
    fn is_anywhere(&self) -> bool {
        *self == Push::Anywhere
    }

    pub fn describe(&self, selfhosted: &[String]) -> String {
        match self {
            Push::Anywhere => "anywhere".into(),
            Push::Nowhere => "nowhere".into(),
            Push::Selfhosted if selfhosted.is_empty() => {
                "self-hosted only (no hosts listed: local paths only)".into()
            }
            Push::Selfhosted => format!("self-hosted hosts only: {}", selfhosted.join(", ")),
            Push::Remotes(list) => format!("only {}", list.join(", ")),
        }
    }
}

#[derive(Serialize, Deserialize)]
#[serde(untagged)]
enum PushText {
    Word(String),
    Remotes(Vec<String>),
}

impl TryFrom<PushText> for Push {
    type Error = String;

    fn try_from(text: PushText) -> Result<Self, Self::Error> {
        match text {
            PushText::Word(word) => match word.trim() {
                "anywhere" => Ok(Push::Anywhere),
                "selfhosted" => Ok(Push::Selfhosted),
                "nowhere" => Ok(Push::Nowhere),
                other => Err(format!(
                    "push = \"{other}\" is not a rule: use \"selfhosted\", \"nowhere\", \"anywhere\" or a list of allowed remote URL prefixes"
                )),
            },
            PushText::Remotes(list) => {
                if list.is_empty() || list.iter().any(|p| p.trim().is_empty()) {
                    Err("push = [] allows nothing; say push = \"nowhere\" for that".into())
                } else {
                    Ok(Push::Remotes(list))
                }
            }
        }
    }
}

impl From<Push> for PushText {
    fn from(push: Push) -> Self {
        match push {
            Push::Anywhere => PushText::Word("anywhere".into()),
            Push::Selfhosted => PushText::Word("selfhosted".into()),
            Push::Nowhere => PushText::Word("nowhere".into()),
            Push::Remotes(list) => PushText::Remotes(list),
        }
    }
}

/// One repository's rule.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RepoRule {
    /// A name for people; matching does not use it.
    pub name: String,
    /// The repository's root commit: its identity.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub first_commit: Option<String>,
    /// Remote URLs the repository is known by: a hint when the first commit is
    /// not known, or the history was rewritten.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub remotes: Vec<String>,
    #[serde(default, skip_serializing_if = "Push::is_anywhere")]
    pub push: Push,
    /// Branches that take no direct push: they are reached only through a pull request.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub protected: Vec<String>,
    /// The owner's reason, shown with a refusal.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
}

/// The owner's rules file.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RulesFile {
    /// Hosts that count as self-hosted.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub selfhosted: Vec<String>,
    /// Words that never go in a pushed commit (names, phone numbers).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub personal: Vec<String>,
    /// Private mail addresses that do belong in commits.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub allow: Vec<String>,
    /// Reminders about work left lying around (`gitdcy rules scan`, the push
    /// check): on when this table is there.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hygiene: Option<HygieneRules>,
    #[serde(default, rename = "repo", skip_serializing_if = "Vec::is_empty")]
    pub repos: Vec<RepoRule>,
}

impl RulesFile {
    fn validate(&self) -> Result<()> {
        if let Some(hygiene) = &self.hygiene {
            hygiene.validate()?;
        }
        let mut names = BTreeSet::new();
        for rule in &self.repos {
            let name = rule.name.trim();
            if name.is_empty() {
                bail!("a [[repo]] has no name");
            }
            if !names.insert(name.to_string()) {
                bail!("two rules are named {name}; each repository has one rule");
            }
            if let Some(first) = &rule.first_commit {
                let hex = first.chars().all(|c| c.is_ascii_hexdigit());
                if !hex || !(first.len() == 40 || first.len() == 64) {
                    bail!("rule {name}: first_commit = \"{first}\" is not a full commit hash (git rev-list --max-parents=0 HEAD shows it)");
                }
            }
            if rule.first_commit.is_none() && rule.remotes.is_empty() {
                bail!(
                    "rule {name} has neither first_commit nor remotes, so it matches no repository"
                );
            }
            if rule.protected.iter().any(|b| b.trim().is_empty()) {
                bail!("rule {name}: an empty name in protected");
            }
        }
        Ok(())
    }
}

/// Reads a rules file; `None` when it is not there.
pub fn read_rules(path: &Path) -> Result<Option<RulesFile>> {
    let text = match fs::read_to_string(path) {
        Ok(text) => text,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(error).with_context(|| format!("reading the rules file {}", path.display()))
        }
    };
    let file: RulesFile = toml::from_str(&text)
        .with_context(|| format!("the rules file {} is not valid", path.display()))?;
    file.validate()
        .with_context(|| format!("the rules file {} is not valid", path.display()))?;
    Ok(Some(file))
}

/// The rules file: the one given, else `rules_file` in this device's own
/// `local.yaml`, else `rules.toml` in GitDCY's config folder. The setting is
/// read from the device's config only, never from a file in the current
/// folder: a repository could carry one.
pub fn rules_file_path(explicit: Option<&Path>) -> Result<PathBuf> {
    if let Some(path) = explicit {
        return Ok(expand_home(path.to_path_buf()));
    }
    let config = load_saved_local_config();
    if let Some(error) = config.config_error {
        bail!("{error}");
    }
    if let Some(path) = config.rules_file {
        return Ok(expand_home(path));
    }
    Ok(config_dir()?.join(RULES_FILE_NAME))
}

// ---------------------------------------------------------------------------
// Remote URLs

/// git's own rule: `host:path` with no `/` before the `:` is an SSH address;
/// a Windows drive (`C:\...`) is not.
fn is_scp_like(url: &str) -> bool {
    if url.contains("://") {
        return false;
    }
    match url.find(':') {
        Some(colon) => {
            let before = &url[..colon];
            !before.contains('/')
                && !before.contains('\\')
                && !(before.len() == 1 && before.chars().all(|c| c.is_ascii_alphabetic()))
        }
        None => false,
    }
}

/// A path on this device (`/srv/git/x.git`, `../x`, `file:///srv/x`).
fn is_local(url: &str) -> bool {
    let url = url.trim();
    url.starts_with("file://") || (!url.contains("://") && !is_scp_like(url))
}

fn strip_port(host: &str) -> &str {
    if let Some(rest) = host.strip_prefix('[') {
        return rest.split(']').next().unwrap_or(rest);
    }
    match host.rsplit_once(':') {
        Some((name, port)) if port.chars().all(|c| c.is_ascii_digit()) => name,
        _ => host,
    }
}

/// `git@host:org/repo.git`, `ssh://git@host:2222/org/repo`,
/// `https://user:secret@host/org/repo` -> `host/org/repo`: lowercase, without
/// user, password, port or `.git`. A local path stays the path it is. What a
/// remote is shown as, so a URL with a token in it is never printed whole.
pub fn normalize_url(url: &str) -> String {
    let url = url.trim();
    if is_local(url) {
        let path = url.strip_prefix("file://").unwrap_or(url);
        return path.trim_end_matches('/').to_string();
    }
    let joined = if let Some((_, rest)) = url.split_once("://") {
        let (authority, path) = rest.split_once('/').unwrap_or((rest, ""));
        let host = authority.rsplit_once('@').map_or(authority, |(_, h)| h);
        format!("{}/{path}", strip_port(host))
    } else if is_scp_like(url) {
        let (authority, path) = url.split_once(':').unwrap_or((url, ""));
        let host = authority.rsplit_once('@').map_or(authority, |(_, h)| h);
        format!("{host}/{}", path.trim_start_matches('/'))
    } else {
        url.to_string()
    };
    let lower = joined.to_lowercase();
    let trimmed = lower.trim_end_matches('/');
    trimmed
        .strip_suffix(".git")
        .unwrap_or(trimmed)
        .trim_end_matches('/')
        .to_string()
}

/// The host of a remote URL, lowercase, without user and port; `None` for a local path.
fn host_of(url: &str) -> Option<String> {
    if is_local(url) {
        return None;
    }
    let normalized = normalize_url(url);
    let host = normalized.split('/').next().unwrap_or("");
    (!host.is_empty()).then(|| host.to_string())
}

/// `prefix` matches the URL itself or what lies under it (`github.com/org`
/// matches `github.com/org/repo`, not `github.com/org-other/repo`).
fn url_has_prefix(url: &str, prefix: &str) -> bool {
    let (url, prefix) = (normalize_url(url), normalize_url(prefix));
    !prefix.is_empty()
        && (url == prefix || (url.starts_with(&prefix) && url[prefix.len()..].starts_with('/')))
}

/// What an SSH alias stands for, from the user's own ssh config (`ssh -G`).
fn ssh_hostname(alias: &str) -> Option<String> {
    let safe = !alias.is_empty()
        && !alias.starts_with('-')
        && alias
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || ".-_".contains(c));
    if !safe {
        return None;
    }
    let output = Command::new("ssh").args(["-G", alias]).output().ok()?;
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .find_map(|line| {
            line.strip_prefix("hostname ")
                .map(|h| h.trim().to_lowercase())
        })
}

/// Whether `push` lets a push go to `url`. `resolve_alias` turns an SSH alias
/// into the host it stands for.
pub fn remote_allowed(
    push: &Push,
    url: &str,
    selfhosted: &[String],
    resolve_alias: impl Fn(&str) -> Option<String>,
) -> bool {
    match push {
        Push::Anywhere => true,
        Push::Nowhere => false,
        Push::Remotes(list) => list.iter().any(|prefix| url_has_prefix(url, prefix)),
        Push::Selfhosted => {
            let Some(host) = host_of(url) else {
                return true; // a path on this device
            };
            let listed: BTreeSet<String> = selfhosted
                .iter()
                .map(|h| host_of(h).unwrap_or_else(|| h.trim().to_lowercase()))
                .chain(LOCAL_HOSTS.iter().map(|h| h.to_string()))
                .collect();
            if listed.contains(&host) {
                return true;
            }
            let lower = url.trim().to_lowercase();
            let ssh = !lower.starts_with("http://")
                && !lower.starts_with("https://")
                && !lower.starts_with("git://");
            ssh && resolve_alias(&host).is_some_and(|real| listed.contains(&real))
        }
    }
}

/// The protected branches among `branches`.
pub fn protected_in<'a>(rule: &RepoRule, branches: impl Iterator<Item = &'a str>) -> Vec<String> {
    let mut out: Vec<String> = branches
        .filter(|b| rule.protected.iter().any(|p| p.trim() == *b))
        .map(str::to_string)
        .collect();
    out.sort();
    out.dedup();
    out
}

// ---------------------------------------------------------------------------
// The repository

struct Repo {
    root: PathBuf,
    /// The git folder shared by all worktrees: where hooks and the copy live.
    git_dir: PathBuf,
    first_commits: Vec<String>,
    /// Remote name and its URLs (fetch and push), as configured.
    remotes: Vec<(String, Vec<String>)>,
}

fn absolute(base: &Path, path: &str) -> PathBuf {
    let path = PathBuf::from(path.trim());
    if path.is_absolute() {
        path
    } else {
        base.join(path)
    }
}

fn open(path: &Path) -> Result<Repo> {
    let dir = if path.is_dir() {
        path.to_path_buf()
    } else {
        path.parent().map(Path::to_path_buf).unwrap_or_default()
    };
    let git_dir = git_output(
        &dir,
        ["rev-parse", "--path-format=absolute", "--git-common-dir"],
    )
    .or_else(|_| git_output(&dir, ["rev-parse", "--absolute-git-dir"]))
    .map(|out| absolute(&dir, &out))
    .with_context(|| format!("{} is not inside a git repository", path.display()))?;
    // A bare repository (a mirror) has no work tree; its git folder is the repository.
    let root = match git_output(&dir, ["rev-parse", "--show-toplevel"]) {
        Ok(out) if !out.trim().is_empty() => PathBuf::from(out.trim()),
        _ => git_dir.clone(),
    };
    let mut first_commits: Vec<String> =
        git_output(&root, ["rev-list", "--max-parents=0", "--all"])
            .unwrap_or_default()
            .lines()
            .map(|l| l.trim().to_lowercase())
            .filter(|l| !l.is_empty())
            .collect();
    first_commits.sort();
    first_commits.dedup();
    let mut remotes = Vec::new();
    for name in git_output(&root, ["remote"]).unwrap_or_default().lines() {
        let name = name.trim();
        if name.is_empty() {
            continue;
        }
        let mut urls = Vec::new();
        for args in [
            vec!["remote", "get-url", "--push", "--all", name],
            vec!["remote", "get-url", "--all", name],
        ] {
            if let Ok(out) = git_output(&root, &args) {
                for url in out.lines().map(str::trim).filter(|u| !u.is_empty()) {
                    if !urls.iter().any(|u: &String| u == url) {
                        urls.push(url.to_string());
                    }
                }
            }
        }
        remotes.push((name.to_string(), urls));
    }
    Ok(Repo {
        root,
        git_dir,
        first_commits,
        remotes,
    })
}

fn push_url(repo: &Repo, remote: &str) -> Option<String> {
    git_output(&repo.root, ["remote", "get-url", "--push", remote])
        .ok()
        .and_then(|out| out.lines().next().map(|l| l.trim().to_string()))
        .filter(|u| !u.is_empty())
}

fn short(sha: &str) -> &str {
    &sha[..sha.len().min(12)]
}

/// Why `rule` is this repository's rule; empty when it is not.
fn matches(rule: &RepoRule, repo: &Repo) -> Vec<String> {
    let mut why = Vec::new();
    if let Some(first) = &rule.first_commit {
        if repo
            .first_commits
            .iter()
            .any(|c| c.eq_ignore_ascii_case(first))
        {
            why.push(format!("first commit {}", short(first)));
        }
    }
    for hint in &rule.remotes {
        let hint = normalize_url(hint);
        for (name, urls) in &repo.remotes {
            if urls.iter().any(|u| normalize_url(u) == hint) {
                why.push(format!("remote {name} ({hint})"));
            }
        }
    }
    why
}

// ---------------------------------------------------------------------------
// Which rule applies

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum RuleSource {
    RulesFile,
    InstalledCopy,
}

struct Found {
    repo: Repo,
    rules_path: PathBuf,
    file: Option<RulesFile>,
    copy_path: PathBuf,
    copy: Option<RulesFile>,
    rule: Option<RepoRule>,
    source: Option<RuleSource>,
    matched_by: Vec<String>,
    /// Two or more rules in the rules file claim this repository.
    conflicts: Vec<String>,
    selfhosted: Vec<String>,
}

impl Found {
    fn source_text(&self) -> String {
        match self.source {
            Some(RuleSource::InstalledCopy) => format!(
                "{} (a copy of the owner's rules file)",
                self.copy_path.display()
            ),
            _ => format!("the rules file {}", self.rules_path.display()),
        }
    }

    fn scan_config(&self) -> ScanConfig {
        self.file
            .as_ref()
            .map(|f| ScanConfig {
                words: f.personal.clone(),
                allow: f.allow.clone(),
            })
            .unwrap_or_default()
    }

    /// What `install` writes as the copy, when there is a rule.
    fn expected_copy(&self) -> Option<RulesFile> {
        self.rule.as_ref().map(|rule| RulesFile {
            selfhosted: self.selfhosted.clone(),
            repos: vec![rule.clone()],
            ..RulesFile::default()
        })
    }
}

fn find(rules_path: &Path, repo_path: &Path) -> Result<Found> {
    let repo = open(repo_path)?;
    let file = read_rules(rules_path)?;
    let copy_path = repo.git_dir.join(RULES_COPY_NAME);
    let copy = read_rules(&copy_path)?;
    if let Some(copy) = &copy {
        if copy.repos.len() != 1 {
            bail!(
                "{} holds {} rules; it is one repository's copy and holds one (gitdcy rules install writes it)",
                copy_path.display(),
                copy.repos.len()
            );
        }
    }
    let matching: Vec<(&RepoRule, Vec<String>)> = file
        .iter()
        .flat_map(|f| f.repos.iter())
        .filter_map(|rule| {
            let why = matches(rule, &repo);
            (!why.is_empty()).then_some((rule, why))
        })
        .collect();
    let mut found = Found {
        repo,
        rules_path: rules_path.to_path_buf(),
        file: None,
        copy_path,
        copy: None,
        rule: None,
        source: None,
        matched_by: Vec::new(),
        conflicts: Vec::new(),
        selfhosted: Vec::new(),
    };
    if matching.len() > 1 {
        found.conflicts = matching.iter().map(|(r, _)| r.name.clone()).collect();
        found.matched_by = matching
            .iter()
            .map(|(r, why)| format!("{}: {}", r.name, why.join(", ")))
            .collect();
    } else if let Some((rule, why)) = matching.first() {
        found.rule = Some((*rule).clone());
        found.source = Some(RuleSource::RulesFile);
        found.matched_by = why.clone();
        found.selfhosted = file
            .as_ref()
            .map(|f| f.selfhosted.clone())
            .unwrap_or_default();
    } else if let Some(copy) = &copy {
        found.rule = copy.repos.first().cloned();
        found.source = Some(RuleSource::InstalledCopy);
        found.matched_by = vec!["the copy in this repository's git folder".into()];
        found.selfhosted = copy.selfhosted.clone();
    }
    found.file = file;
    found.copy = copy;
    Ok(found)
}

// ---------------------------------------------------------------------------
// The hook

fn sh_quote(text: &str) -> String {
    format!("'{}'", text.replace('\'', r"'\''"))
}

/// The pre-push hook: the check, then the hook that was there before, if any.
fn hook_text(chained: Option<&str>, gitdcy: Option<&Path>) -> String {
    let fallback = gitdcy
        .map(|path| {
            format!(
                "command -v gitdcy >/dev/null 2>&1 || gitdcy={}\n",
                sh_quote(&path.display().to_string())
            )
        })
        .unwrap_or_default();
    format!(
        r#"#!/bin/sh
{HOOK_MARK}, this repository's rule (the owner's
# rules file, or the copy gitdcy-rules.toml in the git folder) and the
# personal-data check. Written by gitdcy rules install; run that again to update.
gitdcy=gitdcy
{fallback}chained={chained}
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
"#,
        chained = sh_quote(chained.unwrap_or(""))
    )
}

/// The hook a gitdcy hook chains, as written in it.
fn chained_of(text: &str) -> Option<String> {
    text.lines()
        .find_map(|line| line.strip_prefix("chained='"))
        .and_then(|rest| rest.strip_suffix('\''))
        .filter(|name| !name.is_empty())
        .map(str::to_string)
}

#[derive(Debug, Clone, Serialize)]
pub struct HookState {
    /// `.git/hooks/pre-push`.
    pub path: String,
    /// It is gitdcy's hook.
    pub installed: bool,
    /// It is the hook this gitdcy writes.
    pub current: bool,
    /// git runs it on a push: no `core.hooksPath` points elsewhere, or the hook
    /// there calls this one.
    pub runs: bool,
    /// Where git runs hooks from, when `core.hooksPath` moves them.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hooks_path: Option<String>,
}

fn same_dir(a: &Path, b: &Path) -> bool {
    let a = a.canonicalize().unwrap_or_else(|_| a.to_path_buf());
    let b = b.canonicalize().unwrap_or_else(|_| b.to_path_buf());
    a == b
}

/// The other folder git runs hooks from (`core.hooksPath`), if any, and
/// whether its pre-push calls the repository's own.
fn hooks_elsewhere(repo: &Repo) -> Option<(PathBuf, bool)> {
    let default = repo.git_dir.join("hooks");
    let effective = git_output(
        &repo.root,
        ["rev-parse", "--path-format=absolute", "--git-path", "hooks"],
    )
    .map(|out| absolute(&repo.root, &out))
    .unwrap_or_else(|_| default.clone());
    if same_dir(&effective, &default) {
        return None;
    }
    let chains = fs::read_to_string(effective.join("pre-push"))
        .is_ok_and(|text| text.contains("/hooks/pre-push"));
    Some((effective, chains))
}

fn hook_state(repo: &Repo, gitdcy: Option<&Path>) -> HookState {
    let path = repo.git_dir.join("hooks").join("pre-push");
    let text = fs::read_to_string(&path).unwrap_or_default();
    let installed = text.contains(HOOK_MARK);
    let elsewhere = hooks_elsewhere(repo);
    HookState {
        path: path.display().to_string(),
        installed,
        current: installed && text == hook_text(chained_of(&text).as_deref(), gitdcy),
        runs: elsewhere.as_ref().is_none_or(|(_, chains)| *chains),
        hooks_path: elsewhere.map(|(dir, _)| dir.display().to_string()),
    }
}

// ---------------------------------------------------------------------------
// `gitdcy rules`: which rule applies and why

#[derive(Debug, Clone, Serialize)]
pub struct RemoteState {
    pub name: String,
    /// Shown without user, password or port.
    pub url: Option<String>,
    /// The rule lets a push go there.
    pub allowed: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct CopyState {
    pub path: String,
    pub present: bool,
    /// The copy holds what the rules file says now; `None` without a rule.
    pub current: Option<bool>,
}

#[derive(Debug, Clone, Serialize)]
pub struct RulesReport {
    pub repository: String,
    pub rules_file: String,
    pub rules_file_found: bool,
    pub first_commits: Vec<String>,
    pub rule: Option<RepoRule>,
    pub source: Option<RuleSource>,
    pub matched_by: Vec<String>,
    /// Rules that all match this repository: its pushes are refused until one is left.
    pub conflicts: Vec<String>,
    /// Where the rule lets it go, in words.
    pub push: Option<String>,
    pub selfhosted: Vec<String>,
    pub remotes: Vec<RemoteState>,
    pub copy: CopyState,
    pub hook: HookState,
    /// How many personal words and allowed addresses the check reads (the words themselves are not shown).
    pub personal_words: usize,
    pub allowed_addresses: usize,
    pub warnings: Vec<String>,
}

pub fn rules_report(
    rules_path: &Path,
    repo_path: &Path,
    gitdcy: Option<&Path>,
) -> Result<RulesReport> {
    let found = find(rules_path, repo_path)?;
    let hook = hook_state(&found.repo, gitdcy);
    let scan = found.scan_config();
    let copy_current = found
        .expected_copy()
        .map(|expected| found.copy.as_ref() == Some(&expected));
    let mut warnings = Vec::new();
    if found.file.is_none() {
        warnings.push(match found.source {
            Some(RuleSource::InstalledCopy) => format!(
                "the rules file {} is not on this device; the copy in the git folder holds",
                found.rules_path.display()
            ),
            _ => format!(
                "no rules file at {} (rules_file in GitDCY's local.yaml names another place)",
                found.rules_path.display()
            ),
        });
    } else if found.source == Some(RuleSource::InstalledCopy) {
        warnings.push(format!(
            "the rules file {} has no rule for this repository; the copy in the git folder still holds",
            found.rules_path.display()
        ));
    }
    if !found.conflicts.is_empty() {
        warnings.push(format!(
            "rules {} all match this repository; keep one in the rules file",
            found.conflicts.join(", ")
        ));
    }
    if let Some(rule) = &found.rule {
        if let Some(first) = &rule.first_commit {
            if found.source == Some(RuleSource::RulesFile)
                && !found
                    .repo
                    .first_commits
                    .iter()
                    .any(|c| c.eq_ignore_ascii_case(first))
            {
                warnings.push(format!(
                    "{} matched by its remote only: its first_commit {} is not in this history (rewritten, or a shallow clone?)",
                    rule.name,
                    short(first)
                ));
            }
        }
        if rule.push == Push::Selfhosted && found.selfhosted.is_empty() {
            warnings.push(
                "push = \"selfhosted\" and no self-hosted hosts are listed: only local paths are allowed".into(),
            );
        }
        if copy_current != Some(true) {
            warnings.push(if found.copy.is_none() {
                "the rule is not installed in this checkout: gitdcy rules install".to_string()
            } else {
                "the copy in the git folder differs from the rules file: gitdcy rules install"
                    .to_string()
            });
        }
        if !hook.installed {
            warnings.push("no gitdcy pre-push hook here: gitdcy rules install".into());
        } else if !hook.current {
            warnings.push("the pre-push hook is an older one: gitdcy rules install".into());
        }
        if !hook.runs {
            warnings.push(format!(
                "git runs hooks from {} (core.hooksPath), and the pre-push there does not call this repository's own, so the rule is not checked on a push",
                hook.hooks_path.as_deref().unwrap_or("")
            ));
        }
    }
    let remotes = found
        .repo
        .remotes
        .iter()
        .map(|(name, _)| {
            let url = push_url(&found.repo, name);
            let allowed = match (&found.rule, &url) {
                (Some(rule), Some(url)) => {
                    remote_allowed(&rule.push, url, &found.selfhosted, ssh_hostname)
                }
                _ => found.conflicts.is_empty(),
            };
            RemoteState {
                name: name.clone(),
                url: url.as_deref().map(normalize_url),
                allowed,
            }
        })
        .collect();
    Ok(RulesReport {
        repository: found.repo.root.display().to_string(),
        rules_file: found.rules_path.display().to_string(),
        rules_file_found: found.file.is_some(),
        first_commits: found.repo.first_commits.clone(),
        push: found
            .rule
            .as_ref()
            .map(|r| r.push.describe(&found.selfhosted)),
        rule: found.rule.clone(),
        source: found.source,
        matched_by: found.matched_by.clone(),
        conflicts: found.conflicts.clone(),
        selfhosted: found.selfhosted.clone(),
        remotes,
        copy: CopyState {
            path: found.copy_path.display().to_string(),
            present: found.copy.is_some(),
            current: copy_current,
        },
        hook,
        personal_words: scan.words.len(),
        allowed_addresses: scan.allow.len(),
        warnings,
    })
}

// ---------------------------------------------------------------------------
// `gitdcy rules install`

#[derive(Debug, Clone, Serialize)]
pub struct InstallReport {
    pub repository: String,
    pub rule: String,
    pub copy: String,
    pub copy_written: bool,
    pub hook: String,
    pub hook_written: bool,
    /// Files moved aside before they were replaced.
    pub kept: Vec<String>,
    /// The hook that was there before; it now runs after the check.
    pub chained: Option<String>,
}

/// `YYYY-MM-DD` in UTC.
fn today() -> String {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    civil_date((secs / 86_400) as i64)
}

/// Days since 1970-01-01 -> `YYYY-MM-DD` (the proleptic Gregorian calendar).
fn civil_date(days: i64) -> String {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1_460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!("{year:04}-{month:02}-{day:02}")
}

/// Moves `path` aside as `NAME.bak-YYYY-MM-DD` (`-2`, `-3`, ... when taken).
fn keep_aside(path: &Path) -> Result<PathBuf> {
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let stamp = today();
    let mut target = path.with_file_name(format!("{name}.bak-{stamp}"));
    let mut next = 2;
    while target.symlink_metadata().is_ok() {
        target = path.with_file_name(format!("{name}.bak-{stamp}-{next}"));
        next += 1;
    }
    fs::rename(path, &target)
        .with_context(|| format!("moving {} aside to {}", path.display(), target.display()))?;
    Ok(target)
}

fn copy_text(copy: &RulesFile, rules_path: &Path) -> Result<String> {
    Ok(format!(
        "# This repository's rule, copied from {} by gitdcy rules install.\n\
         # The rule is the owner's: change it in the rules file, then run\n\
         # gitdcy rules install again.\n\n{}",
        rules_path.display(),
        toml::to_string_pretty(copy)?
    ))
}

/// Writes the copy of this repository's rule and the pre-push hook into its
/// git folder. A hook that is there already is moved aside and chained; when
/// git would not run the hook (a `core.hooksPath` elsewhere), nothing is
/// written. Safe to run twice.
pub fn install(
    rules_path: &Path,
    repo_path: &Path,
    gitdcy: Option<&Path>,
) -> Result<InstallReport> {
    let found = find(rules_path, repo_path)?;
    if !found.conflicts.is_empty() {
        bail!(
            "rules {} in {} all match this repository ({}); keep one for it, then run gitdcy rules install again",
            found.conflicts.join(", "),
            found.rules_path.display(),
            found.matched_by.join("; ")
        );
    }
    let (Some(rule), Some(expected)) = (found.rule.clone(), found.expected_copy()) else {
        let first = found
            .repo
            .first_commits
            .first()
            .map(String::as_str)
            .unwrap_or("(no commits yet)");
        bail!(
            "{} has no rule in {}, so there is nothing to install. To give it one, add to that file:\n\n[[repo]]\nname = \"NAME\"\nfirst_commit = \"{first}\"\npush = \"selfhosted\"   # or \"nowhere\", or [\"host/org/\"]\n\nthen run gitdcy rules install again",
            found.repo.root.display(),
            found.rules_path.display()
        );
    };
    if let Some((dir, chains)) = hooks_elsewhere(&found.repo) {
        if !chains {
            bail!(
                "git runs this repository's hooks from {} (core.hooksPath), so a hook in {} would not run, and nothing was written. Make {}/pre-push call .git/hooks/pre-push with the same arguments and input, or remove core.hooksPath from the git config that sets it (git config --show-origin core.hooksPath shows which); then run gitdcy rules install again",
                dir.display(),
                found.repo.git_dir.join("hooks").display(),
                dir.display()
            );
        }
    }
    let hooks = found.repo.git_dir.join("hooks");
    let hook = hooks.join("pre-push");
    if hook.is_dir() {
        bail!(
            "{} is a folder, not a hook; nothing was written",
            hook.display()
        );
    }
    let mut kept = Vec::new();

    let copy_written = found.copy.as_ref() != Some(&expected);
    if copy_written {
        if found.copy_path.symlink_metadata().is_ok() {
            kept.push(keep_aside(&found.copy_path)?.display().to_string());
        }
        fs::write(&found.copy_path, copy_text(&expected, &found.rules_path)?)
            .with_context(|| format!("writing {}", found.copy_path.display()))?;
    }

    fs::create_dir_all(&hooks).with_context(|| format!("creating {}", hooks.display()))?;
    let existing = hook.symlink_metadata().is_ok().then(|| {
        fs::read(&hook)
            .map(|bytes| String::from_utf8_lossy(&bytes).into_owned())
            .unwrap_or_default()
    });
    let mut chained = existing
        .as_deref()
        .filter(|text| text.contains(HOOK_MARK))
        .and_then(chained_of);
    let wanted = |chained: Option<&str>| hook_text(chained, gitdcy);
    let hook_written = existing.as_deref() != Some(wanted(chained.as_deref()).as_str());
    if hook_written {
        if let Some(text) = &existing {
            let aside = keep_aside(&hook)?;
            if !text.contains(HOOK_MARK) {
                chained = aside.file_name().map(|n| n.to_string_lossy().into_owned());
            }
            kept.push(aside.display().to_string());
        }
        fs::write(&hook, wanted(chained.as_deref()))
            .with_context(|| format!("writing {}", hook.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&hook, fs::Permissions::from_mode(0o755))?;
        }
    }
    Ok(InstallReport {
        repository: found.repo.root.display().to_string(),
        rule: rule.name,
        copy: found.copy_path.display().to_string(),
        copy_written,
        hook: hook.display().to_string(),
        hook_written,
        kept,
        chained,
    })
}

// ---------------------------------------------------------------------------
// `gitdcy rules check`: what the hook runs

/// One line of what git gives a pre-push hook on its input.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PushedRef {
    pub local_ref: String,
    pub local_sha: String,
    pub remote_ref: String,
    pub remote_sha: String,
}

fn is_sha(text: &str) -> bool {
    (text.len() == 40 || text.len() == 64) && text.chars().all(|c| c.is_ascii_hexdigit())
}

/// `<local ref> <local sha> <remote ref> <remote sha>` per line. A line that
/// is not that is an error, not skipped: what is not read is not checked.
pub fn parse_pushed_refs(text: &str) -> Result<Vec<PushedRef>> {
    let mut refs = Vec::new();
    for line in text.lines().filter(|l| !l.trim().is_empty()) {
        let words: Vec<&str> = line.split_whitespace().collect();
        match words[..] {
            [local_ref, local_sha, remote_ref, remote_sha]
                if is_sha(local_sha) && is_sha(remote_sha) =>
            {
                refs.push(PushedRef {
                    local_ref: local_ref.into(),
                    local_sha: local_sha.into(),
                    remote_ref: remote_ref.into(),
                    remote_sha: remote_sha.into(),
                })
            }
            _ => bail!("not a line git gives a pre-push hook: {line}"),
        }
    }
    Ok(refs)
}

#[derive(Debug, Clone, Default)]
pub struct CheckRequest {
    /// The remote's name (as git passes it, which is the URL for a push to a URL).
    pub remote: Option<String>,
    pub url: Option<String>,
    /// What the hook got on its input; `None` checks the current branch.
    pub refs: Option<Vec<PushedRef>>,
}

#[derive(Debug, Clone, Serialize)]
pub struct CheckReport {
    pub repository: String,
    pub remote: Option<String>,
    /// Shown without user, password or port.
    pub url: Option<String>,
    pub rule: Option<String>,
    pub source: Option<RuleSource>,
    pub push: Option<String>,
    pub remote_allowed: bool,
    /// Protected branches this push would write to (or delete) directly.
    pub protected_branches: Vec<String>,
    pub range: String,
    pub commits: usize,
    pub findings: Vec<Finding>,
    pub refused: bool,
    /// What was refused and the way forward, one line each.
    pub reasons: Vec<String>,
    /// Work left lying around in the repository (the rules file's
    /// `[hygiene]`): said before the push, never a reason to refuse it.
    pub reminders: Vec<Reminder>,
}

/// How long the push check waits for GitHub's open pull requests.
const HOOK_GH_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);

/// The reminders for a push of `pushing` (commit ids) from this repository.
fn push_reminders(found: &Found, pushing: &[String]) -> Vec<Reminder> {
    let Some(rules) = found.file.as_ref().and_then(|f| f.hygiene.as_ref()) else {
        return Vec::new();
    };
    let urls: Vec<String> = found
        .repo
        .remotes
        .iter()
        .flat_map(|(_, urls)| urls.iter().cloned())
        .collect();
    if rules.skipped_by(&urls).is_some() {
        return Vec::new();
    }
    let github = match hygiene::github_repository(&urls) {
        Some(place) => GitHub::ask_with("gh", &[], &[place], HOOK_GH_TIMEOUT),
        None => GitHub::unknown("not on GitHub"),
    };
    hygiene::read(&found.repo.root, rules, &urls, &github, Some(pushing)).reminders
}

fn is_zero(sha: &str) -> bool {
    !sha.is_empty() && sha.chars().all(|c| c == '0')
}

fn on_no_remote(local: &str) -> Vec<String> {
    vec![local.to_string(), "--not".into(), "--remotes".into()]
}

pub fn check(rules_path: &Path, repo_path: &Path, request: &CheckRequest) -> Result<CheckReport> {
    let found = find(rules_path, repo_path)?;
    let repo = &found.repo;
    let remote = request.remote.clone().unwrap_or_else(|| "origin".into());
    if remote.starts_with('-') {
        bail!("{remote} is not a remote name");
    }
    let url = request.url.clone().or_else(|| push_url(repo, &remote));
    let scan = found.scan_config();

    let mut ranges: Vec<Vec<String>> = Vec::new();
    let mut branches: Vec<String> = Vec::new();
    match &request.refs {
        Some(refs) => {
            for pushed in refs {
                if let Some(branch) = pushed.remote_ref.strip_prefix("refs/heads/") {
                    // A deletion writes to the branch too.
                    branches.push(branch.to_string());
                }
                if is_zero(&pushed.local_sha) {
                    continue; // a deletion carries no commits
                }
                ranges.push(if is_zero(&pushed.remote_sha) {
                    on_no_remote(&pushed.local_sha)
                } else {
                    vec![format!("{}..{}", pushed.remote_sha, pushed.local_sha)]
                });
            }
        }
        None => {
            ranges.push(personal::default_range(&repo.root));
            if let Ok(branch) = git_output(&repo.root, ["symbolic-ref", "--short", "-q", "HEAD"]) {
                let branch = branch.trim();
                if !branch.is_empty() {
                    branches.push(branch.to_string());
                }
            }
        }
    }

    let mut commits = 0;
    let mut findings = Vec::new();
    for range in &mut ranges {
        let read = match personal::scan_commits(&repo.root, range, &scan) {
            Ok(read) => read,
            Err(_) if range.len() == 1 && range[0].contains("..") => {
                // The remote's commit is not here (not fetched): read what is on no remote.
                let local = range[0].rsplit("..").next().unwrap_or("HEAD").to_string();
                *range = on_no_remote(&local);
                personal::scan_commits(&repo.root, range, &scan)?
            }
            Err(error) => return Err(error),
        };
        commits += read.0;
        findings.extend(read.1);
    }

    // No remote: nothing leaves, so there is nothing to refuse; the rule applies once one is added.
    let (remote_ok, protected) = match (&found.rule, &url) {
        (Some(rule), Some(url)) => (
            remote_allowed(&rule.push, url, &found.selfhosted, ssh_hostname),
            protected_in(rule, branches.iter().map(String::as_str)),
        ),
        _ => (true, Vec::new()),
    };

    let shown_url = url.as_deref().map(normalize_url);
    let mut reasons = Vec::new();
    if !found.conflicts.is_empty() {
        reasons.push(format!(
            "Rules {} in {} all match this repository ({}), so its pushes are refused until one is left. Keep one rule for it in the rules file.",
            found.conflicts.join(", "),
            found.rules_path.display(),
            found.matched_by.join("; ")
        ));
    }
    if let Some(rule) = &found.rule {
        let target = shown_url.as_deref().unwrap_or(remote.as_str());
        if !remote_ok {
            let mut line = match &rule.push {
                Push::Nowhere => format!("\"{}\" is not pushed anywhere, and {target} is no exception.", rule.name),
                Push::Selfhosted => format!(
                    "\"{}\" may only be pushed to self-hosted hosts ({}); {target} is not one of them. Push to a remote on one of those hosts instead (git remote -v lists this repository's remotes).",
                    rule.name,
                    if found.selfhosted.is_empty() { "none listed: local paths only".to_string() } else { found.selfhosted.join(", ") }
                ),
                Push::Remotes(list) => format!(
                    "\"{}\" may only be pushed to {}; {target} is not one of them. Push to one of those instead (git remote -v lists this repository's remotes).",
                    rule.name,
                    list.join(", ")
                ),
                Push::Anywhere => String::new(),
            };
            if !rule.note.trim().is_empty() {
                line.push_str(&format!(" The owner's reason: {}", rule.note.trim()));
                if !line.ends_with('.') {
                    line.push('.');
                }
            }
            line.push_str(&format!(
                " Where it may go is the owner's decision, set in {}.",
                found.source_text()
            ));
            reasons.push(line);
        }
        for branch in &protected {
            reasons.push(format!(
                "Branch {branch} of \"{}\" takes no direct push: it is reached only through a pull request. Push a branch of your own (git push {remote} HEAD:refs/heads/NEW-BRANCH) and open a pull request into {branch}.",
                rule.name
            ));
        }
    }
    if !findings.is_empty() {
        reasons.push(format!(
            "The commits being pushed hold personal data in {} place(s), listed above. Take it out of those commits (git commit --amend for the last one, git rebase for older ones) and push again. Whether an address belongs in this repository is the owner's decision; its place is the allow list in the rules file.",
            findings.len()
        ));
    }
    let refused = !reasons.is_empty();
    if refused {
        reasons.push(QUESTION.into());
    }
    let pushing: Vec<String> = match &request.refs {
        Some(refs) => refs
            .iter()
            .filter(|pushed| !is_zero(&pushed.local_sha))
            .map(|pushed| pushed.local_sha.clone())
            .collect(),
        None => git_output(&repo.root, ["rev-parse", "-q", "--verify", "HEAD"])
            .map(|head| vec![head.trim().to_string()])
            .unwrap_or_default(),
    };
    let reminders = push_reminders(&found, &pushing);
    Ok(CheckReport {
        repository: repo.root.display().to_string(),
        remote: Some(remote),
        url: shown_url,
        rule: found.rule.as_ref().map(|r| r.name.clone()),
        source: found.source,
        push: found
            .rule
            .as_ref()
            .map(|r| r.push.describe(&found.selfhosted)),
        remote_allowed: remote_ok,
        protected_branches: protected,
        range: ranges
            .iter()
            .map(|r| r.join(" "))
            .collect::<Vec<_>>()
            .join(", "),
        commits,
        findings,
        refused,
        reasons,
        reminders,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rule(push: Push, protected: &[&str]) -> RepoRule {
        RepoRule {
            name: "example".into(),
            first_commit: None,
            remotes: vec!["forge.example.com/team/example".into()],
            push,
            protected: protected.iter().map(|s| s.to_string()).collect(),
            note: String::new(),
        }
    }

    fn no_alias(_: &str) -> Option<String> {
        None
    }

    #[test]
    fn urls_normalize_without_user_password_port_or_dot_git() {
        assert_eq!(
            normalize_url("git@github.com:Org/repo.git"),
            "github.com/org/repo"
        );
        assert_eq!(
            normalize_url("ssh://git@forge.example.com:2222/team/repo.git"),
            "forge.example.com/team/repo"
        );
        assert_eq!(
            normalize_url("https://user:s3cret@forge.example.com:3000/team/repo/"),
            "forge.example.com/team/repo"
        );
        assert_eq!(
            normalize_url("forge-alias:team/repo.git"),
            "forge-alias/team/repo"
        );
        assert_eq!(normalize_url("/srv/Git/repo.git/"), "/srv/Git/repo.git");
        assert_eq!(
            normalize_url("file:///srv/git/repo.git"),
            "/srv/git/repo.git"
        );
        assert!(is_local(r"C:\repos\x.git"));
        assert!(is_local("./a:b"));
        assert!(!is_local("git@host:x"));
    }

    #[test]
    fn remote_prefixes_end_at_a_path_boundary() {
        let push = Push::Remotes(vec!["github.com/example-org/".into()]);
        assert!(remote_allowed(
            &push,
            "git@github.com:Example-Org/repo.git",
            &[],
            no_alias
        ));
        assert!(remote_allowed(
            &push,
            "https://github.com/example-org/repo",
            &[],
            no_alias
        ));
        assert!(!remote_allowed(
            &push,
            "https://github.com/example-org-other/repo",
            &[],
            no_alias
        ));
        assert!(!remote_allowed(
            &push,
            "https://evil.example/github.com/example-org/repo",
            &[],
            no_alias
        ));
        assert!(!remote_allowed(
            &push,
            "git@github.com:someone/repo.git",
            &[],
            no_alias
        ));
    }

    #[test]
    fn selfhosted_means_listed_hosts_local_paths_and_aliases_to_them() {
        let hosts = vec!["forge.example.com".to_string(), "192.0.2.10".to_string()];
        let push = Push::Selfhosted;
        assert!(remote_allowed(
            &push,
            "ssh://git@192.0.2.10:2222/me/repo.git",
            &hosts,
            no_alias
        ));
        assert!(remote_allowed(
            &push,
            "http://forge.example.com:3000/me/repo.git",
            &hosts,
            no_alias
        ));
        assert!(remote_allowed(&push, "/srv/git/repo.git", &hosts, no_alias));
        assert!(remote_allowed(
            &push,
            "ssh://localhost/srv/repo.git",
            &hosts,
            no_alias
        ));
        assert!(!remote_allowed(
            &push,
            "https://github.com/me/repo",
            &hosts,
            no_alias
        ));
        assert!(!remote_allowed(
            &push,
            "git@github.com:me/repo.git",
            &hosts,
            no_alias
        ));
        let alias = |a: &str| (a == "forge").then(|| "forge.example.com".to_string());
        assert!(remote_allowed(&push, "forge:me/repo.git", &hosts, alias));
        // An alias is SSH's; an https URL to a name is not resolved through it.
        assert!(!remote_allowed(
            &push,
            "https://forge/me/repo.git",
            &hosts,
            alias
        ));
        assert!(!remote_allowed(
            &push,
            "ssh://git@forge.example.com:2222/x",
            &[],
            no_alias
        ));
        assert!(ssh_hostname("-oProxyCommand=x").is_none());
        assert!(ssh_hostname("host;x").is_none());
    }

    #[test]
    fn nowhere_refuses_every_remote_and_anywhere_none() {
        for url in [
            "/srv/git/repo.git",
            "git@github.com:me/repo.git",
            "ssh://localhost/x",
        ] {
            assert!(!remote_allowed(
                &Push::Nowhere,
                url,
                &["localhost".into()],
                no_alias
            ));
            assert!(remote_allowed(&Push::Anywhere, url, &[], no_alias));
        }
    }

    #[test]
    fn protected_branches_take_no_direct_push() {
        let r = rule(Push::Anywhere, &["main", "development"]);
        assert_eq!(
            protected_in(&r, ["feat/x", "development", "development"].into_iter()),
            vec!["development"]
        );
        assert!(protected_in(&r, ["feat/x"].into_iter()).is_empty());
    }

    #[test]
    fn the_rules_file_reads_push_as_a_word_or_a_list() {
        let file: RulesFile = toml::from_str(
            r#"
selfhosted = ["forge.example.com"]
[[repo]]
name = "a"
first_commit = "0123456789abcdef0123456789abcdef01234567"
push = "selfhosted"
protected = ["main"]
note = "stays home"
[[repo]]
name = "b"
remotes = ["github.com/example-org/b"]
push = ["github.com/example-org/"]
[[repo]]
name = "c"
remotes = ["forge.example.com/me/c"]
"#,
        )
        .unwrap();
        file.validate().unwrap();
        assert_eq!(file.repos[0].push, Push::Selfhosted);
        assert_eq!(
            file.repos[1].push,
            Push::Remotes(vec!["github.com/example-org/".into()])
        );
        assert_eq!(file.repos[2].push, Push::Anywhere);
        let again: RulesFile = toml::from_str(&toml::to_string_pretty(&file).unwrap()).unwrap();
        assert_eq!(again, file);
    }

    #[test]
    fn a_rules_file_with_mistakes_is_refused() {
        let bad = [
            "[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npush = \"github\"\n",
            "[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npush = []\n",
            "[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npushh = \"nowhere\"\n",
            "selfhost = [\"x\"]\n",
        ];
        for text in bad {
            assert!(toml::from_str::<RulesFile>(text).is_err(), "{text}");
        }
        let invalid = [
            "[[repo]]\nname = \"a\"\npush = \"nowhere\"\n",
            "[[repo]]\nname = \"a\"\nfirst_commit = \"abc123\"\n",
            "[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\n[[repo]]\nname = \"a\"\nremotes = [\"x/z\"]\n",
            "[[repo]]\nname = \" \"\nremotes = [\"x/y\"]\n",
        ];
        for text in invalid {
            let file: RulesFile = toml::from_str(text).unwrap();
            assert!(file.validate().is_err(), "{text}");
        }
    }

    #[test]
    fn the_hook_checks_then_chains_and_stops_on_a_refusal() {
        let text = hook_text(
            Some("pre-push.bak-2026-01-02"),
            Some(Path::new("/opt/it's/gitdcy")),
        );
        assert!(text.starts_with("#!/bin/sh\n"));
        assert!(text.contains(HOOK_MARK));
        assert!(text.contains(r#"rules check --stdin "$1" "$2" || exit 1"#));
        assert!(text.contains(r#""$(dirname "$0")/$chained" "$@" || exit 1"#));
        assert!(text.contains(r"gitdcy='/opt/it'\''s/gitdcy'"));
        assert_eq!(
            chained_of(&text).as_deref(),
            Some("pre-push.bak-2026-01-02")
        );
        assert_eq!(chained_of(&hook_text(None, None)), None);
        assert!(!text.contains("--no-verify"));
    }

    #[test]
    fn pushed_refs_are_read_from_the_hook_input() {
        let refs = parse_pushed_refs(
            "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n\n",
        )
        .unwrap();
        assert_eq!(refs.len(), 1);
        assert!(parse_pushed_refs("nonsense\n").is_err());
        assert!(parse_pushed_refs(
            "refs/heads/main --output=/tmp/x refs/heads/main 0000000000000000000000000000000000000000\n"
        )
        .is_err());
        assert_eq!(refs[0].remote_ref, "refs/heads/main");
        assert!(is_zero(&refs[0].remote_sha));
        assert!(!is_zero(""));
    }

    #[test]
    fn dates_are_civil_days() {
        assert_eq!(civil_date(0), "1970-01-01");
        assert_eq!(civil_date(11_016), "2000-02-29");
        assert_eq!(civil_date(20_761), "2026-11-04");
    }
}
