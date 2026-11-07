//! `gitdcy relate`: how two commits of a repository relate.

use crate::status::{print_json, OutputFormat};
use clap::{Arg, ArgAction, ArgMatches};
use gitdcy_core::bail;
use gitdcy_core::error::{Context, Result};
use gitdcy_core::relate::{self, Relation};
use serde::Serialize;
use std::io::Read;

#[derive(Debug)]
pub(crate) struct RelateArgs {
    commits: Vec<String>,
    repo: Option<String>,
    stdin: bool,
    list: Option<usize>,
    format: OutputFormat,
}

#[derive(Serialize)]
struct Report {
    relations: Vec<Relation>,
}

pub(crate) fn command() -> clap::Command {
    clap::Command::new("relate")
        .after_help(
            "Examples:
  gitdcy relate main origin/main            this repository's main against its upstream
  gitdcy relate --repo ~/code/app 1a2b3c HEAD --format json
  printf '%s\\t%s\\t%s\\n' ~/code/app 1a2b3c HEAD | gitdcy relate --stdin --format json

Relations: equal, ahead (A holds all of B and more), behind (B holds all of A
and more), diverged (each has commits the other lacks), unrelated (no common
history) and missing (no repository there, or A or B is not in it). ahead
and behind count the commits only in A and only in B. Read-only: nothing is
fetched, and git runs without the repository's hooks or fsmonitor.",
        )
        .arg(
            Arg::new("commits")
                .value_name("COMMIT")
                .num_args(0..=2)
                .action(ArgAction::Append)
                .help("A, then B: commits or refs"),
        )
        .arg(
            Arg::new("repo")
                .long("repo")
                .value_name("PATH")
                .action(ArgAction::Set)
                .help("The repository (default: the current folder)"),
        )
        .arg(
            Arg::new("stdin")
                .long("stdin")
                .action(ArgAction::SetTrue)
                .conflicts_with_all(["commits", "repo"])
                .help("Read PATH<TAB>A<TAB>B lines from standard input instead, answered in order"),
        )
        .arg(
            Arg::new("list")
                .long("list")
                .value_name("N")
                .value_parser(clap::value_parser!(usize))
                .action(ArgAction::Set)
                .help("Also list up to N commits only in A and only in B, newest first"),
        )
        .arg(crate::status::format_arg())
}

impl RelateArgs {
    pub(crate) fn from_matches(m: &ArgMatches) -> RelateArgs {
        RelateArgs {
            commits: m
                .get_many::<String>("commits")
                .map(|commits| commits.cloned().collect())
                .unwrap_or_default(),
            repo: m.get_one::<String>("repo").cloned(),
            stdin: m.get_flag("stdin"),
            list: m.get_one::<usize>("list").copied(),
            format: crate::status::format(m),
        }
    }
}

/// Runs `gitdcy relate ...`; exit 1 only when the command itself failed
/// (`--format json` prints the envelope then too).
pub(crate) fn run(args: RelateArgs) -> Result<()> {
    let report = requests(&args).map(|requests| Report {
        relations: relate::relate_all(&requests, args.list),
    });
    if matches!(args.format, OutputFormat::Json) {
        let warnings = report
            .as_ref()
            .map(|report| {
                report
                    .relations
                    .iter()
                    .filter(|relation| relation.relation == "missing")
                    .map(|relation| format!("{}: {}", relation.repo, relation.detail))
                    .collect()
            })
            .unwrap_or_default();
        print_json("relate", &report, warnings)?;
        if report.is_err() {
            std::process::exit(1);
        }
        return Ok(());
    }
    for relation in report?.relations {
        println!(
            "{}: {} {}",
            relation.repo, relation.relation, relation.detail
        );
        for (side, commits) in [
            (&relation.a, &relation.only_a),
            (&relation.b, &relation.only_b),
        ] {
            for commit in commits.iter().flatten() {
                println!(
                    "  only in {side}: {} {}",
                    &commit.id[..commit.id.len().min(12)],
                    commit.subject
                );
            }
        }
    }
    Ok(())
}

fn requests(args: &RelateArgs) -> Result<Vec<relate::Request>> {
    if args.stdin {
        let mut text = String::new();
        std::io::stdin()
            .read_to_string(&mut text)
            .context("reading the questions from standard input")?;
        return relate::parse_requests(&text);
    }
    let [a, b] = args.commits.as_slice() else {
        bail!("name two commits or refs (A and B), or pass --stdin");
    };
    let repo = args.repo.clone().unwrap_or_else(|| ".".to_string());
    Ok(vec![(repo, a.clone(), b.clone())])
}
