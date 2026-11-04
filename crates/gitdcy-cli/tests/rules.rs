//! `gitdcy rules` end to end: real repositories, real `git push` through the
//! installed hook, in a sandbox with its own HOME and git config.
#![cfg(unix)]

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::time::{SystemTime, UNIX_EPOCH};

const GITDCY: &str = env!("CARGO_BIN_EXE_gitdcy");
const ZERO: &str = "0000000000000000000000000000000000000000";

struct Sandbox {
    root: PathBuf,
    path: String,
}

impl Sandbox {
    fn new(name: &str) -> Self {
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!(
            "gitdcy-rules-{name}-{}-{unique}",
            std::process::id()
        ));
        fs::create_dir_all(root.join("home/.config/gitdcy")).unwrap();
        let bin = Path::new(GITDCY).parent().unwrap().display().to_string();
        let path = format!("{bin}:{}", std::env::var("PATH").unwrap_or_default());
        let sandbox = Self { root, path };
        fs::write(
            sandbox.root.join("home/.gitconfig"),
            "[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n",
        )
        .unwrap();
        sandbox
    }

    fn command(&self, program: &str, dir: &Path) -> Command {
        let home = self.root.join("home");
        let mut command = Command::new(program);
        command
            .current_dir(dir)
            .env("HOME", &home)
            .env("XDG_CONFIG_HOME", home.join(".config"))
            .env("GIT_CONFIG_GLOBAL", home.join(".gitconfig"))
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("PATH", &self.path)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GITDCY_READ_ONLY_STATUS");
        command
    }

    fn git(&self, dir: &Path, args: &[&str]) -> Output {
        self.command("git", dir).args(args).output().unwrap()
    }

    fn ok(&self, dir: &Path, args: &[&str]) -> String {
        let out = self.git(dir, args);
        assert!(out.status.success(), "git {args:?}: {}", text(&out));
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn gitdcy(&self, dir: &Path, args: &[&str]) -> Output {
        self.command(GITDCY, dir).args(args).output().unwrap()
    }

    fn gitdcy_stdin(&self, dir: &Path, args: &[&str], input: &str) -> Output {
        let mut child = self
            .command(GITDCY, dir)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        child
            .stdin
            .take()
            .unwrap()
            .write_all(input.as_bytes())
            .unwrap();
        child.wait_with_output().unwrap()
    }

    /// The rules file, where GitDCY's own config says it is.
    fn rules(&self, text: &str) -> PathBuf {
        let rules = self.root.join("fleet/repos.toml");
        fs::create_dir_all(rules.parent().unwrap()).unwrap();
        fs::write(&rules, text).unwrap();
        fs::write(
            self.root.join("home/.config/gitdcy/local.yaml"),
            format!("rules_file: {}\n", rules.display()),
        )
        .unwrap();
        rules
    }

    /// A working repository with one commit and a bare `origin` next to it.
    fn repo(&self, name: &str) -> (PathBuf, String) {
        let work = self.root.join(name);
        let forge = self.root.join(format!("{name}-forge.git"));
        self.ok(
            &self.root,
            &["init", "-q", "--bare", forge.to_str().unwrap()],
        );
        self.ok(&self.root, &["init", "-q", work.to_str().unwrap()]);
        fs::write(work.join("README"), "hello\n").unwrap();
        self.ok(&work, &["add", "README"]);
        self.ok(&work, &["commit", "-qm", "first"]);
        self.ok(&work, &["remote", "add", "origin", forge.to_str().unwrap()]);
        let first = self.ok(&work, &["rev-parse", "HEAD"]);
        (work, first)
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

fn json(out: &Output) -> serde_json::Value {
    serde_json::from_slice(&out.stdout).unwrap_or_else(|e| panic!("{e}: {}", text(out)))
}

/// Built here so this file holds no address itself.
fn private_address() -> String {
    format!("{}@{}", "someone", "gmail.com")
}

fn rule(first: &str, push: &str, extra: &str) -> String {
    format!(
        "selfhosted = [\"forge.example.com\"]\n\n[[repo]]\nname = \"example-app\"\nfirst_commit = \"{first}\"\npush = {push}\nnote = \"Customer data stays on our own forge\"\n{extra}"
    )
}

#[test]
fn a_selfhosted_rule_refuses_a_public_remote_and_lets_its_own_forge_through() {
    let s = Sandbox::new("selfhosted");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"selfhosted\"", ""));
    let out = s.gitdcy(&work, &["rules", "install"]);
    assert!(out.status.success(), "{}", text(&out));

    s.ok(&work, &["checkout", "-qb", "feature"]);
    let out = s.git(&work, &["push", "-q", "origin", "feature"]);
    assert!(
        out.status.success(),
        "a path on this device is allowed: {}",
        text(&out)
    );

    let head = s.ok(&work, &["rev-parse", "HEAD"]);
    let input = format!("refs/heads/feature {head} refs/heads/feature {ZERO}\n");
    let out = s.gitdcy_stdin(
        &work,
        &[
            "rules",
            "check",
            "--stdin",
            "github",
            "https://user:t0ken@github.com/example-org/app.git",
        ],
        &input,
    );
    let said = text(&out);
    assert_eq!(out.status.code(), Some(1), "{said}");
    assert!(said.contains("REFUSED"), "{said}");
    assert!(
        said.contains("self-hosted hosts (forge.example.com)"),
        "{said}"
    );
    assert!(
        said.contains("Customer data stays on our own forge"),
        "{said}"
    );
    assert!(
        said.contains("question for the repository's owner"),
        "{said}"
    );
    assert!(
        !said.contains("t0ken"),
        "a token in a URL is never printed: {said}"
    );
    assert!(
        !said.contains("--no-verify"),
        "no way around it is offered: {said}"
    );

    let out = s.gitdcy_stdin(
        &work,
        &[
            "rules",
            "check",
            "--stdin",
            "forge",
            "ssh://git@forge.example.com:2222/team/app.git",
        ],
        &input,
    );
    assert!(
        out.status.success(),
        "a listed host is allowed: {}",
        text(&out)
    );
}

