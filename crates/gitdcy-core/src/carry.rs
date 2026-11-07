//! Moving work between two checkouts of one repository (`gitdcy carry`),
//! never overwriting or deleting anything on either side:
//!
//! - `send` pushes this checkout's work into the other checkout under
//!   `refs/carry/FROM/OPERATION/`: local branch tips (`heads/BRANCH`, or
//!   `tips/COMMIT` where no branch points at one), stash entries
//!   (`stash/N`), one commit (`head`), and the working tree as a commit on
//!   top of HEAD (`wip`), made in a throwaway index and object folder so this
//!   checkout is never written to;
//! - `place` turns what arrived into work in the receiving checkout: a
//!   branch only where its name is free (else `carry/FROM/BRANCH`, else the
//!   ref is kept), each stash entry once through `git stash store`, and the
//!   working tree applied only when the checkout is clean and at the commit
//!   the work was made on (else the branch `carry/FROM/wip-OPERATION`, else
//!   the ref is kept). `--fast-forward BRANCH` instead moves that branch, when
//!   it is checked out, to the commit `send --head` carried, only when it is
//!   behind and only as a fast-forward;
//! - `aside` sets this checkout's uncommitted work aside once it continues
//!   elsewhere: `git stash push --include-untracked`, kept under a ref of its
//!   own and taken off the stash list, so `git stash apply REF` brings it
//!   back.
//!
//! The refs under `refs/carry/` stay as the record of what arrived. Git runs
//! with hooks and fsmonitor off; nothing is forced, reset or deleted.

use super::relate::{self, git};
use super::{command_error, reading_git_command};
use crate::bail;
use crate::error::{Context, Result};
use serde::Serialize;
use std::ffi::OsString;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Stdio;

/// The receiving side's git: no hooks of the receiving checkout run.
const RECEIVE_PACK: &str = "git -c core.hooksPath=/dev/null -c core.fsmonitor=false receive-pack";
/// Who the working-tree commit is from where git has no identity configured.
const FALLBACK_NAME: &str = "gitdcy";
const FALLBACK_EMAIL: &str = "carry@gitdcy.invalid";

/// What `send` carries.
#[derive(Debug, Clone, Default)]
pub struct Send {
    /// The other checkout, the way `git push` names it (`host:path`, a URL
    /// or a path); `GIT_SSH_COMMAND` in the environment reaches it.
    pub to: String,
    pub from: String,
    pub operation: String,
    /// A commit for the other side's `--fast-forward`.
    pub head: Option<String>,
    /// Local branch tips.
    pub tips: Vec<String>,
    /// Stash entries, newest first (`git stash list` order).
    pub stashes: Vec<String>,
    /// The working tree, uncommitted and untracked files included.
    pub worktree: bool,
    /// The working-tree commit's message.
    pub message: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Sent {
    pub prefix: String,
    /// `COMMIT:REF` for every ref pushed.
    pub pushed: Vec<String>,
}

/// What `place` does.
#[derive(Debug, Clone, Default)]
pub struct Place {
    pub from: String,
    pub operation: String,
    /// Fast-forward this branch to the carried `head` instead.
    pub fast_forward: Option<String>,
    /// The stash entries' message.
    pub message: Option<String>,
}

/// One thing `place` did or left.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Placed {
    /// `branch`, `stash`, `applied`, `kept`, `fast-forwarded` or `left`.
    pub action: String,
    /// The branch, stash commit or ref.
    pub name: String,
    /// The line a person reads.
    pub text: String,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Placement {
    pub prefix: String,
    pub placed: Vec<Placed>,
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Aside {
    /// The ref that keeps the work.
    #[serde(rename = "ref")]
    pub reference: String,
    pub commit: String,
}

/// `refs/carry/FROM/OPERATION`, once both are plain names.
pub fn prefix(from: &str, operation: &str) -> Result<String> {
    for (what, value) in [("--from", from), ("--operation", operation)] {
        let plain = !value.is_empty()
            && !value.starts_with(['.', '-'])
            && !value.ends_with(".lock")
            && !value.contains("..")
            && value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte));
        if !plain {
            bail!("{what} {value:?} is not a plain name (letters, digits, . _ -)");
        }
    }
    Ok(format!("refs/carry/{from}/{operation}"))
}

