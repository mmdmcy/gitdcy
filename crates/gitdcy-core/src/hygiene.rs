//! No dirty dishes: reminders about work left lying around in a checkout, so
//! it is picked up before work starts and before a push.
//!
//! Four kinds of reminder, each with what clears it:
//!
//! - a dirty tree (local changes not committed), or a pull not done (commits
//!   on the upstream, as of the last fetch);
//! - commits on no remote from before today (they stayed unpushed past a
//!   day's end), or, before a push, the ones that push leaves behind;
//! - a branch not merged into the default branch after some days;
//! - an open pull request, read from GitHub through `gh` when it is installed
//!   and signed in; otherwise their state is unknown, and the reminder says so.
//!
//! The reminders are on when the rules file has a `[hygiene]` table; a
//! repository whose remote starts with one of its `skip` prefixes keeps its
//! own rules and gets none. Read-only: git runs with hooks and fsmonitor off
//! and without optional locks, and nothing is fetched.

use super::process::run_bounded_command;
use super::reading_git_command;
use super::rules::normalize_url;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const OUTPUT_LIMIT: usize = 1024 * 1024;
const GIT_TIMEOUT: Duration = Duration::from_secs(8);
const DAY: i64 = 86_400;

fn default_branch_days() -> u64 {
    3
}

/// The rules file's `[hygiene]` table.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HygieneRules {
    /// A branch not merged after this many days is a reminder.
    #[serde(default = "default_branch_days")]
    pub branch_days: u64,
    /// Remote URL prefixes (`github.com/example-org/`) of repositories that
    /// keep their own rules, such as an employer's: no reminders for them.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub skip: Vec<String>,
}

impl HygieneRules {
    pub(crate) fn validate(&self) -> crate::error::Result<()> {
        if self.branch_days == 0 {
            crate::bail!(
                "[hygiene] branch_days = 0 would remind of every branch at once; say 1 or more"
            );
        }
        if self.skip.iter().any(|prefix| prefix.trim().is_empty()) {
            crate::bail!("[hygiene] an empty prefix in skip");
        }
        Ok(())
    }

    /// The `skip` prefix one of these remote URLs starts with, if any.
    pub fn skipped_by(&self, urls: &[String]) -> Option<&str> {
        self.skip
            .iter()
            .find(|prefix| urls.iter().any(|url| has_prefix(url, prefix)))
            .map(String::as_str)
    }
}

/// `prefix` matches the URL or what lies under it, ignoring case (hosts and
/// GitHub owners are case-insensitive).
fn has_prefix(url: &str, prefix: &str) -> bool {
    let url = normalize_url(url).to_lowercase();
    let prefix = normalize_url(prefix).to_lowercase();
    let prefix = prefix.trim_end_matches('/');
    !prefix.is_empty()
        && (url == prefix || (url.starts_with(prefix) && url[prefix.len()..].starts_with('/')))
}

/// A branch with commits the default branch does not have.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Branch {
    pub branch: String,
    /// What it is compared with (`origin/main`).
    pub base: String,
    /// Its commits that `base` lacks.
    pub commits: u64,
    /// Days since the oldest of them.
    pub days: u64,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PullRequest {
    pub number: u64,
    pub title: String,
    pub url: String,
    pub author: String,
    /// Days since it was opened.
    pub days: u64,
    pub draft: bool,
}

/// The open pull requests of a checkout's repository.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct PullRequests {
    /// `known` (read from GitHub), `unknown` (it is on GitHub, and `gh`
    /// could not be asked) or `not-asked` (not on GitHub).
    pub state: String,
    /// Why they are unknown or not asked.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub detail: String,
    /// The repository on GitHub (`owner/name`).
    #[serde(skip_serializing_if = "String::is_empty")]
    pub repository: String,
    pub open: Vec<PullRequest>,
}