#[test]
fn a_protected_branch_takes_only_a_pull_request() {
    let s = Sandbox::new("protected");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"anywhere\"", "protected = [\"main\"]\n"));
    s.gitdcy(&work, &["rules", "install"]);

    let out = s.git(&work, &["push", "-q", "origin", "main"]);
    let said = text(&out);
    assert!(!out.status.success(), "{said}");
    assert!(
        said.contains("Branch main of \"example-app\" takes no direct push"),
        "{said}"
    );
    assert!(
        said.contains("git push origin HEAD:refs/heads/NEW-BRANCH"),
        "{said}"
    );

    let out = s.git(&work, &["push", "-q", "origin", "HEAD:refs/heads/topic"]);
    assert!(out.status.success(), "{}", text(&out));

    // Deleting the protected branch on the remote writes to it too.
    let out = s.gitdcy_stdin(
        &work,
        &["rules", "check", "--stdin", "origin", "/srv/git/app.git"],
        &format!("(delete) {ZERO} refs/heads/main {first}\n"),
    );
    assert_eq!(out.status.code(), Some(1), "{}", text(&out));
}

#[test]
fn nowhere_refuses_even_a_path_on_this_device() {
    let s = Sandbox::new("nowhere");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"nowhere\"", ""));
    s.gitdcy(&work, &["rules", "install"]);
    let out = s.git(&work, &["push", "-q", "origin", "HEAD:refs/heads/topic"]);
    let said = text(&out);
    assert!(!out.status.success(), "{said}");
    assert!(said.contains("is not pushed anywhere"), "{said}");
}

#[test]
fn a_list_of_remotes_refuses_one_not_on_it() {
    let s = Sandbox::new("remotes");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "[\"github.com/example-org/\"]", ""));
    s.gitdcy(&work, &["rules", "install"]);
    let out = s.git(&work, &["push", "-q", "origin", "HEAD:refs/heads/topic"]);
    let said = text(&out);
    assert!(!out.status.success(), "{said}");
    assert!(
        said.contains("may only be pushed to github.com/example-org/"),
        "{said}"
    );
    let head = s.ok(&work, &["rev-parse", "HEAD"]);
    let out = s.gitdcy_stdin(
        &work,
        &[
            "rules",
            "check",
            "--stdin",
            "gh",
            "git@github.com:Example-Org/app.git",
        ],
        &format!("refs/heads/topic {head} refs/heads/topic {ZERO}\n"),
    );
    assert!(out.status.success(), "{}", text(&out));
}

