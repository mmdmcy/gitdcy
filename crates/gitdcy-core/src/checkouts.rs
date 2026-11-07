//! Checkouts against their upstream: fetch each one, say whether it is up to
//! date, behind, ahead, diverged, has local changes or is missing, and
//! fast-forward the ones that can be. Nothing else ever happens to a
//! checkout: no merge commit, no rebase, no force, and local changes or local
//! commits are never touched.
//!
//! A path is used as given; a leading `~/` is the user's home (also on
//! Windows, where the shell does not expand it), and the report shows the
//! path the way it was given, so a caller can match its own list. Any
//! checkout can be named, so reading its state runs git with hooks and
//! fsmonitor off; only the fetch (skipped with `--no-fetch`) and `update`'s
//! fast-forward run git as the user configured it.

use super::facts::{self, Facts};
use super::reading_git_command;
use super::rules::normalize_url;
use super::{command_error, expand_home, git_command, git_command_output, git_output};
use crate::bail;
use crate::error::{Context, Result};
use serde::Serialize;
use std::ffi::OsStr;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

const FETCH_TIMEOUT: Duration = Duration::from_secs(60);
const FETCH_OUTPUT_LIMIT: usize = 256 * 1024;
/// Checkouts read at the same time; fetches are mostly waiting on the network.
const WORKERS: usize = 8;

/// What to do besides reading.
#[derive(Debug, Clone, Copy, Default)]
pub struct Options {
    /// Fetch each checkout's upstream first (off: as of the last fetch).
    pub fetch: bool,
    /// Fast-forward a checkout that is behind and has no local changes or commits.
    pub update: bool,
    /// Also read what a tool moving the checkout needs ([`Facts`]); slower,
    /// since it hashes local changes.
    pub facts: bool,
}

/// A file with local changes and git's two-letter status for it, as in
/// `git status --porcelain` (`" M"`, `"A "`, `"??"`, `"UU"`, ...).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct ChangedFile {
    pub path: String,
    pub status: String,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Checkout {
    /// As given (`~/code/app` stays `~/code/app`).
    pub path: String,
    /// `current`, `behind`, `ahead`, `diverged`, `changed` (local changes
    /// only), `missing`, `no-upstream` or `error`.
    pub state: String,
    /// Empty when no branch is checked out.
    pub branch: String,
    /// The branch it follows (`origin/main`); empty when none.
    pub tracking_branch: String,
    /// That branch's remote (or `origin`), without user, password or port.
    pub remote: String,
    pub behind: u32,
    pub ahead: u32,
    /// Files with local changes, untracked ones included.
    pub changed: u32,
    pub files: Vec<ChangedFile>,
    /// The checkout's last commit: `hash date subject`.
    pub head: String,
    /// Its commit time in Unix seconds; 0 before the first commit.
    pub head_time: i64,
    /// The upstream's last commit, when behind.
    pub upstream: String,
    /// Fast-forwarded by this run.
    pub updated: bool,
    /// Why the fetch failed; the rest is then as of the last fetch.
    pub fetch_error: String,
    pub detail: String,
    /// With `--facts`, for every checkout that is not missing.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub facts: Option<Facts>,
}

/// Every path, in the order given, read with up to eight at a time.
pub fn checkouts(paths: &[String], options: Options) -> Vec<Checkout> {
    let mut report = vec![Checkout::default(); paths.len()];
    if paths.is_empty() {
        return report;
    }
    let size = paths.len().div_ceil(WORKERS);
    std::thread::scope(|scope| {
        for (given, out) in paths.chunks(size).zip(report.chunks_mut(size)) {
            scope.spawn(move || {
                for (path, slot) in given.iter().zip(out.iter_mut()) {
                    *slot = checkout(path, options);
                }
            });
        }
    });
    report
}

/// One checkout, with its facts last so they show any fast-forward.
pub fn checkout(given: &str, options: Options) -> Checkout {
    let dir = expand_home(PathBuf::from(given));
    let mut found = state_of(given, &dir, options);
    if options.facts && found.state != "missing" {
        found.facts = Some(facts::read(&dir));
    }
    found
}

