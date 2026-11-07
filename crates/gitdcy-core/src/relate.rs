//! How two commits of one repository relate (`gitdcy relate`): the same
//! commit, one ahead of or behind the other, diverged from a common
//! ancestor, or unrelated, with the commits only on each side counted and,
//! when asked, listed.
//!
//! Read-only: git runs with hooks and fsmonitor off, without optional locks
//! and never prompts; nothing is fetched, so a commit the repository does not
//! hold is `missing`, never looked up elsewhere.

use super::{command_error, expand_home, reading_git_command};
use crate::bail;
use crate::error::Result;
use serde::Serialize;
use std::path::{Path, PathBuf};

/// Repositories read at the same time.
const WORKERS: usize = 8;

/// One answer: A against B.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Relation {
    /// The repository, as given (`~/code/app` stays `~/code/app`).
    pub repo: String,
    /// The two commits or refs, as given.
    pub a: String,
    pub b: String,
    /// Their full commit ids; null where it is not in the repository.
    pub a_commit: Option<String>,
    pub b_commit: Option<String>,
    /// `equal`, `ahead` (A holds all of B and more), `behind` (B holds all
    /// of A and more), `diverged` (each has commits the other lacks, from a
    /// common ancestor), `unrelated` (no common ancestor) or `missing` (no
    /// repository there, or A or B is not in it).
    pub relation: String,
    /// Commits in A that B lacks.
    pub ahead: u64,
    /// Commits in B that A lacks.
    pub behind: u64,
    pub detail: String,
    /// With a list asked for: up to that many commits only in A, newest first.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub only_a: Option<Vec<Commit>>,
    /// The same for B.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub only_b: Option<Vec<Commit>>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Commit {
    pub id: String,
    pub subject: String,
}

/// One question: the repository, then A and B.
pub type Request = (String, String, String);

/// `PATH<TAB>A<TAB>B` lines, as `gitdcy relate --stdin` reads them; empty
/// lines are skipped.
pub fn parse_requests(text: &str) -> Result<Vec<Request>> {
    let mut requests = Vec::new();
    for (number, line) in text.lines().enumerate() {
        let line = line.trim_end_matches('\r');
        if line.is_empty() {
            continue;
        }
        let fields = line.split('\t').collect::<Vec<_>>();
        let [repo, a, b] = fields.as_slice() else {
            bail!(
                "line {}: expected PATH, A and B separated by tabs",
                number + 1
            );
        };
        requests.push((repo.to_string(), a.to_string(), b.to_string()));
    }
    Ok(requests)
}

/// Every request, in the order given, with up to eight repositories read at
/// the same time.
pub fn relate_all(requests: &[Request], list: Option<usize>) -> Vec<Relation> {
    let mut answers = vec![Relation::default(); requests.len()];
    if requests.is_empty() {
        return answers;
    }
    let size = requests.len().div_ceil(WORKERS);
    std::thread::scope(|scope| {
        for (asked, out) in requests.chunks(size).zip(answers.chunks_mut(size)) {
            scope.spawn(move || {
                for ((repo, a, b), slot) in asked.iter().zip(out.iter_mut()) {
                    *slot = relate(repo, a, b, list);
                }
            });
        }
    });
    answers
}

