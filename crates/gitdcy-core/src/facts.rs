//! What a tool that moves or retires a checkout needs to know so that no work
//! is lost (`gitdcy checkouts --facts`): the exact commit, branch and
//! upstream, local changes counted file by file, stashes, commits that no
//! remote holds, ignored files that usually cannot be rebuilt (by name only),
//! content fingerprints of the local changes, and the id of the whole working
//! tree.
//!
//! Read-only: every git call runs with hooks and fsmonitor off and without
//! optional locks, and the working-tree id is computed in a throwaway index
//! and object folder, so the repository is never written to. A git read that
//! answers more than 1 MiB or takes longer than 8 seconds counts as failed;
//! each fact then falls back as its field says. The facts are read on their
//! own, so a failure in the rest of the checkout's report does not hide them.

use super::reading_git_command;
use serde::Serialize;
use std::collections::BTreeSet;
use std::env;
use std::fs::{self, File};
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const OUTPUT_LIMIT: usize = 1024 * 1024;
const TIMEOUT: Duration = Duration::from_secs(8);
/// Past this many changed files the fingerprints are left out...
const MAX_FINGERPRINTS: usize = 4096;
/// ...and past this many bytes to hash.
const MAX_FINGERPRINT_BYTES: u64 = 1024 * 1024 * 1024;

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Facts {
    /// Full id of the commit checked out; null before the first commit.
    pub head: Option<String>,
    /// `git symbolic-ref --short HEAD`; null when no branch is checked out.
    pub branch: Option<String>,
    /// The branch it follows (`origin/main`); null when none.
    pub upstream: Option<String>,
    /// Commits on HEAD that the upstream lacks; 0 without an upstream.
    pub ahead: u64,
    /// Commits on the upstream that HEAD lacks; 0 without an upstream.
    pub behind: u64,
    /// `origin`'s URL without user or password, the rest as configured; null
    /// when there is no `origin` or its URL is not safe to show.
    pub origin: Option<String>,
    /// Why `git status` failed; `changed` and `untracked` are then 0 and
    /// there are no fingerprints.
    pub status_error: Option<String>,
    /// Entries `git status --untracked-files=all` lists, so each untracked
    /// file counts on its own.
    pub changed: u64,
    /// Of those, the untracked files.
    pub untracked: u64,
    /// Commit ids of the stash entries, newest first.
    pub stashes: Vec<String>,
    /// Commits on local branches that no remote-tracking branch holds.
    pub unpushed_commits: u64,
    /// The local branch tips among those commits, sorted.
    pub unpushed_tips: Vec<String>,
    /// Ignored files and folders that usually hold what cannot be rebuilt
    /// (`.env`, local databases, keys), by name; never their contents.
    pub ignored_state: Vec<String>,
    /// `DIGEST PATH` for every file that differs from HEAD (changed, added or
    /// untracked; deletions lose no work and are left out), with DIGEST
    /// `sha256:HEX` of its contents, `link:HEX` of a symlink's target or
    /// `dir`. Null when there are no local changes, `git status` failed, a
    /// path is unusual, or past 4096 files or 1 GiB.
    pub fingerprints: Option<Vec<String>>,
    /// The tree id `git add -A && git write-tree` would give, so identical
    /// working trees give identical ids on every device. Null when there are
    /// no fingerprints.
    pub tree: Option<String>,
    /// Tree ids of the commits under `refs/carry/` (working trees another
    /// device's tool parked here), sorted.
    pub carry_trees: Vec<String>,
}