/// Pushes this checkout's work into the other one under the prefix.
pub fn send(dir: &Path, request: &Send) -> Result<Sent> {
    let prefix = prefix(&request.from, &request.operation)?;
    if request.to.is_empty() || request.to.starts_with('-') {
        bail!("say where to send it: the other checkout as git push names it");
    }
    let dir = top(dir)?;
    let mut pushed = Vec::new();
    for tip in &request.tips {
        let tip = commit(&dir, tip)?;
        let branches = git(
            &dir,
            &[
                "for-each-ref",
                "--points-at",
                &tip,
                "--format=%(refname:lstrip=2)",
                "refs/heads",
            ],
        )?;
        let before = pushed.len();
        for branch in branches.lines().filter(|line| !line.is_empty()) {
            pushed.push(format!("{tip}:{prefix}/heads/{branch}"));
        }
        if pushed.len() == before {
            pushed.push(format!("{tip}:{prefix}/tips/{tip}"));
        }
    }
    for (index, stash) in request.stashes.iter().enumerate() {
        pushed.push(format!("{}:{prefix}/stash/{index}", commit(&dir, stash)?));
    }
    if let Some(head) = &request.head {
        pushed.push(format!("{}:{prefix}/head", commit(&dir, head)?));
    }
    if pushed.is_empty() && !request.worktree {
        bail!("nothing to carry: name a --head, --tip or --stash, or --worktree");
    }
    let scratch = request.worktree.then(Scratch::new).transpose()?;
    let environment = match &scratch {
        Some(scratch) => {
            let environment = scratch.environment(&dir)?;
            let message = request.message.clone().unwrap_or_else(|| {
                format!("wip({}): uncommitted work carried by gitdcy", request.from)
            });
            let wip = snapshot(&dir, &environment, &message)?;
            pushed.push(format!("{wip}:{prefix}/wip"));
            environment
        }
        None => Vec::new(),
    };
    let mut command = reading_git_command(
        &dir,
        [
            "push".to_string(),
            "--no-verify".into(),
            "--quiet".into(),
            format!("--receive-pack={RECEIVE_PACK}"),
            request.to.clone(),
        ]
        .into_iter()
        .chain(pushed.iter().cloned()),
    );
    command.envs(environment);
    let output = command.output().context("run git push")?;
    drop(scratch);
    if !output.status.success() {
        bail!("{}", command_error("git push", &output));
    }
    Ok(Sent { prefix, pushed })
}

/// Places what arrived under the prefix, or fast-forwards the branch asked.
pub fn place(dir: &Path, request: &Place) -> Result<Placement> {
    let prefix = prefix(&request.from, &request.operation)?;
    let dir = top(dir)?;
    let placed = match &request.fast_forward {
        Some(branch) => vec![fast_forward(&dir, &prefix, branch)?],
        None => place_work(&dir, &prefix, request)?,
    };
    Ok(Placement { prefix, placed })
}

/// Sets the uncommitted work aside under `reference`, off the stash list.
pub fn aside(dir: &Path, reference: &str, message: &str) -> Result<Aside> {
    let dir = top(dir)?;
    if !reference.starts_with("refs/")
        || git(&dir, &["check-ref-format", reference]).is_err()
        || reference.starts_with("refs/heads/")
        || reference == "refs/stash"
    {
        bail!("{reference:?} is not a ref of its own (refs/..., not a branch or refs/stash)");
    }
    let stash = || git(&dir, &["rev-parse", "-q", "--verify", "refs/stash"]).ok();
    let before = stash();
    git(
        &dir,
        &["stash", "push", "--include-untracked", "-q", "-m", message],
    )?;
    let Some(commit) = stash().filter(|after| Some(after) != before.as_ref()) else {
        bail!("no local changes to set aside");
    };
    // Kept first; it leaves the stash list only once the ref holds it.
    git(&dir, &["update-ref", "-m", message, reference, &commit, ""])
        .with_context(|| format!("{reference} could not keep it; it stays as stash@{{0}}"))?;
    if stash().as_ref() == Some(&commit) {
        git(&dir, &["stash", "drop", "-q", "stash@{0}"])?;
    }
    Ok(Aside {
        reference: reference.to_string(),
        commit,
    })
}