#[test]
fn without_a_rule_a_push_goes_anywhere_and_is_still_read_for_personal_data() {
    let s = Sandbox::new("norule");
    let (work, _) = s.repo("app");
    s.rules("personal = [\"Jane Doe\"]\n");
    let out = s.gitdcy(&work, &["rules", "check", "--format", "json"]);
    assert!(out.status.success(), "{}", text(&out));
    let value = json(&out);
    assert_eq!(value["data"]["rule"], serde_json::Value::Null);
    assert_eq!(value["data"]["refused"], false);

    s.commit(
        &work,
        "contact.txt",
        &format!("mail {}\n", private_address()),
    );
    let out = s.gitdcy(&work, &["rules", "check"]);
    let said = text(&out);
    assert_eq!(out.status.code(), Some(1), "{said}");
    assert!(
        said.contains("contact.txt:1: a private mail address (s…@gmail.com)"),
        "{said}"
    );
    assert!(
        !said.contains(&private_address()),
        "the address is not printed whole: {said}"
    );

    s.ok(&work, &["reset", "-q", "--hard", "HEAD~1"]);
    s.commit(&work, "notes.txt", "met jane doe today\n");
    let out = s.gitdcy(&work, &["rules", "check", "--format", "json"]);
    assert_eq!(out.status.code(), Some(1));
    let value = json(&out);
    assert_eq!(value["data"]["findings"][0]["kind"], "personal-word");
    assert_eq!(value["data"]["findings"][0]["place"], "notes.txt:1");
    assert!(!text(&out).to_lowercase().contains("jane doe"));

    s.ok(&work, &["reset", "-q", "--hard", "HEAD~1"]);
    s.rules(&format!("allow = [\"{}\"]\n", private_address()));
    s.commit(
        &work,
        "contact.txt",
        &format!("mail {}\n", private_address()),
    );
    let out = s.gitdcy(&work, &["rules", "check"]);
    assert!(
        out.status.success(),
        "an allowed address passes: {}",
        text(&out)
    );
}

#[test]
fn the_copy_holds_on_a_machine_without_the_rules_file() {
    let s = Sandbox::new("copy");
    let (work, first) = s.repo("app");
    let rules = s.rules(&rule(&first, "\"selfhosted\"", ""));
    let out = s.gitdcy(&work, &["rules", "install", "--format", "json"]);
    assert!(out.status.success(), "{}", text(&out));
    assert_eq!(json(&out)["data"]["copy_written"], true);
    fs::rename(&rules, rules.with_extension("toml.elsewhere")).unwrap();

    let out = s.gitdcy(&work, &["rules", "--format", "json"]);
    let value = json(&out);
    assert_eq!(value["data"]["source"], "installed-copy");
    assert_eq!(value["data"]["rule"]["push"], "selfhosted");
    assert_eq!(
        value["outcome"], "partial",
        "it says the rules file is missing"
    );

    let head = s.ok(&work, &["rev-parse", "HEAD"]);
    let out = s.gitdcy_stdin(
        &work,
        &[
            "rules",
            "check",
            "--stdin",
            "github",
            "git@github.com:example-org/app.git",
        ],
        &format!("refs/heads/main {head} refs/heads/main {ZERO}\n"),
    );
    let said = text(&out);
    assert_eq!(out.status.code(), Some(1), "{said}");
    assert!(
        said.contains("gitdcy-rules.toml (a copy of the owner's rules file)"),
        "{said}"
    );
}

