//! `gitdcy checkouts` end to end: real repositories with a bare upstream, in a
//! sandbox with its own HOME and git config.
#![cfg(unix)]

use serde_json::Value;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::time::{SystemTime, UNIX_EPOCH};

const GITDCY: &str = env!("CARGO_BIN_EXE_gitdcy");

struct Sandbox {
    root: PathBuf,
}

impl Sandbox {
    fn new(name: &str) -> Self {
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!(
            "gitdcy-checkouts-{name}-{}-{unique}",
            std::process::id()
        ));
        fs::create_dir_all(root.join("home/.config/gitdcy")).unwrap();
        fs::write(
            root.join("home/.gitconfig"),
            "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n",
        )
        .unwrap();
        Self { root }
    }

    fn home(&self) -> PathBuf {
        self.root.join("home")
    }

    fn command(&self, program: &str, dir: &Path) -> Command {
        let home = self.home();
        let mut command = Command::new(program);
        command
            .current_dir(dir)
            .env("HOME", &home)
            .env("XDG_CONFIG_HOME", home.join(".config"))
            .env("GIT_CONFIG_GLOBAL", home.join(".gitconfig"))
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GITDCY_READ_ONLY_STATUS");
        command
    }

    fn ok(&self, dir: &Path, args: &[&str]) -> String {
        let out = self.command("git", dir).args(args).output().unwrap();
        assert!(out.status.success(), "git {args:?}: {}", text(&out));
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn gitdcy(&self, args: &[&str]) -> Output {
        self.command(GITDCY, &self.root)
            .args(args)
            .output()
            .unwrap()
    }

    /// A bare upstream with one commit, and `count` clones of it under HOME.
    fn upstream(&self, count: usize) -> (PathBuf, Vec<PathBuf>) {
        let forge = self.root.join("forge.git");
        let seed = self.root.join("seed");
        self.ok(
            &self.root,
            &["init", "-q", "--bare", forge.to_str().unwrap()],
        );
        self.ok(&self.root, &["init", "-q", seed.to_str().unwrap()]);
        self.commit(&seed, "README", "hello\n");
        self.ok(&seed, &["remote", "add", "origin", forge.to_str().unwrap()]);
        self.ok(&seed, &["push", "-q", "-u", "origin", "main"]);
        let clones = (0..count)
            .map(|n| {
                let clone = self.home().join(format!("code/app{n}"));
                self.ok(
                    &self.root,
                    &[
                        "clone",
                        "-q",
                        forge.to_str().unwrap(),
                        clone.to_str().unwrap(),
                    ],
                );
                clone
            })
            .collect();
        (seed, clones)
    }

    fn commit(&self, work: &Path, file: &str, content: &str) -> String {
        fs::write(work.join(file), content).unwrap();
        self.ok(work, &["add", file]);
        self.ok(work, &["commit", "-qm", &format!("change {file}")]);
        self.ok(work, &["rev-parse", "HEAD"])
    }
}

impl Drop for Sandbox {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

fn text(out: &Output) -> String {
    format!(
        "{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    )
}

fn json(out: &Output) -> Value {
    assert!(out.status.success(), "{}", text(out));
    serde_json::from_slice(&out.stdout).unwrap_or_else(|e| panic!("{e}: {}", text(out)))
}

fn by_path<'a>(envelope: &'a Value, path: &str) -> &'a Value {
    envelope["data"]["checkouts"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["path"] == path)
        .unwrap_or_else(|| panic!("{path} not in {envelope}"))
}