/// A against B in the repository at `repo`; with `list`, up to that many
/// commits only on each side.
pub fn relate(repo: &str, a: &str, b: &str, list: Option<usize>) -> Relation {
    let mut found = Relation {
        repo: repo.to_string(),
        a: a.to_string(),
        b: b.to_string(),
        ..Relation::default()
    };
    let dir = expand_home(PathBuf::from(repo));
    if !dir.is_dir() {
        return missing(found, "no folder at that path".into());
    }
    found.a_commit = commit_id(&dir, a);
    found.b_commit = commit_id(&dir, b);
    let (Some(a_id), Some(b_id)) = (found.a_commit.clone(), found.b_commit.clone()) else {
        let absent = [(a, &found.a_commit), (b, &found.b_commit)]
            .into_iter()
            .filter(|(_, id)| id.is_none())
            .map(|(given, _)| given)
            .collect::<Vec<_>>()
            .join(" and ");
        return missing(
            found,
            format!("{absent}: no such commit in this repository"),
        );
    };
    if a_id == b_id {
        found.relation = "equal".into();
        found.detail = "the same commit".into();
        if list.is_some() {
            (found.only_a, found.only_b) = (Some(Vec::new()), Some(Vec::new()));
        }
        return found;
    }
    let counts = match git(
        &dir,
        &[
            "rev-list",
            "--left-right",
            "--count",
            &format!("{a_id}...{b_id}"),
        ],
    ) {
        Ok(counts) => counts,
        Err(error) => return missing(found, format!("{error:#}")),
    };
    let mut numbers = counts.split_whitespace().map(|n| n.parse::<u64>().ok());
    let (Some(Some(ahead)), Some(Some(behind))) = (numbers.next(), numbers.next()) else {
        return missing(found, format!("git rev-list answered {counts:?}"));
    };
    (found.ahead, found.behind) = (ahead, behind);
    let (relation, detail) = match (ahead, behind) {
        (_, 0) => ("ahead", format!("{a} is {ahead} commit(s) ahead of {b}")),
        (0, _) => ("behind", format!("{a} is {behind} commit(s) behind {b}")),
        _ if git(&dir, &["merge-base", &a_id, &b_id]).is_ok() => (
            "diverged",
            format!("diverged: {ahead} commit(s) only in {a}, {behind} only in {b}"),
        ),
        _ => (
            "unrelated",
            format!("no common history: {ahead} commit(s) only in {a}, {behind} only in {b}"),
        ),
    };
    found.relation = relation.into();
    found.detail = detail;
    if let Some(limit) = list {
        found.only_a = Some(only_in(&dir, &a_id, &b_id, limit));
        found.only_b = Some(only_in(&dir, &b_id, &a_id, limit));
    }
    found
}

fn missing(mut found: Relation, detail: String) -> Relation {
    found.relation = "missing".into();
    found.detail = detail;
    found
}

/// The full id of the commit `rev` names here, if it is one.
fn commit_id(dir: &Path, rev: &str) -> Option<String> {
    if rev.is_empty() || rev.starts_with('-') {
        return None;
    }
    git(
        dir,
        &[
            "rev-parse",
            "--verify",
            "--quiet",
            &format!("{rev}^{{commit}}"),
        ],
    )
    .ok()
    .filter(|id| !id.is_empty())
}

/// Up to `limit` commits in `tip` that `other` lacks, newest first.
fn only_in(dir: &Path, tip: &str, other: &str, limit: usize) -> Vec<Commit> {
    if limit == 0 {
        return Vec::new();
    }
    let listing = git(
        dir,
        &[
            "log",
            "--no-decorate",
            "--no-show-signature",
            "--format=%H %s",
            "-n",
            &limit.to_string(),
            tip,
            "--not",
            other,
            "--",
        ],
    )
    .unwrap_or_default();
    listing
        .lines()
        .filter_map(|line| {
            let (id, subject) = line.split_once(' ').unwrap_or((line, ""));
            (!id.is_empty()).then(|| Commit {
                id: id.to_string(),
                subject: subject.to_string(),
            })
        })
        .collect()
}