fn state_of(given: &str, dir: &Path, options: Options) -> Checkout {
    let mut found = Checkout {
        path: given.to_string(),
        ..Checkout::default()
    };
    if !dir.join(".git").exists() {
        found.state = "missing".into();
        found.detail = if dir.is_dir() {
            "not a git checkout at that path".into()
        } else {
            "no folder at that path".into()
        };
        return found;
    }
    if let Err(error) = read(dir, &mut found, options.fetch) {
        found.state = "error".into();
        found.detail = first_line(&format!("{error:#}"));
        return found;
    }
    if options.update && found.state == "behind" && found.changed == 0 {
        let behind = found.behind;
        match git_output(dir, ["merge", "--ff-only", "--quiet", "@{u}"]) {
            Ok(_) => {
                let fetch_error = std::mem::take(&mut found.fetch_error);
                let reread = read(dir, &mut found, false);
                found.fetch_error = fetch_error;
                found.updated = true;
                if let Err(error) = reread {
                    found.state = "error".into();
                    found.detail = format!(
                        "fast-forwarded {behind} commit(s), then could not read it: {}",
                        first_line(&format!("{error:#}"))
                    );
                } else if found.state == "current" {
                    found.detail = format!("fast-forwarded {behind} commit(s); up to date");
                }
            }
            Err(error) => {
                found.detail = format!(
                    "{behind} commit(s) behind; the fast-forward failed: {}",
                    first_line(&format!("{error:#}"))
                );
            }
        }
    }
    if !found.fetch_error.is_empty() {
        found.detail = format!(
            "{}; the fetch failed ({}), so this is as of the last fetch",
            found.detail, found.fetch_error
        );
    }
    found
}

/// Reads branch, upstream, changes and commits into `found`.
fn read(dir: &Path, found: &mut Checkout, fetch: bool) -> Result<()> {
    (found.behind, found.ahead) = (0, 0);
    found.branch = read_text(dir, ["branch", "--show-current"])?
        .trim()
        .to_string();
    found.tracking_branch = read_text(
        dir,
        ["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"],
    )
    .map(|text| text.trim().to_string())
    .unwrap_or_default();
    if fetch && !found.tracking_branch.is_empty() {
        if let Err(error) = fetch_upstream(dir) {
            found.fetch_error = first_line(&format!("{error:#}"));
        }
    }
    found.remote = remote_of(dir, &found.branch);
    found.files = parse_status(&read_git(
        dir,
        ["status", "--porcelain=v1", "-z", "--untracked-files=normal"],
    )?);
    found.changed = u32::try_from(found.files.len()).unwrap_or(u32::MAX);
    (found.head_time, found.head) = last_commit(dir, "HEAD");
    found.upstream.clear();
    if found.tracking_branch.is_empty() {
        found.state = "no-upstream".into();
        found.detail = if found.branch.is_empty() {
            "not on a branch (detached HEAD)".into()
        } else {
            "the branch follows no remote branch".into()
        };
        return Ok(());
    }
    let counts = read_text(dir, ["rev-list", "--left-right", "--count", "@{u}...HEAD"])?;
    let mut numbers = counts.split_whitespace().map(|n| n.parse().unwrap_or(0));
    found.behind = numbers.next().unwrap_or(0);
    found.ahead = numbers.next().unwrap_or(0);
    if found.behind > 0 {
        found.upstream = last_commit(dir, "@{u}").1;
    }
    let (state, detail) = verdict(found.behind, found.ahead, found.changed);
    found.state = state.into();
    found.detail = detail;
    Ok(())
}

/// The state and its sentence, from the counts.
pub fn verdict(behind: u32, ahead: u32, changed: u32) -> (&'static str, String) {
    match (behind, ahead) {
        (0, 0) if changed > 0 => (
            "changed",
            format!("{changed} local change(s), not committed"),
        ),
        (0, 0) => ("current", "up to date".into()),
        (_, 0) if changed > 0 => (
            "behind",
            format!(
                "{behind} commit(s) behind, but {changed} local change(s): commit or stash them first"
            ),
        ),
        (_, 0) => (
            "behind",
            format!("{behind} commit(s) behind; checkouts update brings it up"),
        ),
        (0, _) => ("ahead", format!("{ahead} local commit(s) not pushed")),
        _ => (
            "diverged",
            format!("{behind} behind and {ahead} ahead: merge or rebase by hand"),
        ),
    }
}

/// `N checkout(s): N up to date, N behind, N need attention`.
pub fn summary(checkouts: &[Checkout]) -> String {
    let count = |state: &str| checkouts.iter().filter(|c| c.state == state).count();
    format!(
        "{} checkout(s): {} up to date, {} behind, {} need attention",
        checkouts.len(),
        count("current"),
        count("behind"),
        checkouts.len() - count("current") - count("behind")
    )
}

/// `git fetch` from the branch's upstream remote, never asking for a password.
fn fetch_upstream(dir: &Path) -> Result<()> {
    let mut command = git_command(dir, ["fetch", "--quiet"]);
    command
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GCM_INTERACTIVE", "never");
    let started = Instant::now();
    let output = super::process::run_bounded_command(command, FETCH_OUTPUT_LIMIT, FETCH_TIMEOUT)?;
    if output.status.success() {
        return Ok(());
    }
    if started.elapsed() >= FETCH_TIMEOUT {
        bail!(
            "git fetch took longer than {} s and was stopped",
            FETCH_TIMEOUT.as_secs()
        );
    }
    bail!("{}", command_error("git fetch", &output))
}