/// One reminder: what it is about, in words, and what clears it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Reminder {
    /// `dirty`, `behind`, `unpushed`, `branch`, `pull-request` or
    /// `pull-requests-unknown`.
    pub kind: &'static str,
    pub text: String,
    pub fix: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Hygiene {
    /// Files with local changes, untracked ones included.
    pub changed: u64,
    /// The upstream (`origin/main`), when the checked-out branch has one.
    pub upstream: Option<String>,
    /// Commits on the upstream that were not pulled, as of the last fetch.
    pub behind: u64,
    /// When the repository last fetched (Unix seconds); null when never.
    pub fetched_at: Option<i64>,
    /// Commits on local branches that no remote-tracking branch holds.
    pub unpushed: u64,
    /// Of those, the ones made before today (local midnight).
    pub unpushed_before_today: u64,
    /// The default branch branches are compared with; null when none is found.
    pub base: Option<String>,
    /// Branches not merged into `base` for `branch_days` or more.
    pub branches: Vec<Branch>,
    pub pull_requests: PullRequests,
    pub reminders: Vec<Reminder>,
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

fn git(dir: &Path, args: &[&str]) -> Option<String> {
    let output =
        run_bounded_command(reading_git_command(dir, args), OUTPUT_LIMIT, GIT_TIMEOUT).ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_string())
}

fn count(dir: &Path, args: &[&str]) -> u64 {
    git(dir, args).and_then(|out| out.parse().ok()).unwrap_or(0)
}

/// The branch others are compared with: `origin/HEAD`, else `origin/main` or
/// `origin/master`, else a local `main` or `master`.
fn base_branch(dir: &Path) -> Option<String> {
    if let Some(head) = git(
        dir,
        &["symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"],
    )
    .filter(|head| !head.is_empty())
    {
        return Some(head);
    }
    ["origin/main", "origin/master", "main", "master"]
        .into_iter()
        .find(|name| {
            git(
                dir,
                &["rev-parse", "--verify", "-q", &format!("{name}^{{commit}}")],
            )
            .is_some()
        })
        .map(str::to_string)
}

/// Local branches with commits `base` lacks, whose oldest such commit is
/// `days` or more old.
fn unmerged(dir: &Path, base: &str, days: u64, at: i64) -> Vec<Branch> {
    let short = base.rsplit_once('/').map_or(base, |(_, name)| name);
    let Some(list) = git(
        dir,
        &["for-each-ref", "--format=%(refname:short)", "refs/heads"],
    ) else {
        return Vec::new();
    };
    let mut found = Vec::new();
    for branch in list.lines().map(str::trim).filter(|b| !b.is_empty()) {
        if branch == short || branch == base {
            continue;
        }
        let range = format!("{base}..refs/heads/{branch}");
        let Some(times) = git(dir, &["log", "--no-merges", "--format=%ct", &range]) else {
            continue;
        };
        let times: Vec<i64> = times
            .lines()
            .filter_map(|t| t.trim().parse().ok())
            .collect();
        let Some(oldest) = times.iter().min() else {
            continue;
        };
        let age = ((at - oldest).max(0) / DAY) as u64;
        if age >= days {
            found.push(Branch {
                branch: branch.to_string(),
                base: base.to_string(),
                commits: times.len() as u64,
                days: age,
            });
        }
    }
    found
}

/// The repository's place on GitHub (`owner/name`), from its remotes.
pub fn github_repository(urls: &[String]) -> Option<String> {
    urls.iter().find_map(|url| {
        let normalized = normalize_url(url);
        let rest = normalized.strip_prefix("github.com/")?;
        let mut parts = rest.split('/');
        let (owner, name) = (parts.next()?, parts.next()?);
        (!owner.is_empty() && !name.is_empty() && parts.next().is_none())
            .then(|| format!("{owner}/{name}"))
    })
}