#[test]
fn an_existing_hook_is_kept_aside_and_chained() {
    let s = Sandbox::new("chain");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"anywhere\"", ""));
    let hooks = work.join(".git/hooks");
    let marker = s.root.join("their-hook-ran");
    let theirs = format!(
        "#!/bin/sh\ncat > {}\nexit \"${{THEIR_EXIT:-0}}\"\n",
        marker.display()
    );
    fs::write(hooks.join("pre-push"), &theirs).unwrap();
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(hooks.join("pre-push"), fs::Permissions::from_mode(0o755)).unwrap();

    let out = s.gitdcy(&work, &["rules", "install", "--format", "json"]);
    assert!(out.status.success(), "{}", text(&out));
    let value = json(&out);
    let chained = value["data"]["chained"].as_str().unwrap().to_string();
    assert!(chained.starts_with("pre-push.bak-"), "{chained}");
    assert_eq!(fs::read_to_string(hooks.join(&chained)).unwrap(), theirs);

    let out = s.git(&work, &["push", "-q", "origin", "HEAD:refs/heads/topic"]);
    assert!(out.status.success(), "{}", text(&out));
    assert!(
        fs::read_to_string(&marker)
            .unwrap()
            .contains("refs/heads/topic"),
        "their hook got the same input"
    );

    let out = s.gitdcy(&work, &["rules", "install", "--format", "json"]);
    let value = json(&out);
    assert_eq!(
        value["data"]["hook_written"], false,
        "a second run changes nothing"
    );
    assert_eq!(value["data"]["copy_written"], false);
    assert_eq!(
        value["data"]["chained"],
        chained.as_str(),
        "the chain is kept"
    );
    let backups = fs::read_dir(&hooks)
        .unwrap()
        .filter(|e| {
            e.as_ref()
                .unwrap()
                .file_name()
                .to_string_lossy()
                .contains(".bak-")
        })
        .count();
    assert_eq!(backups, 1);

    s.commit(&work, "b.txt", "more\n");
    let out = s
        .command("git", &work)
        .args(["push", "-q", "origin", "HEAD:refs/heads/topic"])
        .env("THEIR_EXIT", "1")
        .output()
        .unwrap();
    assert!(
        !out.status.success(),
        "a refusal by their hook stops the push"
    );
}

#[test]
fn install_writes_nothing_when_core_hooks_path_would_skip_the_hook() {
    let s = Sandbox::new("hookspath");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"selfhosted\"", ""));
    fs::create_dir_all(work.join(".husky")).unwrap();
    fs::write(work.join(".husky/pre-push"), "#!/bin/sh\nnpm test\n").unwrap();
    s.ok(&work, &["config", "core.hooksPath", ".husky"]);
    let out = s.gitdcy(&work, &["rules", "install"]);
    let said = text(&out);
    assert_eq!(out.status.code(), Some(1), "{said}");
    assert!(said.contains("core.hooksPath"), "{said}");
    assert!(!work.join(".git/hooks/pre-push").exists());
    assert!(!work.join(".git/gitdcy-rules.toml").exists());

    // A hook there that calls the repository's own is enough.
    fs::write(
        work.join(".husky/pre-push"),
        "#!/bin/sh\n\"$(git rev-parse --git-dir)/hooks/pre-push\" \"$@\"\n",
    )
    .unwrap();
    let out = s.gitdcy(&work, &["rules", "install"]);
    assert!(out.status.success(), "{}", text(&out));
}

#[test]
fn install_refuses_without_a_rule_and_two_rules_refuse_every_push() {
    let s = Sandbox::new("conflict");
    let (work, first) = s.repo("app");
    s.rules("selfhosted = []\n");
    let out = s.gitdcy(&work, &["rules", "install"]);
    let said = text(&out);
    assert_eq!(out.status.code(), Some(1), "{said}");
    assert!(
        said.contains(&format!("first_commit = \"{first}\"")),
        "{said}"
    );
    assert!(!work.join(".git/hooks/pre-push").exists());

    let forge = s.ok(&work, &["remote", "get-url", "origin"]);
    s.rules(&format!(
        "[[repo]]\nname = \"one\"\nfirst_commit = \"{first}\"\n\n[[repo]]\nname = \"two\"\nremotes = [\"{forge}\"]\npush = \"nowhere\"\n"
    ));
    let out = s.gitdcy(&work, &["rules", "install"]);
    assert_eq!(out.status.code(), Some(1), "{}", text(&out));
    let out = s.gitdcy(&work, &["rules", "check", "--format", "json"]);
    assert_eq!(out.status.code(), Some(1), "{}", text(&out));
    let value = json(&out);
    assert_eq!(value["data"]["refused"], true);
    assert!(value["data"]["reasons"][0]
        .as_str()
        .unwrap()
        .contains("Rules one, two"));
}

