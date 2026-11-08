//! `gitdcy carry`: moving work between two checkouts of one repository.

use crate::status::{print_json, OutputFormat};
use clap::{Arg, ArgAction, ArgMatches};
use gitdcy_core::carry::{self, Place, Send};
use gitdcy_core::error::Result;
use std::path::PathBuf;

#[derive(Debug)]
pub(crate) struct CarryArgs {
    command: CarryCommand,
    repo: Option<PathBuf>,
    format: OutputFormat,
}

#[derive(Debug)]
enum CarryCommand {
    Send {
        to: String,
        from: String,
        operation: String,
        head: Option<String>,
        tips: Vec<String>,
        stashes: Vec<String>,
        worktree: bool,
        message: Option<String>,
    },
    Place {
        from: String,
        operation: String,
        fast_forward: Option<String>,
        message: Option<String>,
    },
    Aside {
        reference: String,
        message: String,
    },
}

pub(crate) fn command() -> clap::Command {
    let option = |id: &'static str, long: &'static str, value: &'static str| {
        Arg::new(id)
            .long(long)
            .value_name(value)
            .action(ArgAction::Set)
    };
    let from = |help: &'static str| option("from", "from", "NAME").required(true).help(help);
    let operation = || option("operation", "operation", "ID").required(true);
    clap::Command::new("carry")
        .after_help(
            "Examples:
  gitdcy carry send host:code/app --from laptop --operation 1 --tip feature --stash stash@{0} --worktree
  gitdcy carry place --from laptop --operation 1                (in the receiving checkout)
  gitdcy carry send host:code/app --from laptop --operation 2 --head HEAD
  gitdcy carry place --from laptop --operation 2 --fast-forward main
  gitdcy carry aside --as refs/carry-aside/desk-1 --message 'continues on desk'

send pushes into the other checkout under refs/carry/FROM/OPERATION/: each
branch at a --tip (heads/BRANCH, or tips/COMMIT), each --stash (stash/N,
newest first), --head (head) and, with --worktree, the working tree as a
commit on top of HEAD (wip), made in a throwaway index and object folder so
this checkout is not written to. GIT_SSH_COMMAND in the environment reaches
the other side; its hooks do not run.

place makes branches only where the name is free (else carry/FROM/BRANCH,
else the ref is kept), stores each stash entry once (git stash store) and
applies the working tree only when this checkout is clean and at the commit
it was made on (else the branch carry/FROM/wip-OPERATION, else the ref is
kept). --fast-forward BRANCH instead moves that branch, when it is checked
out, to the carried head, only when it is behind and only as a fast-forward.

aside stashes the uncommitted work, untracked files included, keeps it under
the ref given and takes it off the stash list: git stash apply REF brings it
back. Nothing is forced, reset or deleted; the refs/carry/ refs stay as the
record.",
        )
        .subcommand_required(true)
        .arg_required_else_help(true)
        .subcommands([
            clap::Command::new("send")
                .about("Push this checkout's work into another checkout of the repository")
                .arg(
                    Arg::new("to")
                        .value_name("TO")
                        .required(true)
                        .action(ArgAction::Set)
                        .help(
                            "The other checkout, the way git push names it (host:path, a URL, a \
                             path)",
                        ),
                )
                .arg(from("This side's name; the work arrives under refs/carry/FROM/"))
                .arg(operation().help("A name for this carry, so repeated carries stay apart"))
                .arg(
                    option("head", "head", "COMMIT")
                        .help("A commit for the other side's place --fast-forward"),
                )
                .arg(
                    option("tips", "tip", "COMMIT")
                        .action(ArgAction::Append)
                        .help("A local branch tip; repeat"),
                )
                .arg(
                    option("stashes", "stash", "COMMIT")
                        .action(ArgAction::Append)
                        .help("A stash entry, newest first; repeat"),
                )
                .arg(
                    Arg::new("worktree")
                        .long("worktree")
                        .action(ArgAction::SetTrue)
                        .help("The working tree, uncommitted and untracked files included"),
                )
                .arg(option("message", "message", "TEXT").help("The working-tree commit's message")),
            clap::Command::new("place")
                .about(
                    "Turn what another checkout sent into branches, stash entries and the working \
                     tree",
                )
                .arg(from("The sending side's name, as it sent it"))
                .arg(operation())
                .arg(
                    option("fast_forward", "fast-forward", "BRANCH")
                        .help("Fast-forward this branch to the carried head instead"),
                )
                .arg(
                    option("message", "message", "TEXT")
                        .help("The stash entries' message (default: carried from FROM)"),
                ),
            clap::Command::new("aside")
                .about("Stash the uncommitted work under a ref of its own, off the stash list")
                .arg(
                    option("reference", "as", "REF")
                        .required(true)
                        .help("The ref that keeps it (refs/...)"),
                )
                .arg(option("message", "message", "TEXT").default_value("set aside by gitdcy carry")),
        ])
        .arg(
            option("repo", "repo", "PATH")
                .value_parser(clap::value_parser!(PathBuf))
                .global(true)
                .help("The checkout (default: the current folder)"),
        )
        .arg(crate::status::format_arg().global(true))
}