/// Every fact about the checkout at `dir` (its top folder).
pub fn read(dir: &Path) -> Facts {
    let text = |args: &[&str]| git_text(dir, args, &[]);
    let status = text(&["status", "--porcelain=v1", "--untracked-files=all"]);
    let upstream = text(&[
        "rev-parse",
        "--abbrev-ref",
        "--symbolic-full-name",
        "@{upstream}",
    ])
    .ok();
    let (ahead, behind) = upstream
        .as_ref()
        .and_then(|_| text(&["rev-list", "--left-right", "--count", "@{upstream}...HEAD"]).ok())
        .and_then(|counts| {
            let mut values = counts.split_whitespace().map(|n| n.parse::<u64>().ok());
            let behind = values.next()??;
            let ahead = values.next()??;
            Some((ahead, behind))
        })
        .unwrap_or((0, 0));
    let (changed, untracked, status_error) = match status {
        Ok(listing) => {
            let lines = listing.lines().filter(|line| !line.is_empty());
            let (mut changed, mut untracked) = (0, 0);
            for line in lines {
                changed += 1;
                untracked += u64::from(line.starts_with("??"));
            }
            (changed, untracked, None)
        }
        Err(error) => (0, 0, Some(error)),
    };
    let (unpushed_commits, unpushed_tips) = unpushed(dir);
    let fingerprints = if changed > 0 && status_error.is_none() {
        fingerprints(dir)
    } else {
        None
    };
    let tree = fingerprints.as_ref().and_then(|_| working_tree_id(dir));
    Facts {
        head: text(&["rev-parse", "HEAD"]).ok(),
        branch: text(&["symbolic-ref", "--short", "HEAD"]).ok(),
        upstream,
        ahead,
        behind,
        origin: text(&["remote", "get-url", "origin"])
            .ok()
            .and_then(|url| without_credentials(&url)),
        status_error,
        changed,
        untracked,
        stashes: lines(&text(&["stash", "list", "--format=%H"]).unwrap_or_default()),
        unpushed_commits,
        unpushed_tips,
        ignored_state: notable_ignored_state(dir),
        fingerprints,
        tree,
        carry_trees: text(&["for-each-ref", "--format=%(tree)", "refs/carry/"])
            .map(|trees| {
                trees
                    .lines()
                    .filter(|tree| is_object_id(tree))
                    .map(str::to_string)
                    .collect::<BTreeSet<_>>()
                    .into_iter()
                    .collect()
            })
            .unwrap_or_default(),
    }
}

/// Commits reachable from local branches but from no remote-tracking ref,
/// and the branch tips that hold them: these exist nowhere but here.
fn unpushed(dir: &Path) -> (u64, Vec<String>) {
    let Ok(listing) = git_text(dir, &["rev-list", "--branches", "--not", "--remotes"], &[]) else {
        return (0, Vec::new());
    };
    let commits = listing
        .lines()
        .filter(|line| !line.is_empty())
        .collect::<BTreeSet<_>>();
    if commits.is_empty() {
        return (0, Vec::new());
    }
    let tips = git_text(
        dir,
        &["for-each-ref", "--format=%(objectname)", "refs/heads"],
        &[],
    )
    .map(|tips| {
        tips.lines()
            .filter(|tip| commits.contains(tip))
            .map(str::to_string)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect()
    })
    .unwrap_or_default();
    (commits.len() as u64, tips)
}

/// Ignored files that usually hold irreplaceable local state (secrets, local
/// databases, private notes) rather than rebuildable output, in git's order.
fn notable_ignored_state(dir: &Path) -> Vec<String> {
    let args = [
        "ls-files",
        "--others",
        "--ignored",
        "--exclude-standard",
        "--directory",
        "--no-empty-directory",
    ];
    let Ok(listing) = git_text(dir, &args, &[]) else {
        return Vec::new();
    };
    listing
        .lines()
        .map(|line| line.trim_end_matches('/'))
        .filter(|path| !path.is_empty() && is_notable_ignored_path(path))
        .map(str::to_string)
        .collect()
}

/// Whether an ignored path is likely state that cannot be rebuilt: `.env`
/// files (not examples), databases, keys, `*.private.*` notes and a few
/// well-known data folders, outside build and dependency folders.
fn is_notable_ignored_path(path: &str) -> bool {
    const REBUILDABLE: &[&str] = &[
        ".git",
        ".cache",
        ".dart_tool",
        ".gradle",
        ".venv",
        "__pycache__",
        "build",
        "dist",
        "node_modules",
        "target",
        "vendor",
        "coverage",
        "ephemeral",
        ".idea",
        ".next",
        ".nuxt",
        ".astro",
        ".turbo",
        ".svelte-kit",
        ".terraform",
        ".expo",
        "Pods",
        "DerivedData",
    ];
    // A folder ending in `-partial` is a copy still being written (rsync's
    // `--partial-dir`), never state of its own.
    if path
        .split('/')
        .any(|part| REBUILDABLE.contains(&part) || part.ends_with("-partial"))
    {
        return false;
    }
    let name = path.rsplit('/').next().unwrap_or(path);
    let lower = name.to_ascii_lowercase();
    (lower.starts_with(".env") && !lower.ends_with(".example") && !lower.ends_with(".sample"))
        || [
            ".db",
            ".sqlite",
            ".sqlite3",
            ".pem",
            ".key",
            ".p12",
            ".jks",
            ".keystore",
        ]
        .iter()
        .any(|suffix| lower.ends_with(suffix))
        || lower.contains(".private.")
        || matches!(
            lower.as_str(),
            "pb_data" | "secrets" | ".secrets" | "credentials.json"
        )
}

