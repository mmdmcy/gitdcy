//! `gitdcy carry`: moving work between two checkouts of one repository.

use crate::status::{print_json, OutputFormat};
use clap::{Args, Subcommand};
use gitdcy_core::carry::{self, Place, Send};
use gitdcy_core::error::Result;
use std::path::PathBuf;

#[derive(Debug, Args)]
#[command(after_help = "Examples:
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
record.")]
pub(crate) struct CarryArgs {
    #[command(subcommand)]
    command: CarryCommand,
    /// The checkout (default: the current folder).
    #[arg(long, global = true, value_name = "PATH")]
    repo: Option<PathBuf>,
    #[arg(long, value_enum, default_value = "human", global = true)]
    format: OutputFormat,
}

#[derive(Debug, Subcommand)]
enum CarryCommand {
    /// Push this checkout's work into another checkout of the repository.
    Send {
        /// The other checkout, the way git push names it (host:path, a URL, a path).
        to: String,
        /// This side's name; the work arrives under refs/carry/FROM/.
        #[arg(long, value_name = "NAME")]
        from: String,
        /// A name for this carry, so repeated carries stay apart.
        #[arg(long, value_name = "ID")]
        operation: String,
        /// A commit for the other side's place --fast-forward.
        #[arg(long, value_name = "COMMIT")]
        head: Option<String>,
        /// A local branch tip; repeat.
        #[arg(long = "tip", value_name = "COMMIT")]
        tips: Vec<String>,
        /// A stash entry, newest first; repeat.
        #[arg(long = "stash", value_name = "COMMIT")]
        stashes: Vec<String>,
        /// The working tree, uncommitted and untracked files included.
        #[arg(long)]
        worktree: bool,
        /// The working-tree commit's message.
        #[arg(long, value_name = "TEXT")]
        message: Option<String>,
    },
    /// Turn what another checkout sent into branches, stash entries and the working tree.
    Place {
        /// The sending side's name, as it sent it.
        #[arg(long, value_name = "NAME")]
        from: String,
        #[arg(long, value_name = "ID")]
        operation: String,
        /// Fast-forward this branch to the carried head instead.
        #[arg(long, value_name = "BRANCH")]
        fast_forward: Option<String>,
        /// The stash entries' message (default: carried from FROM).
        #[arg(long, value_name = "TEXT")]
        message: Option<String>,
    },
    /// Stash the uncommitted work under a ref of its own, off the stash list.
    Aside {
        /// The ref that keeps it (refs/...).
        #[arg(long = "as", value_name = "REF")]
        reference: String,
        #[arg(long, value_name = "TEXT", default_value = "set aside by gitdcy carry")]
        message: String,
    },
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