#[test]
fn a_rule_is_found_by_its_remote_when_the_first_commit_is_not_known() {
    let s = Sandbox::new("hint");
    let (work, _) = s.repo("app");
    let forge = s.ok(&work, &["remote", "get-url", "origin"]);
    s.rules(&format!(
        "[[repo]]\nname = \"by-remote\"\nremotes = [\"{forge}/\"]\npush = \"nowhere\"\n"
    ));
    let out = s.gitdcy(&work, &["rules", "--format", "json"]);
    let value = json(&out);
    assert_eq!(value["data"]["rule"]["name"], "by-remote", "{}", text(&out));
    assert!(value["data"]["matched_by"][0]
        .as_str()
        .unwrap()
        .starts_with("remote origin"));
    assert_eq!(value["data"]["remotes"][0]["allowed"], false);
}

#[test]
fn a_local_yaml_in_the_repository_cannot_move_the_rules_file() {
    let s = Sandbox::new("cwdconfig");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"nowhere\"", ""));
    let empty = s.root.join("empty.toml");
    fs::write(&empty, "").unwrap();
    fs::write(
        work.join(".gitdcy.local.yaml"),
        format!("rules_file: {}\n", empty.display()),
    )
    .unwrap();
    let out = s.gitdcy(&work, &["rules", "check", "--format", "json"]);
    let value = json(&out);
    assert_eq!(value["data"]["rule"], "example-app", "{}", text(&out));
    assert_eq!(value["data"]["refused"], true);
}

#[test]
fn errors_come_as_the_json_envelope() {
    let s = Sandbox::new("errors");
    let not_a_repo = s.root.join("plain");
    fs::create_dir_all(&not_a_repo).unwrap();
    let out = s.gitdcy(&not_a_repo, &["rules", "--format", "json"]);
    assert_eq!(out.status.code(), Some(1));
    let value = json(&out);
    assert_eq!(value["schema_version"], "gitdcy.response.v1");
    assert_eq!(value["operation"], "rules");
    assert_eq!(value["outcome"], "error");
    assert!(value["error"]["message"]
        .as_str()
        .unwrap()
        .contains("not inside a git repository"));

    let (work, _) = s.repo("app");
    s.rules("[[repo]]\nname = \"x\"\npush = \"github\"\nremotes = [\"x/y\"]\n");
    let out = s.gitdcy(&work, &["rules", "check"]);
    assert_eq!(
        out.status.code(),
        Some(1),
        "a broken rules file refuses the push"
    );
    assert!(text(&out).contains("is not valid"), "{}", text(&out));
}

#[test]
fn the_hook_refuses_when_gitdcy_is_gone() {
    let s = Sandbox::new("gone");
    let (work, first) = s.repo("app");
    s.rules(&rule(&first, "\"anywhere\"", ""));
    let bin = s.root.join("bin");
    fs::create_dir_all(&bin).unwrap();
    fs::copy(GITDCY, bin.join("gitdcy")).unwrap();
    let out = s
        .command(bin.join("gitdcy").to_str().unwrap(), &work)
        .args(["rules", "install"])
        .output()
        .unwrap();
    assert!(out.status.success(), "{}", text(&out));
    fs::rename(bin.join("gitdcy"), bin.join("gitdcy.moved")).unwrap();
    let out = s
        .command("git", &work)
        .args(["push", "-q", "origin", "HEAD:refs/heads/topic"])
        .env("PATH", "/usr/bin:/bin")
        .output()
        .unwrap();
    let said = text(&out);
    assert!(!out.status.success(), "{said}");
    assert!(said.contains("gitdcy is not installed here"), "{said}");
}

/// A `gh` in front of the PATH that runs `answer` (a shell line).
fn fake_gh(s: &mut Sandbox, answer: &str) {
    use std::os::unix::fs::PermissionsExt;
    let bin = s.root.join("fakebin");
    fs::create_dir_all(&bin).unwrap();
    let gh = bin.join("gh");
    fs::write(&gh, format!("#!/bin/sh\n{answer}\n")).unwrap();
    fs::set_permissions(&gh, fs::Permissions::from_mode(0o755)).unwrap();
    s.path = format!("{}:{}", bin.display(), s.path);
}