/// Content digests of the files that differ from HEAD; see
/// [`Facts::fingerprints`].
fn fingerprints(dir: &Path) -> Option<Vec<String>> {
    let changed = git_text(dir, &["diff", "--name-only", "-z", "HEAD"], &[]).ok()?;
    let untracked = git_text(
        dir,
        &["ls-files", "--others", "--exclude-standard", "-z"],
        &[],
    )
    .ok()?;
    let paths = changed
        .split('\0')
        .chain(untracked.split('\0'))
        .filter(|path| !path.is_empty())
        .collect::<BTreeSet<_>>();
    if paths.len() > MAX_FINGERPRINTS {
        return None;
    }
    let mut hashed = 0_u64;
    let mut fingerprints = Vec::with_capacity(paths.len());
    for path in paths {
        if !plain_relative_path(path.trim_end_matches('/')) {
            return None;
        }
        let full = dir.join(path);
        let Ok(metadata) = fs::symlink_metadata(&full) else {
            continue;
        };
        let digest = if metadata.is_dir() {
            "dir".to_string()
        } else if metadata.file_type().is_symlink() {
            let target = fs::read_link(&full).ok()?;
            format!("link:{}", sha256_hex(target.as_os_str().as_encoded_bytes()))
        } else {
            hashed = hashed.saturating_add(metadata.len());
            if hashed > MAX_FINGERPRINT_BYTES {
                return None;
            }
            format!("sha256:{}", sha256_file(&full).ok()?)
        };
        fingerprints.push(format!("{digest} {path}"));
    }
    Some(fingerprints)
}

/// The tree id `git add -A && git write-tree` would produce, computed with a
/// throwaway index and object folder so the repository itself is never
/// written to.
fn working_tree_id(dir: &Path) -> Option<String> {
    let path = |name: &str| {
        git_text(
            dir,
            &["rev-parse", "--path-format=absolute", "--git-path", name],
            &[],
        )
        .ok()
    };
    let (index, objects) = (path("index")?, path("objects")?);
    let scratch = scratch_folder()?;
    let tree = (|| {
        let scratch_index = scratch.join("index");
        if Path::new(&index).is_file() {
            fs::copy(&index, &scratch_index).ok()?;
        }
        let scratch_objects = scratch.join("objects");
        fs::create_dir(&scratch_objects).ok()?;
        let environment = [
            ("GIT_INDEX_FILE", scratch_index.into_os_string()),
            ("GIT_OBJECT_DIRECTORY", scratch_objects.into_os_string()),
            ("GIT_ALTERNATE_OBJECT_DIRECTORIES", objects.into()),
        ];
        git_text(dir, &["add", "-A"], &environment).ok()?;
        git_text(dir, &["write-tree"], &environment)
            .ok()
            .filter(|tree| is_object_id(tree))
    })();
    let _ = fs::remove_dir_all(&scratch);
    tree
}

/// A new private folder under the temporary directory.
pub(crate) fn scratch_folder() -> Option<PathBuf> {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let folder = env::temp_dir().join(format!(
        "gitdcy-tree-{}-{nanos}-{}",
        std::process::id(),
        COUNTER.fetch_add(1, Ordering::Relaxed)
    ));
    let mut builder = fs::DirBuilder::new();
    #[cfg(unix)]
    std::os::unix::fs::DirBuilderExt::mode(&mut builder, 0o700);
    builder.create(&folder).ok()?;
    Some(folder)
}

/// `origin`'s URL with any user and password removed and the rest exactly as
/// written; None when it is empty, too long, has control characters or
/// spaces, or is a `user:password@host:path` form that cannot be cleaned.
fn without_credentials(url: &str) -> Option<String> {
    if url.is_empty()
        || url.len() > 2048
        || url
            .chars()
            .any(|character| character.is_control() || character.is_whitespace())
    {
        return None;
    }
    // scheme://[user[:password]@]host/path: drop the user part.
    if let Some((scheme, rest)) = url.split_once("://") {
        if !scheme.is_empty()
            && scheme
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"+.-".contains(&byte))
        {
            let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
            let (authority, path) = rest.split_at(end);
            let host = authority
                .rsplit_once('@')
                .map_or(authority, |(_, host)| host);
            return Some(format!("{scheme}://{host}{path}"));
        }
    }
    if let Some((user, _)) = url.split_once('@') {
        if user.is_empty() || user.contains(':') {
            return None;
        }
    }
    Some(url.to_string())
}