impl CarryArgs {
    pub(crate) fn from_matches(m: &ArgMatches) -> CarryArgs {
        let text = |m: &ArgMatches, id: &str| m.get_one::<String>(id).cloned();
        let given = |m: &ArgMatches, id: &str| text(m, id).unwrap_or_default();
        let many = |m: &ArgMatches, id: &str| {
            m.get_many::<String>(id)
                .map(|values| values.cloned().collect())
                .unwrap_or_default()
        };
        let (name, sub) = m.subcommand().expect("a subcommand is required");
        CarryArgs {
            command: match name {
                "send" => CarryCommand::Send {
                    to: given(sub, "to"),
                    from: given(sub, "from"),
                    operation: given(sub, "operation"),
                    head: text(sub, "head"),
                    tips: many(sub, "tips"),
                    stashes: many(sub, "stashes"),
                    worktree: sub.get_flag("worktree"),
                    message: text(sub, "message"),
                },
                "place" => CarryCommand::Place {
                    from: given(sub, "from"),
                    operation: given(sub, "operation"),
                    fast_forward: text(sub, "fast_forward"),
                    message: text(sub, "message"),
                },
                _ => CarryCommand::Aside {
                    reference: given(sub, "reference"),
                    message: given(sub, "message"),
                },
            },
            repo: m.get_one::<PathBuf>("repo").cloned(),
            format: crate::status::format(m),
        }
    }
}

/// Runs `gitdcy carry ...`; exit 1 when it could not be done (`--format
/// json` prints the envelope then too).
pub(crate) fn run(args: CarryArgs) -> Result<()> {
    let repo = args.repo.unwrap_or_else(|| PathBuf::from("."));
    let json = matches!(args.format, OutputFormat::Json);
    let failed = match args.command {
        CarryCommand::Send {
            to,
            from,
            operation,
            head,
            tips,
            stashes,
            worktree,
            message,
        } => {
            let request = Send {
                to,
                from,
                operation,
                head,
                tips,
                stashes,
                worktree,
                message,
            };
            finish(json, "carry.send", carry::send(&repo, &request), |sent| {
                vec![format!("pushed to {}", sent.prefix)]
            })?
        }
        CarryCommand::Place {
            from,
            operation,
            fast_forward,
            message,
        } => {
            let request = Place {
                from,
                operation,
                fast_forward,
                message,
            };
            finish(
                json,
                "carry.place",
                carry::place(&repo, &request),
                |placement| placement.placed.iter().map(|p| p.text.clone()).collect(),
            )?
        }
        CarryCommand::Aside { reference, message } => finish(
            json,
            "carry.aside",
            carry::aside(&repo, &reference, &message),
            |aside| {
                vec![format!(
                    "saved as {0}; git stash apply {0} brings it back",
                    aside.reference
                )]
            },
        )?,
    };
    if failed {
        std::process::exit(1);
    }
    Ok(())
}

/// Prints the result; true when the command should exit 1.
fn finish<T: serde::Serialize>(
    json: bool,
    operation: &'static str,
    result: Result<T>,
    lines: impl Fn(&T) -> Vec<String>,
) -> Result<bool> {
    if json {
        print_json(operation, &result, Vec::new())?;
        return Ok(result.is_err());
    }
    match result {
        Ok(value) => {
            for line in lines(&value) {
                println!("{line}");
            }
            Ok(false)
        }
        Err(error) => {
            eprintln!("gitdcy carry: {error:#}");
            Ok(true)
        }
    }
}