/// A commit dated `days` ago, on the branch checked out.
fn old_commit(s: &Sandbox, work: &Path, file: &str, days: u64) {
    let when = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
        - days * 86_400;
    fs::write(work.join(file), "old work\n").unwrap();
    s.ok(work, &["add", file]);
    let out = s
        .command("git", work)
        .args(["commit", "-qm", "old work"])
        .env("GIT_AUTHOR_DATE", format!("@{when} +0000"))
        .env("GIT_COMMITTER_DATE", format!("@{when} +0000"))
        .output()
        .unwrap();
    assert!(out.status.success(), "{}", text(&out));
}

const ONE_PR: &str = r#"echo '[{"number":7,"title":"Fix the thing","url":"https://github.com/example-org/app/pull/7","createdAt":"2026-01-01T00:00:00Z","author":{"login":"someone"},"repository":{"nameWithOwner":"example-org/app"},"isDraft":false}]'"#;

#[test]
fn a_scan_names_each_rule_its_forbidden_remotes_and_pushed_personal_data() {
    let s = Sandbox::new("scan");
    let (work, first) = s.repo("app");
    let (other, _) = s.repo("other");
    // Made in the same second, both first commits would be one and the same.
    s.ok(&other, &["commit", "-q", "--amend", "-m", "another first"]);
    s.rules(&rule(&first, "\"selfhosted\"", ""));
    s.ok(
        &work,
        &[
            "remote",
            "add",
            "github",
            "https://github.com/example-org/app.git",
        ],
    );
    // What already reached a remote: a commit with a private address.
    s.commit(&other, "NOTES", &format!("mail {}\n", private_address()));
    s.ok(&other, &["push", "-q", "origin", "main"]);

    let paths = [
        work.to_str().unwrap(),
        other.to_str().unwrap(),
        "/nonexistent/x",
    ];
    let mut args = vec!["rules", "scan", "--format", "json"];
    args.extend(paths);
    let out = s.gitdcy(&s.root, &args);
    let value = json(&out);
    assert!(out.status.success(), "{}", text(&out));
    assert_eq!(value["operation"], "rules.scan");
    let checkouts = value["data"]["checkouts"].as_array().unwrap();
    assert_eq!(checkouts.len(), 3);
    let app = &checkouts[0];
    assert_eq!(app["rule"], "example-app");
    assert_eq!(app["forbidden"].as_array().unwrap().len(), 1, "{app}");
    assert_eq!(app["forbidden"][0]["name"], "github");
    assert_eq!(app["forbidden"][0]["url"], "github.com/example-org/app");
    assert!(
        app["hygiene"].is_null(),
        "no [hygiene], no reminders: {app}"
    );
    let other = &checkouts[1];
    assert!(other["rule"].is_null());
    assert!(other["forbidden"].as_array().unwrap().is_empty());
    assert_eq!(
        other["pushed_findings"].as_array().unwrap().len(),
        1,
        "{other}"
    );
    assert!(
        !other.to_string().contains(&private_address()),
        "masked: {other}"
    );
    assert_eq!(checkouts[2]["state"], "missing");
    assert_eq!(
        value["outcome"], "partial",
        "a missing checkout is a warning"
    );

    // The same in words: the forbidden remote and the pushed finding.
    let mut args = vec!["rules", "scan"];
    args.extend(&paths[..2]);
    let said = text(&s.gitdcy(&s.root, &args));
    assert!(
        said.contains("remote github -> github.com/example-org/app"),
        "{said}"
    );
    assert!(said.contains("pushed commit"), "{said}");
}