#[test]
fn a_clean_checkout_behind_is_fast_forwarded_and_nothing_else_is_touched() {
    let s = Sandbox::new("update");
    let (seed, clones) = s.upstream(4);
    let (clean, dirty, ahead, diverged) = (&clones[0], &clones[1], &clones[2], &clones[3]);
    let newest = s.commit(&seed, "NEWS", "one\n");
    s.ok(&seed, &["push", "-q"]);

    fs::write(dirty.join("README"), "my own edit\n").unwrap();
    s.ok(ahead, &["pull", "-q", "--ff-only"]);
    let ahead_head = s.commit(ahead, "MINE", "mine\n");
    let diverged_head = s.commit(diverged, "OTHER", "other\n");
    let dirty_head = s.ok(dirty, &["rev-parse", "HEAD"]);

    let paths = ["~/code/app0", "~/code/app1", "~/code/app2", "~/code/app3"];
    let mut args = vec!["checkouts", "--format", "json"];
    args.extend(paths);
    let before = json(&s.gitdcy(&args));
    assert_eq!(before["operation"], "checkouts");
    assert_eq!(before["outcome"], "success");
    let c = by_path(&before, "~/code/app0");
    assert_eq!(c["state"], "behind");
    assert_eq!(c["behind"], 1);
    assert_eq!(c["branch"], "main");
    assert_eq!(c["tracking_branch"], "origin/main");
    assert!(
        c["upstream"].as_str().unwrap().ends_with("change NEWS"),
        "{c}"
    );
    assert_eq!(
        c["detail"],
        "1 commit(s) behind; checkouts update brings it up"
    );
    let d = by_path(&before, "~/code/app1");
    assert_eq!(
        (d["state"].as_str(), d["changed"].as_u64()),
        (Some("behind"), Some(1))
    );
    assert_eq!(d["files"][0]["path"], "README");
    assert_eq!(d["files"][0]["status"], " M");
    assert_eq!(by_path(&before, "~/code/app2")["state"], "ahead");
    assert_eq!(by_path(&before, "~/code/app3")["state"], "diverged");

    let mut args = vec!["checkouts", "update", "--format", "json"];
    args.extend(paths);
    let after = json(&s.gitdcy(&args));
    assert_eq!(after["operation"], "checkouts.update");
    let c = by_path(&after, "~/code/app0");
    assert_eq!(c["state"], "current");
    assert_eq!(c["updated"], true);
    assert_eq!(c["detail"], "fast-forwarded 1 commit(s); up to date");
    assert_eq!(s.ok(clean, &["rev-parse", "HEAD"]), newest);

    // Local changes, local commits and a diverged branch stay exactly as they were.
    assert_eq!(by_path(&after, "~/code/app1")["updated"], false);
    assert_eq!(s.ok(dirty, &["rev-parse", "HEAD"]), dirty_head);
    assert_eq!(
        fs::read_to_string(dirty.join("README")).unwrap(),
        "my own edit\n"
    );
    assert_eq!(s.ok(ahead, &["rev-parse", "HEAD"]), ahead_head);
    assert_eq!(s.ok(diverged, &["rev-parse", "HEAD"]), diverged_head);
    assert_eq!(by_path(&after, "~/code/app3")["state"], "diverged");
    assert_eq!(
        s.ok(diverged, &["rev-list", "--count", "HEAD"]),
        "2",
        "no merge commit"
    );
}

#[test]
fn no_fetch_shows_the_last_fetch_and_an_untracked_file_blocks_the_update() {
    let s = Sandbox::new("nofetch");
    let (seed, clones) = s.upstream(1);
    s.commit(&seed, "NEWS", "one\n");
    s.ok(&seed, &["push", "-q"]);

    let quiet = json(&s.gitdcy(&["checkouts", "--no-fetch", "--format", "json", "~/code/app0"]));
    assert_eq!(by_path(&quiet, "~/code/app0")["state"], "current");

    fs::write(clones[0].join("scratch.txt"), "notes\n").unwrap();
    let out = json(&s.gitdcy(&["checkouts", "update", "--format", "json", "~/code/app0"]));
    let c = by_path(&out, "~/code/app0");
    assert_eq!(c["state"], "behind");
    assert_eq!(c["updated"], false);
    assert_eq!(c["files"][0]["status"], "??");
    assert!(
        c["detail"].as_str().unwrap().contains("commit or stash"),
        "{c}"
    );
}

#[test]
fn missing_paths_branches_without_upstream_and_failed_fetches_are_said_plainly() {
    let s = Sandbox::new("plain");
    let (_, clones) = s.upstream(2);
    s.ok(&clones[0], &["checkout", "-qb", "local-only"]);
    fs::create_dir_all(s.home().join("not-a-repo")).unwrap();
    s.ok(
        &clones[1],
        &[
            "remote",
            "set-url",
            "origin",
            "/nonexistent/gitdcy-forge.git",
        ],
    );

    let out = json(&s.gitdcy(&[
        "checkouts",
        "--format",
        "json",
        "~/code/app0",
        "~/code/app1",
        "~/not-a-repo",
        "~/gone",
    ]));
    assert_eq!(out["outcome"], "partial");
    let local = by_path(&out, "~/code/app0");
    assert_eq!(local["state"], "no-upstream");
    assert_eq!(local["branch"], "local-only");
    assert_eq!(local["detail"], "the branch follows no remote branch");
    let unfetched = by_path(&out, "~/code/app1");
    assert_eq!(unfetched["state"], "current");
    assert!(!unfetched["fetch_error"].as_str().unwrap().is_empty());
    assert!(
        unfetched["detail"]
            .as_str()
            .unwrap()
            .contains("so this is as of the last fetch"),
        "{unfetched}"
    );
    assert_eq!(
        by_path(&out, "~/not-a-repo")["detail"],
        "not a git checkout at that path"
    );
    assert_eq!(by_path(&out, "~/gone")["state"], "missing");
    let warnings = out["warnings"].as_array().unwrap();
    assert_eq!(warnings.len(), 3, "{warnings:?}");

    let human = s.gitdcy(&["checkouts", "~/gone"]);
    assert!(human.status.success());
    let said = text(&human);
    assert!(
        said.starts_with("1 checkout(s): 0 up to date, 0 behind, 1 need attention"),
        "{said}"
    );
    assert!(
        said.contains("~/gone") && said.contains("no folder at that path"),
        "{said}"
    );
}
