//! `gitdcy rules scan`: every checkout on the device against the rules, read
//! only. Per checkout: which rule applies and where it lets the repository
//! go, the remotes that rule forbids (a self-hosted-only repository with a
//! public remote), whether git runs the push check there, personal data in
//! what already reached a remote, and the reminders about work left lying
//! around ([`super::hygiene`]).

use super::expand_home;
use super::hygiene::{self, GitHub, Hygiene, HygieneRules};
use super::personal::{self, Finding, ScanConfig};
use super::rules::{read_rules, rules_report, HookState, RemoteState, RuleSource, RulesReport};
use anyhow::Result;
use serde::Serialize;
use std::collections::BTreeSet;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// How far back pushed commits are read for personal data without `--since`.
pub const SINCE: &str = "7 days ago";
/// Checkouts read at the same time.
const WORKERS: usize = 8;
const GH_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Debug, Clone, Default, Serialize)]
pub struct CheckoutScan {
    /// As given (`~/code/app` stays `~/code/app`).
    pub path: String,
    /// `ok`, `missing` or `error`.
    pub state: String,
    pub detail: String,
    /// Its top folder.
    pub repository: String,
    pub rule: Option<String>,
    pub source: Option<RuleSource>,
    /// Where the rule lets it go, in words.
    pub push: Option<String>,
    /// Rules that all match it: its pushes are refused until one is left.
    pub conflicts: Vec<String>,
    pub remotes: Vec<RemoteState>,
    /// The remotes its rule does not allow: a push there is refused.
    pub forbidden: Vec<RemoteState>,
    /// The pre-push hook, for a checkout with a rule.
    pub hook: Option<HookState>,
    /// Commits that reached a remote since `since`, read for personal data.
    pub pushed_commits: usize,
    pub pushed_findings: Vec<Finding>,
    /// The reminders; null when they are off for it (`hygiene_off` says why).
    pub hygiene: Option<Hygiene>,
    pub hygiene_off: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ScanReport {
    pub rules_file: String,
    pub rules_file_found: bool,
    /// What already reached a remote is read from this on (git's `--since`).
    pub since: String,
    /// The `[hygiene]` settings; null when the rules file has none (no reminders).
    pub hygiene: Option<HygieneRules>,
    /// How many personal words and allowed addresses are read for (the words are not shown).
    pub personal_words: usize,
    pub allowed_addresses: usize,
    pub checkouts: Vec<CheckoutScan>,
}

/// git reads a bare date as that day at the current time; a day means from its start.
pub fn since_text(since: Option<&str>) -> String {
    match since.map(str::trim).filter(|s| !s.is_empty()) {
        Some(day) if day.len() == 10 && day.chars().all(|c| c.is_ascii_digit() || c == '-') => {
            format!("{day} 00:00")
        }
        Some(other) => other.to_string(),
        None => SINCE.to_string(),
    }
}

/// `items` through `f`, with up to eight at a time, in order.
fn parallel<T: Sync, U: Default + Clone + Send>(items: &[T], f: impl Fn(&T) -> U + Sync) -> Vec<U> {
    let mut out = vec![U::default(); items.len()];
    if items.is_empty() {
        return out;
    }
    let size = items.len().div_ceil(WORKERS);
    std::thread::scope(|scope| {
        for (given, slots) in items.chunks(size).zip(out.chunks_mut(size)) {
            let f = &f;
            scope.spawn(move || {
                for (item, slot) in given.iter().zip(slots.iter_mut()) {
                    *slot = f(item);
                }
            });
        }
    });
    out
}

fn first_line(text: &str) -> String {
    text.lines().next().unwrap_or(text).trim().to_string()
}

/// Every checkout in `paths` against the rules file at `rules_path`. `gh` is
/// the program asked for open pull requests (`gh` on the PATH).
pub fn scan(
    rules_path: &Path,
    paths: &[String],
    since: Option<&str>,
    gitdcy: Option<&Path>,
    gh: &str,
) -> Result<ScanReport> {
    let file = read_rules(rules_path)?;
    let since = since_text(since);
    let rules = file.as_ref().and_then(|f| f.hygiene.clone());
    let config = file
        .as_ref()
        .map(|f| ScanConfig {
            words: f.personal.clone(),
            allow: f.allow.clone(),
        })
        .unwrap_or_default();
    let reports: Vec<Option<std::result::Result<RulesReport, String>>> = parallel(paths, |given| {
        let dir = expand_home(PathBuf::from(given));
        if !dir.join(".git").exists() {
            return None;
        }
        Some(rules_report(rules_path, &dir, gitdcy).map_err(|e| first_line(&format!("{e:#}"))))
    });
    let urls = |report: &RulesReport| -> Vec<String> {
        report
            .remotes
            .iter()
            .filter_map(|remote| remote.url.clone())
            .collect()
    };
    // One question to GitHub per owner, for every checkout the reminders are for.
    let owners: Vec<String> = match &rules {
        Some(rules) => reports
            .iter()
            .flatten()
            .flatten()
            .map(urls)
            .filter(|urls| rules.skipped_by(urls).is_none())
            .filter_map(|urls| hygiene::github_repository(&urls))
            .filter_map(|repo| repo.split_once('/').map(|(owner, _)| owner.to_string()))
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect(),
        None => Vec::new(),
    };
    let github = if owners.is_empty() {
        GitHub::unknown("not asked")
    } else {
        GitHub::ask_with(gh, &owners, &[], GH_TIMEOUT)
    };
    let jobs: Vec<(String, Option<std::result::Result<RulesReport, String>>)> =
        paths.iter().cloned().zip(reports).collect();
    let checkouts = parallel(&jobs, |(given, report)| {
        let mut found = CheckoutScan {
            path: given.clone(),
            ..CheckoutScan::default()
        };
        let report = match report {
            None => {
                let dir = expand_home(PathBuf::from(given));
                found.state = "missing".into();
                found.detail = if dir.is_dir() {
                    "not a git checkout at that path".into()
                } else {
                    "no folder at that path".into()
                };
                return found;
            }
            Some(Err(error)) => {
                found.state = "error".into();
                found.detail = error.clone();
                return found;
            }
            Some(Ok(report)) => report,
        };
        found.state = "ok".into();
        found.repository = report.repository.clone();
        found.rule = report.rule.as_ref().map(|rule| rule.name.clone());
        found.source = report.source;
        found.push = report.push.clone();
        found.conflicts = report.conflicts.clone();
        found.forbidden = report
            .remotes
            .iter()
            .filter(|remote| !remote.allowed && remote.url.is_some())
            .cloned()
            .collect();
        found.remotes = report.remotes.clone();
        found.hook = report.rule.as_ref().map(|_| report.hook.clone());
        let root = Path::new(&report.repository);
        let range = vec!["--remotes".to_string(), format!("--since={since}")];
        match personal::scan_commits(root, &range, &config) {
            Ok((commits, findings)) => {
                found.pushed_commits = commits;
                found.pushed_findings = findings;
            }
            Err(error) => {
                found.detail = format!(
                    "what reached a remote could not be read: {}",
                    first_line(&format!("{error:#}"))
                )
            }
        }
        let urls = urls(report);
        match &rules {
            None => found.hygiene_off = Some("the rules file has no [hygiene] table".into()),
            Some(rules) => match rules.skipped_by(&urls) {
                Some(prefix) => {
                    found.hygiene_off = Some(format!(
                        "keeps its own rules: its remote starts with {prefix} ([hygiene] skip)"
                    ))
                }
                None => found.hygiene = Some(hygiene::read(root, rules, &urls, &github, None)),
            },
        }
        found
    });
    Ok(ScanReport {
        rules_file: rules_path.display().to_string(),
        rules_file_found: file.is_some(),
        since,
        hygiene: rules,
        personal_words: config.words.len(),
        allowed_addresses: config.allow.len(),
        checkouts,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_bare_day_is_read_from_its_start() {
        assert_eq!(since_text(Some("2026-10-24")), "2026-10-24 00:00");
        assert_eq!(since_text(Some("3 days ago")), "3 days ago");
        assert_eq!(since_text(None), SINCE);
    }

    #[test]
    fn work_is_spread_and_kept_in_order() {
        let items: Vec<u32> = (0..20).collect();
        assert_eq!(
            parallel(&items, |n| n * 2),
            (0..20).map(|n| n * 2).collect::<Vec<_>>()
        );
        assert!(parallel(&Vec::<u32>::new(), |n| *n).is_empty());
    }
}