/// git's trimmed answer, or why there is none.
fn git_text(
    dir: &Path,
    args: &[&str],
    environment: &[(&str, std::ffi::OsString)],
) -> Result<String, String> {
    let mut command = reading_git_command(dir, args);
    command.envs(environment.iter().map(|(key, value)| (key, value)));
    let what = format!("git {}", args.first().copied().unwrap_or_default());
    let output = super::process::run_bounded_command(command, OUTPUT_LIMIT, TIMEOUT)
        .map_err(|_| format!("{what} answered more than 1 MiB or could not start"))?;
    if !output.status.success() {
        let said = String::from_utf8_lossy(&output.stderr);
        let said = said.lines().map(str::trim).find(|line| !line.is_empty());
        return Err(match said {
            Some(said) => format!("{what} failed: {said}"),
            None => format!("{what} failed or took longer than {} s", TIMEOUT.as_secs()),
        });
    }
    String::from_utf8(output.stdout)
        .map(|text| text.trim().to_string())
        .map_err(|_| format!("{what} answered something that is not UTF-8"))
}

fn lines(text: &str) -> Vec<String> {
    text.lines()
        .filter(|line| !line.is_empty())
        .map(str::to_string)
        .collect()
}

fn is_object_id(value: &str) -> bool {
    matches!(value.len(), 40 | 64) && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

/// A relative path without empty, `.` or `..` parts or control characters.
fn plain_relative_path(path: &str) -> bool {
    !path.is_empty()
        && !path.starts_with('/')
        && !path.chars().any(char::is_control)
        && !path
            .split('/')
            .any(|part| part.is_empty() || part == "." || part == "..")
}

fn sha256_file(path: &Path) -> std::io::Result<String> {
    use sha2::{Digest, Sha256};
    let mut file = File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let count = file.read(&mut buffer)?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    format!("{:x}", Sha256::digest(bytes))
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::process::Command;

    fn run(dir: &Path, script: &str) -> String {
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
        let dir = scratch_folder().unwrap().join(name);
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn working_tree_id_matches_git_and_writes_nothing() {
        let dir = fresh("repo");
        run(
            &dir,
            "git init -q && echo base > a.txt && echo '*.log' > .gitignore && git add -A && \
             git -c commit.gpgsign=false commit -qm base && \
             echo edited > a.txt && echo new > b.txt && echo noise > c.log",
        );
        let state = |dir: &Path| {
            run(
                dir,
                "git count-objects -v | head -1 && git status --porcelain && cksum .git/index",
            )
        };
        let before = state(&dir);
        let tree = working_tree_id(&dir).expect("tree id");
        assert_eq!(state(&dir), before);

        let expected = run(
            &dir,
            "cp -R . ../copy-$$ && cd ../copy-$$ && git add -A && git write-tree && cd .. && rm -rf copy-$$",
        );
        assert_eq!(tree, expected);
        fs::remove_dir_all(dir.parent().unwrap()).unwrap();
    }

    #[test]
    fn facts_cover_changes_stashes_unpushed_work_and_ignored_state_without_hooks() {
        let dir = fresh("work");
        run(
            &dir,
            "git init -q --bare -b main upstream.git &&git init -q -b main repo && cd repo && \
             printf '.env\\nbuild/\\n*.log\\n' > .gitignore && echo one > a.txt && git add -A && \
             git commit -qm one && git remote add origin ../upstream.git && \
             git push -q -u origin main && echo two > a.txt && git commit -qam two && \
             echo stashed > a.txt && git stash -q && echo edited > a.txt && \
             mkdir -p notes build && echo n > notes/n.md && echo TOKEN=x > .env && \
             echo out > build/app.env && ln -s a.txt link && \
             mkdir -p .git/hooks && printf '#!/bin/sh\\ntouch ../hook-ran\\n' > .git/hooks/post-index-change && \
             chmod +x .git/hooks/post-index-change && git config core.fsmonitor 'touch ../fsmonitor-ran; false'",
        );
        let repo = dir.join("repo");
        let commit = |rev: &str| {
            run(
                &repo,
                &format!("git -c core.fsmonitor=false rev-parse {rev}"),
            )
        };
        let facts = read(&repo);
        assert_eq!(facts.head.as_deref(), Some(commit("HEAD").as_str()));
        assert_eq!(facts.branch.as_deref(), Some("main"));
        assert_eq!(facts.upstream.as_deref(), Some("origin/main"));
        assert_eq!((facts.ahead, facts.behind), (1, 0));
        assert_eq!(facts.origin.as_deref(), Some("../upstream.git"));
        assert_eq!(facts.status_error, None);
        // a.txt, link and notes/n.md (each untracked file on its own).
        assert_eq!((facts.changed, facts.untracked), (3, 2));
        assert_eq!(facts.stashes, vec![commit("stash@{0}")]);
        assert_eq!(facts.unpushed_commits, 1);
        assert_eq!(facts.unpushed_tips, vec![commit("HEAD")]);
        assert_eq!(facts.ignored_state, vec![".env".to_string()]);
        let fingerprints = facts.fingerprints.expect("fingerprints");
        let edited = sha256_hex(b"edited\n");
        assert_eq!(
            fingerprints,
            vec![
                format!("sha256:{edited} a.txt"),
                format!("link:{} link", sha256_hex(b"a.txt")),
                format!("sha256:{} notes/n.md", sha256_hex(b"n\n")),
            ]
        );
        assert!(is_object_id(facts.tree.as_deref().unwrap()));
        assert!(facts.carry_trees.is_empty());
        assert!(!dir.join("hook-ran").exists(), "a hook ran");
        assert!(!dir.join("fsmonitor-ran").exists(), "fsmonitor ran");

        run(
            &repo,
            "git -c core.fsmonitor=false update-ref refs/carry/peer/x HEAD",
        );
        let carried = read(&repo).carry_trees;
        assert_eq!(carried, vec![commit("HEAD^{tree}")]);
        fs::remove_dir_all(dir.parent().unwrap()).unwrap();
    }

    #[test]
    fn a_clean_checkout_and_one_before_its_first_commit_have_plain_facts() {
        let dir = fresh("clean");
        run(&dir, "git init -q -b main empty && git init -q -b main clean && cd clean && echo a > a && git add a && git commit -qm a");
        let empty = read(&dir.join("empty"));
        assert_eq!(empty.head, None);
        assert_eq!(empty.branch.as_deref(), Some("main"));
        assert_eq!((empty.changed, empty.unpushed_commits), (0, 0));
        assert_eq!(empty.fingerprints, None);
        let clean = read(&dir.join("clean"));
        assert_eq!((clean.changed, clean.fingerprints.clone()), (0, None));
        assert_eq!(clean.tree, None);
        // No remote at all: every commit exists only here.
        assert_eq!(clean.unpushed_commits, 1);
        assert_eq!(clean.origin, None);
        fs::remove_dir_all(dir.parent().unwrap()).unwrap();
    }

    #[test]
    fn a_folder_git_cannot_read_says_why() {
        let dir = fresh("broken");
        fs::create_dir(dir.join(".git")).unwrap();
        let facts = read(&dir);
        let error = facts.status_error.expect("status error");
        assert!(error.starts_with("git status failed: "), "{error}");
        assert_eq!((facts.changed, facts.head), (0, None));
        fs::remove_dir_all(dir.parent().unwrap()).unwrap();
    }

    #[test]
    fn urls_lose_their_credentials_and_nothing_else() {
        let clean = |url: &str| without_credentials(url).unwrap();
        assert_eq!(
            clean("https://example.org/a/b.git"),
            "https://example.org/a/b.git"
        );
        assert_eq!(
            clean("https://user:tok@example.org/a/b.git"),
            "https://example.org/a/b.git"
        );
        assert_eq!(
            clean("ssh://git@example.org:2222/a/b"),
            "ssh://example.org:2222/a/b"
        );
        assert_eq!(
            clean("http://git.example.internal:3000/owner/x"),
            "http://git.example.internal:3000/owner/x"
        );
        assert_eq!(clean("file:///srv/git/x.git"), "file:///srv/git/x.git");
        assert_eq!(
            clean("git@example.org:owner/x.git"),
            "git@example.org:owner/x.git"
        );
        assert!(without_credentials("user:pw@host:x").is_none());
        assert!(without_credentials("https://a b").is_none());
    }

    #[test]
    fn notable_ignored_state_keeps_secrets_and_data_but_not_build_output() {
        for path in [
            ".env",
            "apps/web/.env.local",
            "services/api/pb_data",
            "data/app.sqlite3",
            "WIFI_MESH_FIX.private.md",
        ] {
            assert!(is_notable_ignored_path(path), "{path} should be reported");
        }
        for path in [
            ".env.example",
            "node_modules/pkg/.env",
            "target/debug/app.db",
            "macos/Flutter/ephemeral",
            ".DS_Store",
            "dist",
            ".rsync-partial/.env",
            "app/.copy-partial/data.db",
        ] {
            assert!(!is_notable_ignored_path(path), "{path} should be skipped");
        }
    }
}
