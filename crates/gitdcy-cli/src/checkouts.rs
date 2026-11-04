//! `gitdcy checkouts`: checkouts against their upstream, and the safe
//! fast-forward.

use crate::status::{print_json, OutputFormat};
use anyhow::Result;
use clap::{Args, Subcommand};
use gitdcy_core::checkouts::{self, Checkout, Options};
use gitdcy_core::facts::Facts;
use gitdcy_core::load_or_discover_manifest;
use serde::Serialize;

#[derive(Debug, Args)]
#[command(after_help = "Examples:
  gitdcy checkouts                         every repository in the workspace
  gitdcy checkouts ~/code/app ~/code/lib   these checkouts
  gitdcy checkouts --no-fetch --format json
  gitdcy checkouts --no-fetch --facts --format json /abs/repo
  gitdcy checkouts update ~/code/app       fast-forward it if that is safe

States: current, behind, ahead, diverged, changed (local changes only),
missing, no-upstream, error. update fast-forwards only a checkout that is
behind with no local changes and no local commits; it never makes a merge
commit, rebases, forces or touches local changes. A leading ~/ is your home,
also on Windows. Reading a checkout's state never runs its hooks or
fsmonitor; only the fetch and update's fast-forward run git as configured.

--facts adds what a tool moving the checkout to another device needs, as
`facts` in the JSON: the full head id, stash ids, commits on no remote,
notable ignored files by name, fingerprints of the local changes and the
working tree's id (computed without writing to the repository).")]
pub(crate) struct CheckoutsArgs {
    #[command(subcommand)]
    command: Option<CheckoutsCommand>,
    /// Checkouts to read (default: every repository in the workspace).
    #[arg(value_name = "PATH")]
    paths: Vec<String>,
    /// Do not fetch first: show what the last fetch knew.
    #[arg(long, global = true)]
    no_fetch: bool,
    /// Also report what moving each checkout needs (slower: hashes local changes).
    #[arg(long, global = true)]
    facts: bool,
    #[arg(long, value_enum, default_value = "human", global = true)]
    format: OutputFormat,
}

#[derive(Debug, Subcommand)]
enum CheckoutsCommand {
    /// Fast-forward the checkouts that are behind and have no local changes or commits.
    Update {
        /// Checkouts to update (default: every repository in the workspace).
        #[arg(value_name = "PATH")]
        paths: Vec<String>,
    },
}

#[derive(Serialize)]
struct Report {
    checkouts: Vec<Checkout>,
}

/// Runs `gitdcy checkouts ...`; exit 1 only when the command itself failed
/// (`--format json` prints the envelope then too).
pub(crate) fn run(args: CheckoutsArgs) -> Result<()> {
    let (operation, update, paths) = match args.command {
        None => ("checkouts", false, args.paths),
        Some(CheckoutsCommand::Update { paths }) => ("checkouts.update", true, paths),
    };
    let options = Options {
        fetch: !args.no_fetch,
        update,
        facts: args.facts,
    };
    let report = given_or_workspace(paths).map(|paths| Report {
        checkouts: checkouts::checkouts(&paths, options),
    });
    if matches!(args.format, OutputFormat::Json) {
        let warnings = report
            .as_ref()
            .map(|r| warnings(&r.checkouts))
            .unwrap_or_default();
        print_json(operation, &report, warnings)?;
        if report.is_err() {
            std::process::exit(1);
        }
        return Ok(());
    }
    print_human(&report?.checkouts);
    Ok(())
}

pub(crate) fn given_or_workspace(paths: Vec<String>) -> Result<Vec<String>> {
    if !paths.is_empty() {
        return Ok(paths);
    }
    Ok(load_or_discover_manifest()?
        .repos
        .into_iter()
        .filter(|repo| repo.enabled)
        .map(|repo| repo.path.display().to_string())
        .collect())
}