/// Branches where the name is free, each stash entry once, and the working
/// tree only where the checkout is clean at the commit it was made on.
fn place_work(dir: &Path, prefix: &str, request: &Place) -> Result<Vec<Placed>> {
    let from = &request.from;
    let mut placed = Vec::new();
    let heads = git(
        dir,
        &[
            "for-each-ref",
            "--format=%(refname)",
            &format!("{prefix}/heads/"),
        ],
    )?;
    for reference in heads.lines().filter(|line| !line.is_empty()) {
        let branch = &reference[prefix.len() + "/heads/".len()..];
        let commit = git(dir, &["rev-parse", reference])?;
        let carried = format!("carry/{from}/{branch}");
        if create(dir, &format!("refs/heads/{branch}"), &commit) {
            placed.push(line("branch", branch, format!("branch {branch}")));
        } else if git(
            dir,
            &[
                "rev-parse",
                "-q",
                "--verify",
                &format!("refs/heads/{branch}"),
            ],
        )
        .is_ok_and(|there| there == commit)
        {
            // Already there under its own name.
            continue;
        } else if create(dir, &format!("refs/heads/{carried}"), &commit) {
            placed.push(line("branch", &carried, format!("branch {carried}")));
        } else {
            placed.push(line("kept", reference, format!("kept {reference}")));
        }
    }
    let mut entries = git(
        dir,
        &[
            "for-each-ref",
            "--format=%(refname:lstrip=-1)",
            &format!("{prefix}/stash/"),
        ],
    )?
    .lines()
    .filter_map(|index| index.parse::<u64>().ok())
    .collect::<Vec<_>>();
    // Oldest first, so the newest ends on top as it was.
    entries.sort_unstable_by(|a, b| b.cmp(a));
    let message = request
        .message
        .clone()
        .unwrap_or_else(|| format!("carried from {from}"));
    for index in entries {
        let reference = format!("{prefix}/stash/{index}");
        let commit = git(dir, &["rev-parse", &reference])?;
        let listed = git(dir, &["stash", "list", "--format=%H"]).unwrap_or_default();
        if listed.lines().any(|line| line == commit) {
            continue;
        }
        if git(dir, &["stash", "store", "-m", &message, &commit]).is_ok() {
            placed.push(line("stash", &commit, format!("stash {commit}")));
        } else {
            placed.push(line("kept", &reference, format!("kept {reference}")));
        }
    }
    let wip_ref = format!("{prefix}/wip");
    if let Ok(wip) = git(
        dir,
        &[
            "rev-parse",
            "-q",
            "--verify",
            &format!("{wip_ref}^{{commit}}"),
        ],
    ) {
        let made_on = git(dir, &["rev-parse", "-q", "--verify", &format!("{wip}^")]).ok();
        let here = git(dir, &["rev-parse", "-q", "--verify", "HEAD"]).ok();
        let clean = git(dir, &["status", "--porcelain"]).is_ok_and(|status| status.is_empty());
        let branch = format!("carry/{from}/wip-{}", request.operation);
        if here == made_on && clean && apply(dir, &wip) {
            placed.push(line("applied", "", "applied the working tree".into()));
        } else if create(dir, &format!("refs/heads/{branch}"), &wip) {
            placed.push(line(
                "branch",
                &branch,
                format!("branch {branch} (successor checkout differs)"),
            ));
        } else {
            placed.push(line("kept", &wip_ref, format!("kept {wip_ref}")));
        }
    }
    Ok(placed)
}

/// The branch, when checked out, to the carried `head` when it is behind it.
fn fast_forward(dir: &Path, prefix: &str, branch: &str) -> Result<Placed> {
    let carried = format!("{prefix}/head");
    let head = git(
        dir,
        &[
            "rev-parse",
            "-q",
            "--verify",
            &format!("{carried}^{{commit}}"),
        ],
    )
    .map_err(|_| crate::err!("nothing was carried as {carried}"))?;
    let left = |text: String| line("left", branch, text);
    let on = git(dir, &["symbolic-ref", "-q", "--short", "HEAD"]).unwrap_or_default();
    if on != branch {
        return Ok(left(format!("not on {branch}; left as is")));
    }
    let relation = relate::relate(&dir.display().to_string(), &head, "HEAD", None).relation;
    Ok(match relation.as_str() {
        "equal" | "behind" => left("already ahead; left as is".into()),
        "ahead" => {
            if git(dir, &["merge", "--ff-only", "--quiet", &head]).is_ok() {
                line(
                    "fast-forwarded",
                    branch,
                    format!("fast-forwarded to {}", &head[..8]),
                )
            } else {
                left(format!(
                    "fast-forward refused (local changes in the way); {carried} keeps the commit"
                ))
            }
        }
        _ => left(format!(
            "diverged; left as is, {carried} keeps the device commit"
        )),
    })
}