/// Days since 1970-01-01 of a civil date.
fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let year = if month <= 2 { year - 1 } else { year };
    let era = year.div_euclid(400);
    let yoe = year - era * 400;
    let mp = (month + 9) % 12;
    let doy = (153 * mp + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

/// `2026-11-01T10:00:00Z` -> Unix seconds (UTC; the time zone is ignored, a
/// day is all that is counted).
fn unix_time(stamp: &str) -> Option<i64> {
    let (date, time) = stamp.split_once('T')?;
    let mut date = date.split('-').map(|part| part.parse::<i64>().ok());
    let (year, month, day) = (date.next()??, date.next()??, date.next()??);
    let mut time = time
        .trim_end_matches('Z')
        .split(['+', '-'])
        .next()?
        .split(':')
        .map(|part| part.split('.').next().and_then(|p| p.parse::<i64>().ok()));
    let (hour, minute) = (time.next()??, time.next()??);
    let second = time.next().flatten().unwrap_or(0);
    Some(days_from_civil(year, month, day) * DAY + hour * 3600 + minute * 60 + second)
}

#[derive(Deserialize)]
struct Found {
    number: u64,
    #[serde(default)]
    title: String,
    #[serde(default)]
    url: String,
    #[serde(default, rename = "createdAt")]
    created_at: String,
    #[serde(default, rename = "isDraft")]
    is_draft: bool,
    #[serde(default)]
    author: Option<Login>,
    #[serde(default)]
    repository: Option<Named>,
}

#[derive(Deserialize)]
struct Login {
    #[serde(default)]
    login: String,
}

#[derive(Deserialize)]
struct Named {
    #[serde(default, rename = "nameWithOwner")]
    name_with_owner: String,
}

/// The open pull requests on GitHub, by repository (`owner/name`, lowercase),
/// or why they could not be read. One `gh` search per owner, or for one
/// repository.
pub struct GitHub {
    found: Result<BTreeMap<String, Vec<PullRequest>>, String>,
}

impl GitHub {
    /// Asks for the open pull requests of these owners' repositories, or of
    /// these repositories (`owner/name`), each question
    /// stopped after `timeout`; `program` is `gh`.
    pub fn ask_with(
        program: &str,
        owners: &[String],
        repositories: &[String],
        timeout: Duration,
    ) -> Self {
        let mut found = BTreeMap::new();
        let mut asks: Vec<[String; 2]> = owners
            .iter()
            .map(|owner| ["--owner".to_string(), owner.clone()])
            .collect();
        asks.extend(
            repositories
                .iter()
                .map(|repo| ["--repo".to_string(), repo.clone()]),
        );
        for [flag, value] in asks {
            let mut command = Command::new(program);
            command
                .args(["search", "prs", &flag, &value, "--state", "open"])
                .args([
                    "--json",
                    "number,title,url,createdAt,author,repository,isDraft",
                    "--limit",
                    "200",
                ])
                .env("GH_PROMPT_DISABLED", "1")
                .env("GH_NO_UPDATE_NOTIFIER", "1");
            let output = match run_bounded_command(command, OUTPUT_LIMIT, timeout) {
                Ok(output) => output,
                Err(_) => {
                    return Self {
                        found: Err(format!("{program} is not installed here")),
                    }
                }
            };
            if !output.status.success() {
                let said = String::from_utf8_lossy(&output.stderr);
                let said = said
                    .lines()
                    .map(str::trim)
                    .find(|line| !line.is_empty())
                    .map(str::to_string)
                    .unwrap_or_else(|| format!("no answer within {} s", timeout.as_secs()));
                return Self {
                    found: Err(format!("{program} could not be asked: {said}")),
                };
            }
            let Ok(list) = serde_json::from_slice::<Vec<Found>>(&output.stdout) else {
                return Self {
                    found: Err(format!("{program} answered something unexpected")),
                };
            };
            let at = now();
            for pr in list {
                let Some(repo) = pr.repository.map(|r| r.name_with_owner.to_lowercase()) else {
                    continue;
                };
                let opened = unix_time(&pr.created_at).unwrap_or(at);
                let entry: &mut Vec<PullRequest> = found.entry(repo).or_default();
                if entry.iter().any(|known| known.number == pr.number) {
                    continue;
                }
                entry.push(PullRequest {
                    number: pr.number,
                    title: pr.title,
                    url: pr.url,
                    author: pr.author.map(|a| a.login).unwrap_or_default(),
                    days: ((at - opened).max(0) / DAY) as u64,
                    draft: pr.is_draft,
                });
            }
        }
        for list in found.values_mut() {
            list.sort_by_key(|pr| pr.number);
        }
        Self { found: Ok(found) }
    }

    /// Nothing asked: every repository on GitHub is unknown, for this reason.
    pub fn unknown(why: &str) -> Self {
        Self {
            found: Err(why.to_string()),
        }
    }

    /// Why GitHub's pull requests could not be read, if they could not.
    pub fn error(&self) -> Option<&str> {
        self.found.as_ref().err().map(String::as_str)
    }

    fn of(&self, repository: Option<&str>) -> PullRequests {
        let Some(repository) = repository else {
            return PullRequests {
                state: "not-asked".into(),
                detail: "not on GitHub: only GitHub's pull requests are read, through gh".into(),
                ..PullRequests::default()
            };
        };
        match &self.found {
            Ok(found) => PullRequests {
                state: "known".into(),
                repository: repository.to_string(),
                open: found
                    .get(&repository.to_lowercase())
                    .cloned()
                    .unwrap_or_default(),
                ..PullRequests::default()
            },
            Err(why) => PullRequests {
                state: "unknown".into(),
                detail: why.clone(),
                repository: repository.to_string(),
                open: Vec::new(),
            },
        }
    }
}

/// The checkout's dishes. `urls` are its remote URLs; `pushing` the commits a
/// push sends (before a push: what it leaves behind is reminded of, whatever
/// its age), `None` for a scan (commits from before today).
pub fn read(
    dir: &Path,
    rules: &HygieneRules,
    urls: &[String],
    github: &GitHub,
    pushing: Option<&[String]>,
) -> Hygiene {
    let at = now();
    let shown = dir.display().to_string();
    let changed = git(
        dir,
        &["status", "--porcelain=v1", "--untracked-files=normal"],
    )
    .map(|out| out.lines().filter(|l| !l.is_empty()).count() as u64)
    .unwrap_or(0);
    let upstream = git(
        dir,
        &[
            "rev-parse",
            "--abbrev-ref",
            "--symbolic-full-name",
            "@{upstream}",
        ],
    )
    .filter(|u| !u.is_empty());
    let behind = upstream.as_ref().map_or(0, |_| {
        count(dir, &["rev-list", "--count", "HEAD..@{upstream}"])
    });
    let fetched_at = git(
        dir,
        &[
            "rev-parse",
            "--path-format=absolute",
            "--git-path",
            "FETCH_HEAD",
        ],
    )
    .and_then(|path| std::fs::metadata(path).ok())
    .and_then(|meta| meta.modified().ok())
    .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
    .map(|age| age.as_secs() as i64);
    let has_remote = git(dir, &["remote"]).is_some_and(|r| !r.is_empty());
    let mut on_no_remote = vec!["rev-list", "--count", "--branches", "--not", "--remotes"];
    let pushed: Vec<&str> = pushing
        .unwrap_or_default()
        .iter()
        .map(String::as_str)
        .collect();
    on_no_remote.extend(pushed.iter().copied());
    let unpushed = if has_remote {
        count(dir, &on_no_remote)
    } else {
        0
    };
    let unpushed_before_today = if unpushed > 0 {
        let mut before = vec![
            "rev-list",
            "--count",
            "--before=midnight",
            "--branches",
            "--not",
            "--remotes",
        ];
        before.extend(pushed.iter().copied());
        count(dir, &before)
    } else {
        0
    };
    let base = base_branch(dir);
    let branches = base
        .as_deref()
        .map(|base| unmerged(dir, base, rules.branch_days, at))
        .unwrap_or_default();
    let pull_requests = github.of(github_repository(urls).as_deref());

    let mut reminders = Vec::new();
    if changed > 0 {
        reminders.push(Reminder {
            kind: "dirty",
            text: format!("{changed} file(s) changed and not committed"),
            fix: format!("commit or stash them (git -C {shown} status)"),
        });
    }
    if behind > 0 {
        let upstream = upstream.as_deref().unwrap_or("the upstream");
        reminders.push(Reminder {
            kind: "behind",
            text: format!("{behind} commit(s) on {upstream} not pulled (as of the last fetch)"),
            fix: format!("git -C {shown} pull --ff-only"),
        });
    }
    let left = if pushing.is_some() {
        unpushed
    } else {
        unpushed_before_today
    };
    if left > 0 {
        reminders.push(Reminder {
            kind: "unpushed",
            text: if pushing.is_some() {
                format!("{left} commit(s) on other branches are on no remote")
            } else {
                format!("{left} commit(s) from before today are on no remote")
            },
            fix: format!(
                "push them (git -C {shown} log --branches --not --remotes --oneline lists them)"
            ),
        });
    }
    for branch in &branches {
        reminders.push(Reminder {
            kind: "branch",
            text: format!(
                "branch {} not merged into {} for {} day(s) ({} commit(s))",
                branch.branch, branch.base, branch.days, branch.commits
            ),
            fix: format!(
                "merge it, or delete it when it was merged another way (git -C {shown} branch -d {})",
                branch.branch
            ),
        });
    }
    for pr in &pull_requests.open {
        reminders.push(Reminder {
            kind: "pull-request",
            text: format!(
                "pull request #{} open for {} day(s){}: {}",
                pr.number,
                pr.days,
                if pr.draft { " (draft)" } else { "" },
                pr.title
            ),
            fix: format!("merge or close it: {}", pr.url),
        });
    }
    if pull_requests.state == "unknown" {
        reminders.push(Reminder {
            kind: "pull-requests-unknown",
            text: format!("open pull requests unknown ({})", pull_requests.detail),
            fix: "gh auth login, then ask again".into(),
        });
    }
    Hygiene {
        changed,
        upstream,
        behind,
        fetched_at,
        unpushed,
        unpushed_before_today,
        base,
        branches,
        pull_requests,
        reminders,
    }
}

/// The reminders in one line, for a hook.
pub fn one_line(hygiene: &Hygiene) -> Option<String> {
    (!hygiene.reminders.is_empty()).then(|| {
        hygiene
            .reminders
            .iter()
            .map(|r| r.text.as_str())
            .collect::<Vec<_>>()
            .join("; ")
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn github_places_and_skip_prefixes_end_at_a_path_boundary() {
        let urls = vec![
            "ssh://git@forge.example.com:2222/team/app.git".to_string(),
            "git@github.com:Example-Org/App.git".to_string(),
        ];
        assert_eq!(github_repository(&urls).as_deref(), Some("example-org/app"));
        assert_eq!(github_repository(&["https://gitlab.com/a/b".into()]), None);
        let rules = HygieneRules {
            branch_days: 3,
            skip: vec!["github.com/example-org/".into()],
        };
        assert_eq!(rules.skipped_by(&urls), Some("github.com/example-org/"));
        assert_eq!(
            rules.skipped_by(&["https://github.com/example-org-other/app".into()]),
            None
        );
        assert!(HygieneRules {
            branch_days: 0,
            skip: vec![]
        }
        .validate()
        .is_err());
    }

    #[test]
    fn github_times_are_read_as_days() {
        assert_eq!(unix_time("1970-01-02T00:00:00Z"), Some(DAY));
        assert_eq!(
            unix_time("2026-11-04T12:30:15Z"),
            Some(days_from_civil(2026, 11, 4) * DAY + 12 * 3600 + 30 * 60 + 15)
        );
        assert_eq!(days_from_civil(2000, 3, 1), 11_017);
        assert_eq!(unix_time("not a time"), None);
    }

    #[test]
    fn pull_requests_are_known_unknown_or_not_asked() {
        let unknown = GitHub::unknown("gh is not installed here");
        assert_eq!(unknown.of(None).state, "not-asked");
        let of = unknown.of(Some("o/r"));
        assert_eq!(
            (of.state.as_str(), of.detail.as_str()),
            ("unknown", "gh is not installed here")
        );
        let known = GitHub {
            found: Ok(BTreeMap::from([(
                "o/r".to_string(),
                vec![PullRequest {
                    number: 4,
                    title: "t".into(),
                    url: "u".into(),
                    author: "a".into(),
                    days: 2,
                    draft: false,
                }],
            )])),
        };
        assert_eq!(known.of(Some("O/R")).open.len(), 1);
        assert!(known.of(Some("o/other")).open.is_empty());
        let missing = GitHub::ask_with(
            "/nonexistent/gh",
            &["o".into()],
            &[],
            Duration::from_secs(5),
        );
        assert_eq!(missing.of(Some("o/r")).state, "unknown");
    }
}