/// What could not be read as asked.
fn warnings(list: &[Checkout]) -> Vec<String> {
    list.iter()
        .filter(|c| matches!(c.state.as_str(), "missing" | "error") || !c.fetch_error.is_empty())
        .map(|c| format!("{}: {}", c.path, c.detail))
        .collect()
}

fn print_human(list: &[Checkout]) {
    println!("{}", checkouts::summary(list));
    let width = list.iter().map(|c| c.path.len()).max().unwrap_or(0).min(48);
    for c in list {
        println!("  {:<width$}  {:<11} {}", c.path, c.state, c.detail);
        if !c.upstream.is_empty() {
            println!("  {:<width$}  {:<11} newest: {}", "", "", c.upstream);
        }
        if let Some(facts) = &c.facts {
            println!("  {:<width$}  {:<11} {}", "", "", facts_line(facts));
        }
    }
}

/// The facts in one line for a person; the JSON has them all.
fn facts_line(facts: &Facts) -> String {
    let head = facts
        .head
        .as_deref()
        .map_or("no commit yet", |h| &h[..h.len().min(12)]);
    let mut parts = vec![format!("head {head}")];
    if let Some(error) = &facts.status_error {
        parts.push(error.clone());
    } else if facts.changed > 0 {
        parts.push(format!(
            "{} changed ({} untracked)",
            facts.changed, facts.untracked
        ));
    }
    if !facts.stashes.is_empty() {
        parts.push(format!("{} stash(es)", facts.stashes.len()));
    }
    if facts.unpushed_commits > 0 {
        parts.push(format!("{} commit(s) on no remote", facts.unpushed_commits));
    }
    if !facts.ignored_state.is_empty() {
        parts.push(format!("ignored: {}", facts.ignored_state.join(", ")));
    }
    if let Some(tree) = &facts.tree {
        parts.push(format!("tree {}", &tree[..tree.len().min(12)]));
    }
    parts.join("; ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn facts_appear_in_the_json_only_when_asked() {
        let plain = serde_json::to_value(Checkout::default()).unwrap();
        assert!(plain.get("facts").is_none(), "{plain}");
        let asked = serde_json::to_value(Checkout {
            facts: Some(Facts {
                head: Some("a".repeat(40)),
                stashes: vec!["b".repeat(40)],
                ..Facts::default()
            }),
            ..Checkout::default()
        })
        .unwrap();
        let facts = &asked["facts"];
        assert_eq!(facts["head"], "a".repeat(40));
        assert_eq!(facts["stashes"][0], "b".repeat(40));
        assert!(facts["fingerprints"].is_null() && facts["tree"].is_null());
        for key in [
            "branch",
            "upstream",
            "ahead",
            "behind",
            "origin",
            "status_error",
            "changed",
            "untracked",
            "unpushed_commits",
            "unpushed_tips",
            "ignored_state",
            "carry_trees",
        ] {
            assert!(facts.get(key).is_some(), "facts.{key} is missing");
        }
        let line = facts_line(&Facts {
            head: Some("0123456789abcdef".into()),
            changed: 2,
            untracked: 1,
            unpushed_commits: 3,
            ..Facts::default()
        });
        assert_eq!(
            line,
            "head 0123456789ab; 2 changed (1 untracked); 3 commit(s) on no remote"
        );
    }

    #[test]
    fn only_unreadable_checkouts_and_failed_fetches_are_warnings() {
        let list = vec![
            Checkout {
                path: "~/a".into(),
                state: "behind".into(),
                detail: "2 commit(s) behind".into(),
                ..Checkout::default()
            },
            Checkout {
                path: "~/b".into(),
                state: "missing".into(),
                detail: "no folder at that path".into(),
                ..Checkout::default()
            },
            Checkout {
                path: "~/c".into(),
                state: "current".into(),
                fetch_error: "could not resolve host".into(),
                detail: "up to date; the fetch failed".into(),
                ..Checkout::default()
            },
        ];
        assert_eq!(
            warnings(&list),
            vec![
                "~/b: no folder at that path".to_string(),
                "~/c: up to date; the fetch failed".to_string()
            ]
        );
    }
}