fn line(action: &str, name: &str, text: String) -> Placed {
    Placed {
        action: action.into(),
        name: name.into(),
        text,
    }
}

/// Creates `reference` at `commit` only when it does not exist yet.
fn create(dir: &Path, reference: &str, commit: &str) -> bool {
    git(dir, &["update-ref", reference, commit, ""]).is_ok()
}

/// `git diff --binary HEAD WIP | git apply`: the working tree only, all or
/// nothing.
fn apply(dir: &Path, wip: &str) -> bool {
    let Ok(diff) = reading_git_command(
        dir,
        [
            "diff",
            "--binary",
            "--no-color",
            "--no-ext-diff",
            "--no-textconv",
            "HEAD",
            wip,
        ],
    )
    .output() else {
        return false;
    };
    if !diff.status.success() {
        return false;
    }
    let mut command = reading_git_command(dir, ["apply", "--whitespace=nowarn"]);
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    let Ok(mut child) = command.spawn() else {
        return false;
    };
    let written = child
        .stdin
        .take()
        .is_some_and(|mut stdin| stdin.write_all(&diff.stdout).is_ok());
    child.wait().is_ok_and(|status| status.success()) && written
}

/// The checkout's top folder.
fn top(dir: &Path) -> Result<PathBuf> {
    git(dir, &["rev-parse", "--show-toplevel"])
        .map(PathBuf::from)
        .with_context(|| format!("{} is not a git checkout", dir.display()))
}

/// The full id of the commit `rev` names.
fn commit(dir: &Path, rev: &str) -> Result<String> {
    if rev.starts_with('-') {
        bail!("{rev:?} is not a commit");
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
    .with_context(|| format!("{rev} is not a commit here"))
}

/// A throwaway index and object folder, removed when dropped.
struct Scratch(PathBuf);

impl Scratch {
    fn new() -> Result<Self> {
        super::facts::scratch_folder()
            .map(Self)
            .context("could not make a private temporary folder")
    }

    /// git's environment for writing into the scratch only, reading the
    /// checkout's own objects alongside.
    fn environment(&self, dir: &Path) -> Result<Vec<(&'static str, OsString)>> {
        let path = |name: &str| {
            git(
                dir,
                &["rev-parse", "--path-format=absolute", "--git-path", name],
            )
        };
        let (index, objects) = (path("index")?, path("objects")?);
        let scratch_index = self.0.join("index");
        if Path::new(&index).is_file() {
            fs::copy(&index, &scratch_index).context("copy the index")?;
        }
        let scratch_objects = self.0.join("objects");
        fs::create_dir(&scratch_objects).context("make the scratch object folder")?;
        Ok(vec![
            ("GIT_INDEX_FILE", scratch_index.into_os_string()),
            ("GIT_OBJECT_DIRECTORY", scratch_objects.into_os_string()),
            ("GIT_ALTERNATE_OBJECT_DIRECTORIES", objects.into()),
        ])
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

/// The working tree as a commit on top of HEAD (none before the first
/// commit), written into the scratch only.
fn snapshot(dir: &Path, environment: &[(&str, OsString)], message: &str) -> Result<String> {
    let run = |args: &[&str], identity: &[(&str, &str)]| -> Result<String> {
        let mut command = reading_git_command(dir, args);
        command.envs(environment.iter().map(|(key, value)| (key, value)));
        command.envs(identity.iter().copied());
        let output = command.output()?;
        if !output.status.success() {
            bail!("{}", command_error("git", &output));
        }
        Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
    };
    run(&["add", "-A"], &[])?;
    let tree = run(&["write-tree"], &[])?;
    let identity: &[(&str, &str)] = if run(&["var", "GIT_AUTHOR_IDENT"], &[]).is_ok() {
        &[]
    } else {
        &[
            ("GIT_AUTHOR_NAME", FALLBACK_NAME),
            ("GIT_AUTHOR_EMAIL", FALLBACK_EMAIL),
            ("GIT_COMMITTER_NAME", FALLBACK_NAME),
            ("GIT_COMMITTER_EMAIL", FALLBACK_EMAIL),
        ]
    };
    if run(&["rev-parse", "-q", "--verify", "HEAD"], &[]).is_ok() {
        run(
            &["commit-tree", &tree, "-p", "HEAD", "-m", message],
            identity,
        )
    } else {
        run(&["commit-tree", &tree, "-m", message], identity)
    }
}