/// The remote the branch follows, else `origin`, as `host/path`.
fn remote_of(dir: &Path, branch: &str) -> String {
    let configured = (!branch.is_empty())
        .then(|| read_text(dir, ["config", "--get", &format!("branch.{branch}.remote")]).ok())
        .flatten()
        .map(|name| name.trim().to_string())
        .filter(|name| !name.is_empty() && name != ".");
    let name = configured.unwrap_or_else(|| "origin".to_string());
    read_text(dir, ["remote", "get-url", &name])
        .map(|url| normalize_url(url.trim()))
        .unwrap_or_default()
}

/// `(unix seconds, "hash date subject")` of a commit; empty before the first one.
fn last_commit(dir: &Path, rev: &str) -> (i64, String) {
    let text =
        read_text(dir, ["log", "-1", "--format=%ct %h %cs %s", rev, "--"]).unwrap_or_default();
    let line = text.trim_end_matches(['\r', '\n']);
    match line.split_once(' ') {
        Some((time, rest)) => (time.parse().unwrap_or(0), rest.to_string()),
        None => (0, String::new()),
    }
}

/// What `git ARGS` printed, read with hooks and fsmonitor off.
fn read_git<I, S>(dir: &Path, args: I) -> Result<Vec<u8>>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let output = git_command_output(reading_git_command(dir, args)).context("run git")?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(output.stdout)
}

fn read_text<I, S>(dir: &Path, args: I) -> Result<String>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    Ok(String::from_utf8_lossy(&read_git(dir, args)?).into_owned())
}

/// `git status --porcelain=v1 -z`: `XY path`, and after a rename or copy the
/// old path as a field of its own.
fn parse_status(output: &[u8]) -> Vec<ChangedFile> {
    let mut files = Vec::new();
    let mut fields = output.split(|byte| *byte == 0);
    while let Some(field) = fields.next() {
        if field.len() < 4 {
            continue;
        }
        let status = String::from_utf8_lossy(&field[..2]).into_owned();
        if status.contains(['R', 'C']) {
            let _ = fields.next();
        }
        files.push(ChangedFile {
            path: String::from_utf8_lossy(&field[3..]).into_owned(),
            status,
        });
    }
    files
}

fn first_line(text: &str) -> String {
    text.lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .unwrap_or("")
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn states_come_from_behind_ahead_and_changes() {
        assert_eq!(verdict(0, 0, 0), ("current", "up to date".to_string()));
        assert_eq!(verdict(0, 0, 3).0, "changed");
        assert_eq!(
            verdict(2, 0, 0),
            (
                "behind",
                "2 commit(s) behind; checkouts update brings it up".to_string()
            )
        );
        let (state, detail) = verdict(2, 0, 1);
        assert_eq!(state, "behind");
        assert!(detail.contains("commit or stash them first"), "{detail}");
        assert_eq!(verdict(0, 1, 5).0, "ahead");
        assert_eq!(verdict(2, 1, 0).0, "diverged");
    }

    #[test]
    fn porcelain_keeps_each_file_once_with_its_status() {
        let output = b" M src/main.rs\0R  new.rs\0old.rs\0?? notes/\0UU both.txt\0";
        let files = parse_status(output);
        let shown = files
            .iter()
            .map(|f| (f.status.as_str(), f.path.as_str()))
            .collect::<Vec<_>>();
        assert_eq!(
            shown,
            vec![
                (" M", "src/main.rs"),
                ("R ", "new.rs"),
                ("??", "notes/"),
                ("UU", "both.txt"),
            ]
        );
        assert!(parse_status(b"").is_empty());
    }

    #[test]
    fn the_summary_counts_what_needs_a_person() {
        let list = ["current", "behind", "diverged", "missing"]
            .iter()
            .map(|state| Checkout {
                state: state.to_string(),
                ..Checkout::default()
            })
            .collect::<Vec<_>>();
        assert_eq!(
            summary(&list),
            "4 checkout(s): 1 up to date, 1 behind, 2 need attention"
        );
    }

    #[test]
    fn a_path_that_is_no_checkout_is_missing() {
        let found = checkout("/nonexistent/gitdcy-checkouts-test", Options::default());
        assert_eq!(found.state, "missing");
        assert_eq!(found.detail, "no folder at that path");
        assert_eq!(found.path, "/nonexistent/gitdcy-checkouts-test");
        let with_facts = Options {
            facts: true,
            ..Options::default()
        };
        assert!(checkout("/nonexistent/gitdcy-checkouts-test", with_facts)
            .facts
            .is_none());
    }

    #[test]
    fn facts_come_only_when_asked_even_when_git_fails() {
        let dir = std::env::temp_dir().join(format!(
            "gitdcy-checkout-facts-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::create_dir_all(dir.join(".git")).unwrap();
        let path = dir.display().to_string();
        let plain = checkout(&path, Options::default());
        assert_eq!(
            (plain.state.as_str(), plain.facts.is_none()),
            ("error", true)
        );
        let asked = checkout(
            &path,
            Options {
                facts: true,
                ..Options::default()
            },
        );
        assert_eq!(asked.state, "error");
        assert!(asked.facts.unwrap().status_error.is_some());
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