#[test]
fn reminders_name_the_dirty_dishes_and_never_refuse_a_push() {
    let mut s = Sandbox::new("dishes");
    let (work, first) = s.repo("app");
    s.ok(&work, &["push", "-q", "origin", "main"]);
    s.ok(&work, &["branch", "-q", "--set-upstream-to=origin/main"]);
    s.ok(
        &work,
        &[
            "remote",
            "add",
            "github",
            "https://github.com/example-org/app.git",
        ],
    );
    s.rules(&format!(
        "{}\n[hygiene]\nbranch_days = 3\n",
        rule(&first, "\"anywhere\"", "")
    ));
    fake_gh(&mut s, ONE_PR);
    s.ok(&work, &["checkout", "-qb", "old-idea"]);
    old_commit(&s, &work, "IDEA", 10);
    s.ok(&work, &["checkout", "-q", "main"]);
    fs::write(work.join("README"), "changed\n").unwrap();

    let out = s.gitdcy(&work, &["rules", "scan", "--format", "json", "."]);
    let value = json(&out);
    let dishes = &value["data"]["checkouts"][0]["hygiene"];
    let kinds: Vec<&str> = dishes["reminders"]
        .as_array()
        .unwrap_or_else(|| panic!("{value}"))
        .iter()
        .map(|r| r["kind"].as_str().unwrap())
        .collect();
    assert_eq!(
        kinds,
        ["dirty", "unpushed", "branch", "pull-request"],
        "{dishes}"
    );
    assert_eq!(dishes["branches"][0]["branch"], "old-idea");
    assert!(dishes["branches"][0]["days"].as_u64().unwrap() >= 10);
    assert_eq!(dishes["unpushed_before_today"], 1);
    assert_eq!(dishes["pull_requests"]["open"][0]["number"], 7);

    // Before a push: the reminders, and the push goes.
    s.gitdcy(&work, &["rules", "install"]);
    s.ok(&work, &["checkout", "-q", "--", "README"]);
    s.commit(&work, "NEW", "new\n");
    let out = s.git(&work, &["push", "-q", "origin", "main"]);
    let said = text(&out);
    assert!(out.status.success(), "a reminder never refuses: {said}");
    assert!(said.contains("left lying around"), "{said}");
    assert!(
        said.contains("branch old-idea not merged into origin/main"),
        "{said}"
    );
    assert!(said.contains("pull request #7 open"), "{said}");
    assert!(
        said.contains("1 commit(s) on other branches are on no remote"),
        "what the push leaves behind: {said}"
    );
}

#[test]
fn without_gh_pull_requests_are_unknown_and_a_skipped_repository_gets_no_reminders() {
    let mut s = Sandbox::new("unknown");
    let (work, first) = s.repo("app");
    s.ok(
        &work,
        &[
            "remote",
            "add",
            "github",
            "https://github.com/example-org/app.git",
        ],
    );
    fake_gh(
        &mut s,
        "echo 'To get started with GitHub CLI, please run:  gh auth login' >&2; exit 4",
    );
    s.rules(&format!(
        "{}\n[hygiene]\n",
        rule(&first, "\"anywhere\"", "")
    ));
    let out = s.gitdcy(&work, &["rules", "scan", "--format", "json", "."]);
    let dishes = &json(&out)["data"]["checkouts"][0]["hygiene"];
    assert_eq!(dishes["pull_requests"]["state"], "unknown", "{dishes}");
    let unknown = dishes["reminders"]
        .as_array()
        .unwrap()
        .iter()
        .find(|r| r["kind"] == "pull-requests-unknown")
        .unwrap_or_else(|| panic!("{dishes}"));
    assert!(
        unknown["text"].as_str().unwrap().contains("gh auth login"),
        "{unknown}"
    );

    s.rules(&format!(
        "{}\n[hygiene]\nskip = [\"github.com/Example-Org/\"]\n",
        rule(&first, "\"anywhere\"", "")
    ));
    let out = s.gitdcy(&work, &["rules", "scan", "--format", "json", "."]);
    let checkout = &json(&out)["data"]["checkouts"][0];
    assert!(checkout["hygiene"].is_null(), "{checkout}");
    assert!(
        checkout["hygiene_off"]
            .as_str()
            .unwrap()
            .contains("keeps its own rules"),
        "{checkout}"
    );
    let out = s.gitdcy(&work, &["rules", "check", "--format", "json"]);
    assert_eq!(json(&out)["data"]["reminders"], serde_json::json!([]));
}