/// git's trimmed answer, read with hooks and fsmonitor off.
pub(crate) fn git(dir: &Path, args: &[&str]) -> Result<String> {
    let output = reading_git_command(dir, args).output()?;
    if !output.status.success() {
        bail!("{}", command_error("git", &output));
    }
    Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::fs;
    use std::process::Command;

    fn sh(dir: &Path, script: &str) -> String {
        let output = Command::new("sh")
            .arg("-c")
            .arg(script)
            .current_dir(dir)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_NAME", "Test")
            .env("GIT_AUTHOR_EMAIL", "test@example.invalid")
            .env("GIT_COMMITTER_NAME", "Test")
            .env("GIT_COMMITTER_EMAIL", "test@example.invalid")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{script}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_string()
    }

    fn fresh(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "gitdcy-relate-{name}-{}-{:?}",
            std::process::id(),
            std::time::SystemTime::now()
        ));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn every_relation_matches_git_and_counts_each_side() {
        let dir = fresh("all");
        sh(
            &dir,
            "git init -q -b main . && git commit -q --allow-empty -m one && git tag one && \
             git commit -q --allow-empty -m two && git tag two && \
             git switch -qc side one && git commit -q --allow-empty -m side && git tag side && \
             git switch -q --orphan alone && git commit -q --allow-empty -m alone && git tag alone && \
             git switch -q main",
        );
        let repo = dir.display().to_string();
        let shown = |a: &str, b: &str| {
            let found = relate(&repo, a, b, None);
            (found.relation, found.ahead, found.behind)
        };
        assert_eq!(shown("two", "main"), ("equal".into(), 0, 0));
        assert_eq!(shown("two", "one"), ("ahead".into(), 1, 0));
        assert_eq!(shown("one", "two"), ("behind".into(), 0, 1));
        assert_eq!(shown("side", "two"), ("diverged".into(), 1, 1));
        assert_eq!(shown("alone", "two"), ("unrelated".into(), 1, 2));
        // What git itself says, for the two checks this replaces.
        let is_ancestor = |a: &str, b: &str| {
            Command::new("git")
                .args(["merge-base", "--is-ancestor", a, b])
                .current_dir(&dir)
                .status()
                .unwrap()
                .success()
        };
        for (a, b) in [
            ("one", "two"),
            ("two", "one"),
            ("side", "two"),
            ("two", "two"),
        ] {
            let relation = relate(&repo, a, b, None).relation;
            assert_eq!(
                is_ancestor(a, b),
                matches!(relation.as_str(), "equal" | "behind"),
                "{a} {b} {relation}"
            );
        }
        let counted = sh(&dir, "git rev-list --left-right --count side...two");
        let found = relate(&repo, "side", "two", Some(10));
        assert_eq!(format!("{}\t{}", found.ahead, found.behind), counted);
        let subjects = |list: Option<Vec<Commit>>| {
            list.unwrap()
                .into_iter()
                .map(|commit| commit.subject)
                .collect::<Vec<_>>()
        };
        assert_eq!(subjects(found.only_a), vec!["side"]);
        assert_eq!(subjects(found.only_b), vec!["two"]);
        assert_eq!(
            found.a_commit.as_deref(),
            Some(sh(&dir, "git rev-parse side").as_str())
        );
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn what_is_not_there_is_missing_and_says_so() {
        let dir = fresh("missing");
        sh(
            &dir,
            "git init -q -b main . && git commit -q --allow-empty -m one",
        );
        let repo = dir.display().to_string();
        let absent = "0".repeat(40);
        let found = relate(&repo, &absent, "HEAD", None);
        assert_eq!(found.relation, "missing");
        assert!(found.b_commit.is_some());
        assert!(found.detail.starts_with(&absent), "{}", found.detail);
        assert_eq!(relate(&repo, "--all", "HEAD", None).relation, "missing");
        let nowhere = relate("/nonexistent/gitdcy-relate", "HEAD", "HEAD", None);
        assert_eq!(
            (nowhere.relation.as_str(), nowhere.detail.as_str()),
            ("missing", "no folder at that path")
        );
        // Before the first commit HEAD names nothing.
        sh(&dir, "git init -q -b main empty");
        let empty = relate(
            &dir.join("empty").display().to_string(),
            "HEAD",
            "HEAD",
            None,
        );
        assert_eq!(empty.relation, "missing");
        fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn requests_are_tab_separated_lines_answered_in_order() {
        let text = "/a b\tHEAD\tmain\n\n~/c\tx\ty\r\n";
        assert_eq!(
            parse_requests(text).unwrap(),
            vec![
                ("/a b".into(), "HEAD".into(), "main".into()),
                ("~/c".into(), "x".into(), "y".into()),
            ]
        );
        assert!(parse_requests("/a HEAD main\n").is_err());
        let answers = relate_all(
            &(0..20)
                .map(|n| (format!("/nonexistent/{n}"), "a".into(), "b".into()))
                .collect::<Vec<_>>(),
            None,
        );
        assert!(answers
            .iter()
            .enumerate()
            .all(|(n, answer)| answer.repo == format!("/nonexistent/{n}")));
    }
}
