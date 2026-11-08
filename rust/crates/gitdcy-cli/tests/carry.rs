//! `gitdcy carry` against the shell scripts it replaces, on throwaway
//! repositories: the same refs, stash entries, working tree and lines, the
//! refused cases included (a branch name taken, a dirty checkout, another
//! commit). Both run as processes with the same clean environment.

#![cfg(unix)]

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

/// How the reference scripts run git: no hooks, no fsmonitor.
const G: &str = "G='git -c core.hooksPath=/dev/null -c core.fsmonitor=false'";
const RECEIVE: &str = "git -c core.hooksPath=/dev/null -c core.fsmonitor=false receive-pack";

struct Lab(PathBuf);

impl Drop for Lab {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

impl Lab {
    fn new(name: &str) -> Self {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let root = std::env::temp_dir().join(format!(
            "gitdcy-carry-{name}-{}-{nanos}",
            std::process::id()
        ));
        fs::create_dir_all(root.join("home")).unwrap();
        Lab(root)
    }

    fn path(&self, name: &str) -> PathBuf {
        self.0.join(name)
    }

    fn command(&self, program: &str, dir: &Path) -> Command {
        let mut command = Command::new(program);
        command
            .current_dir(dir)
            .env("HOME", self.0.join("home"))
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_NAME", "Test")
            .env("GIT_AUTHOR_EMAIL", "test@example.invalid")
            .env("GIT_COMMITTER_NAME", "Test")
            .env("GIT_COMMITTER_EMAIL", "test@example.invalid");
        command
    }

    /// A script's success and its standard output.
    fn try_sh(&self, dir: &Path, script: &str) -> (bool, String) {
        let output = self
            .command("sh", dir)
            .arg("-c")
            .arg(script)
            .output()
            .unwrap();
        (
            output.status.success(),
            String::from_utf8_lossy(&output.stdout).trim().to_string(),
        )
    }

    fn sh(&self, dir: &Path, script: &str) -> String {
        let output = self
            .command("sh", dir)
            .arg("-c")
            .arg(script)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "{script}\n{}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_string()
    }

    fn gitdcy(&self, dir: &Path, args: &[&str]) -> (bool, String) {
        let output = self
            .command(env!("CARGO_BIN_EXE_gitdcy"), dir)
            .args(args)
            .output()
            .unwrap();
        (
            output.status.success(),
            format!(
                "{}{}",
                String::from_utf8_lossy(&output.stdout).trim(),
                String::from_utf8_lossy(&output.stderr).trim()
            ),
        )
    }

    fn copy(&self, from: &str, to: &str) -> PathBuf {
        self.sh(&self.0, &format!("rm -rf {to} && cp -R {from} {to}"));
        self.path(to)
    }

    /// Everything about a checkout carry may change.
    fn state(&self, dir: &Path) -> String {
        self.sh(
            dir,
            "git for-each-ref --format='%(refname) %(objectname)' && echo -- && \
             git stash list --format='%H %gs' && echo -- && \
             git status --porcelain --untracked-files=all && echo -- && \
             find . -path ./.git -prune -o -type f -print | LC_ALL=C sort | \
             while IFS= read -r f; do cksum \"$f\"; done",
        )
    }
}

/// The placement script `gitdcy carry place` replaces, word for word
/// apart from the message and the folder.
fn reference_place(prefix: &str, from: &str, operation: &str, head: &str, message: &str) -> String {
    format!(
        "set -eu\n{G}\nP={prefix}\n\
         for ref in $($G for-each-ref --format='%(refname)' \"$P/heads/\"); do\n  b=${{ref#\"$P/heads/\"}}; sha=$($G rev-parse \"$ref\")\n  if $G update-ref \"refs/heads/$b\" \"$sha\" \"\" 2>/dev/null; then echo \"branch $b\"\n  elif [ \"$($G rev-parse \"refs/heads/$b\")\" = \"$sha\" ]; then :\n  elif $G update-ref \"refs/heads/carry/{from}/$b\" \"$sha\" \"\" 2>/dev/null; then echo \"branch carry/{from}/$b\"\n  else echo \"kept $ref\"; fi\ndone\n\
         for i in $($G for-each-ref --format='%(refname:lstrip=-1)' \"$P/stash/\" | sort -rn); do\n  sha=$($G rev-parse \"$P/stash/$i\")\n  if ! $G stash list --format=%H | grep -qx \"$sha\"; then $G stash store -m '{message}' \"$sha\" && echo \"stash $sha\"; fi\ndone\n\
         wip=$($G rev-parse -q --verify \"$P/wip^{{commit}}\")\nif [ \"$($G rev-parse -q --verify HEAD || true)\" = '{head}' ] && [ -z \"$($G status --porcelain)\" ] && $G diff --binary HEAD \"$wip\" | $G apply --whitespace=nowarn; then echo 'applied the working tree'\nelif $G update-ref 'refs/heads/carry/{from}/wip-{operation}' \"$wip\" \"\" 2>/dev/null; then echo 'branch carry/{from}/wip-{operation} (successor checkout differs)'\nelse echo \"kept $P/wip\"; fi\n"
    )
}

/// The fast-forward script `place --fast-forward` replaces.
fn reference_fast_forward(prefix: &str, branch: &str, head: &str) -> String {
    format!(
        "set -eu\n{G}\nif [ \"$($G symbolic-ref -q --short HEAD || true)\" != {branch} ]; then echo 'not on {branch}; left as is'\nelif $G merge-base --is-ancestor {head} HEAD; then echo 'already ahead; left as is'\nelif ! $G merge-base --is-ancestor HEAD {head}; then echo 'diverged; left as is, {prefix}/head keeps the device commit'\nelif $G merge --ff-only --quiet {head}; then echo 'fast-forwarded to {short}'\nelse echo 'fast-forward refused (local changes in the way); {prefix}/head keeps the commit'; fi\n",
        short = &head[..8]
    )
}

/// The push script `gitdcy carry send` replaces, into a local path.
fn reference_send(
    prefix: &str,
    to: &Path,
    tips: &[&str],
    stashes: &[&str],
    message: &str,
) -> String {
    let mut script = format!("set -eu\n{G}\nrefs=\"\"\n");
    for tip in tips {
        script.push_str(&format!(
            "found=0\nfor b in $($G for-each-ref --points-at {tip} --format='%(refname:lstrip=2)' refs/heads); do refs=\"$refs {tip}:{prefix}/heads/$b\"; found=1; done\n[ \"$found\" = 1 ] || refs=\"$refs {tip}:{prefix}/tips/{tip}\"\n"
        ));
    }
    for (index, stash) in stashes.iter().enumerate() {
        script.push_str(&format!("refs=\"$refs {stash}:{prefix}/stash/{index}\"\n"));
    }
    script.push_str(&format!(
        "objects=$($G rev-parse --path-format=absolute --git-path objects)\nindex=$($G rev-parse --path-format=absolute --git-path index)\nscratch=$(mktemp -d)\ntrap 'rm -rf \"$scratch\"' 0\nmkdir \"$scratch/objects\"\n[ -f \"$index\" ] && cp \"$index\" \"$scratch/index\"\nGIT_INDEX_FILE=\"$scratch/index\" GIT_OBJECT_DIRECTORY=\"$scratch/objects\" GIT_ALTERNATE_OBJECT_DIRECTORIES=\"$objects\"\nexport GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES\n$G add -A\ntree=$($G write-tree)\nif $G rev-parse -q --verify HEAD >/dev/null; then wip=$($G commit-tree \"$tree\" -p HEAD -m '{message}'); else wip=$($G commit-tree \"$tree\" -m '{message}'); fi\nrefs=\"$refs $wip:{prefix}/wip\"\n$G push --no-verify --quiet --receive-pack='{RECEIVE}' '{}' $refs\necho pushed to {prefix}\n",
        to.display()
    ));
    script
}

/// A device with two local-only branches, a stash and uncommitted work
/// (an edit, an untracked file with a space, a deletion), and a receiving
/// checkout of its main branch.
fn device_and_receiver(lab: &Lab) -> (PathBuf, PathBuf) {
    let root = lab.path("");
    lab.sh(
        &root,
        "git init -q -b main device && cd device && \
         echo base > a.txt && echo keep > b.txt && git add -A && git commit -qm base && \
         git switch -qc feature && echo f > f.txt && git add f.txt && git commit -qm feature && \
         git switch -qc topic main && git commit -q --allow-empty -m topic && \
         git switch -q main && echo stashed > a.txt && git stash -q && \
         echo edited > a.txt && echo new > 'new file.txt' && rm b.txt",
    );
    lab.sh(
        &root,
        "git init -q -b main receiver && cd receiver && git fetch -q ../device main && \
         git switch -qc main FETCH_HEAD",
    );
    (lab.path("device"), lab.path("receiver"))
}

#[test]
fn send_pushes_what_the_old_push_did_and_leaves_the_device_as_it_was() {
    let lab = Lab::new("send");
    let (device, _) = device_and_receiver(&lab);
    let rev = |name: &str| lab.sh(&device, &format!("git rev-parse {name}"));
    let (feature, topic, stash) = (rev("feature"), rev("topic"), rev("stash@{0}"));
    let before = lab.state(&device);
    let old = lab.copy("receiver", "old");
    let new = lab.copy("receiver", "new");
    let prefix = "refs/carry/mini/op1";
    let said = lab.sh(
        &device,
        &reference_send(
            prefix,
            &old,
            &[&feature, &topic],
            &[&stash],
            "wip(mini): carried",
        ),
    );
    assert_eq!(said, format!("pushed to {prefix}"));
    assert_eq!(lab.state(&device), before);
    let new_path = new.display().to_string();
    let (ok, said) = lab.gitdcy(
        &device,
        &[
            "carry",
            "send",
            &new_path,
            "--from",
            "mini",
            "--operation",
            "op1",
            "--tip",
            &feature,
            "--tip",
            &topic,
            "--stash",
            &stash,
            "--worktree",
            "--message",
            "wip(mini): carried",
        ],
    );
    assert!(ok, "{said}");
    assert_eq!(said, format!("pushed to {prefix}"));
    assert_eq!(lab.state(&device), before, "the device was written to");

    let refs = |dir: &Path| {
        lab.sh(
            dir,
            "git for-each-ref --format='%(refname) %(objectname)' refs/carry/ | grep -v /wip",
        )
    };
    assert_eq!(refs(&old), refs(&new));
    assert!(refs(&new).contains("refs/carry/mini/op1/heads/feature"));
    assert!(refs(&new).contains("refs/carry/mini/op1/stash/0"));
    // The working-tree commit: same tree, parent and message.
    let wip = |dir: &Path| {
        lab.sh(
            dir,
            "git log -1 --format='%T %P %B' refs/carry/mini/op1/wip",
        )
    };
    assert_eq!(wip(&old), wip(&new));
    assert_eq!(
        lab.sh(&new, "git show refs/carry/mini/op1/wip:'new file.txt'"),
        "new"
    );
}

#[test]
fn send_refuses_what_it_cannot_carry() {
    let lab = Lab::new("refuse");
    let (device, receiver) = device_and_receiver(&lab);
    let to = receiver.display().to_string();
    let send = |args: &[&str]| {
        let mut all = vec!["carry", "send", to.as_str()];
        all.extend_from_slice(args);
        lab.gitdcy(&device, &all)
    };
    let (ok, said) = send(&["--from", "mini", "--operation", "op1"]);
    assert!(!ok && said.contains("nothing to carry"), "{said}");
    let (ok, said) = send(&["--from", "../x", "--operation", "op1", "--worktree"]);
    assert!(!ok && said.contains("not a plain name"), "{said}");
    let (ok, said) = send(&["--from", "mini", "--operation", "op1", "--tip", "nosuch"]);
    assert!(!ok && said.contains("not a commit"), "{said}");
    assert_eq!(lab.sh(&receiver, "git for-each-ref refs/carry/"), "");
}

#[test]
fn place_does_what_the_old_placement_did_in_every_case() {
    let lab = Lab::new("place");
    let (device, receiver) = device_and_receiver(&lab);
    let head = lab.sh(&device, "git rev-parse HEAD");
    let rev = |name: &str| lab.sh(&device, &format!("git rev-parse {name}"));
    let (feature, topic, stash) = (rev("feature"), rev("topic"), rev("stash@{0}"));
    let to = receiver.display().to_string();
    let (ok, said) = lab.gitdcy(
        &device,
        &[
            "carry",
            "send",
            &to,
            "--from",
            "mini",
            "--operation",
            "op1",
            "--tip",
            &feature,
            "--tip",
            &topic,
            "--stash",
            &stash,
            "--worktree",
        ],
    );
    assert!(ok, "{said}");
    let message = "carried from mini";
    let reference = reference_place("refs/carry/mini/op1", "mini", "op1", &head, message);
    let place = [
        "carry",
        "place",
        "--from",
        "mini",
        "--operation",
        "op1",
        "--message",
        message,
    ];

    // Each case: what the receiving checkout looks like before the placement.
    let cases: &[(&str, &str, &[&str])] = &[
        (
            "free",
            "true",
            &[
                "branch feature",
                "branch topic",
                "stash",
                "applied the working tree",
            ],
        ),
        (
            "names-taken",
            "git branch feature HEAD && git branch topic HEAD && git branch carry/mini/topic HEAD",
            &[
                "branch carry/mini/feature",
                "kept refs/carry/mini/op1/heads/topic",
            ],
        ),
        (
            "same-branch",
            &format!("git branch feature {feature}"),
            &["branch topic"],
        ),
        (
            "dirty",
            "echo mine > a.txt",
            &["branch carry/mini/wip-op1 (successor checkout differs)"],
        ),
        (
            "untracked-only",
            "echo stray > stray.txt",
            &["branch carry/mini/wip-op1 (successor checkout differs)"],
        ),
        (
            "other-commit",
            "git commit -q --allow-empty -m other",
            &["branch carry/mini/wip-op1 (successor checkout differs)"],
        ),
        (
            "wip-name-taken",
            "echo mine > a.txt && git branch carry/mini/wip-op1 HEAD",
            &["kept refs/carry/mini/op1/wip"],
        ),
        (
            "stash-there",
            &format!("git stash store -m earlier {stash}"),
            &["branch feature", "applied the working tree"],
        ),
    ];
    for (name, before, expected) in cases {
        let prepared = lab.copy("receiver", &format!("{name}-prepared"));
        lab.sh(&prepared, before);
        let old = lab.copy(&format!("{name}-prepared"), &format!("{name}-old"));
        let new = lab.copy(&format!("{name}-prepared"), &format!("{name}-new"));
        let old_said = lab.sh(&old, &reference);
        let (ok, new_said) = lab.gitdcy(&new, &place);
        assert!(ok, "{name}: {new_said}");
        assert_eq!(new_said, old_said, "{name}: the lines differ");
        assert_eq!(
            lab.state(&new),
            lab.state(&old),
            "{name}: the checkouts differ"
        );
        for line in *expected {
            assert!(
                new_said.lines().any(|said| said.starts_with(line)),
                "{name}: no {line:?} in {new_said:?}"
            );
        }
        // Twice: nothing is placed again, the same way.
        let old_again = lab.sh(&old, &reference);
        let (_, new_again) = lab.gitdcy(&new, &place);
        assert_eq!(new_again, old_again, "{name}: the second run differs");
        assert_eq!(lab.state(&new), lab.state(&old), "{name}: second run");
    }
    // The clean case at the same commit has the device's work, unstaged.
    let free = lab.path("free-new");
    assert_eq!(fs::read_to_string(free.join("a.txt")).unwrap(), "edited\n");
    assert_eq!(
        fs::read_to_string(free.join("new file.txt")).unwrap(),
        "new\n"
    );
    assert!(!free.join("b.txt").exists());
    assert_eq!(
        lab.sh(&free, "git stash list --format='%H %gs'"),
        format!("{stash} {message}")
    );
}

#[test]
fn fast_forward_does_what_the_old_one_did() {
    let lab = Lab::new("forward");
    let root = lab.path("");
    lab.sh(
        &root,
        "git init -q -b main device && cd device && echo one > a.txt && git add a.txt && \
         git commit -qm one && cd .. && git init -q -b main receiver && cd receiver && \
         git fetch -q ../device main && git switch -qc main FETCH_HEAD && \
         cd ../device && echo two > a.txt && git commit -qam two",
    );
    let device = lab.path("device");
    let head = lab.sh(&device, "git rev-parse HEAD");
    let to = lab.path("receiver").display().to_string();
    let (ok, said) = lab.gitdcy(
        &device,
        &[
            "carry",
            "send",
            &to,
            "--from",
            "mini",
            "--operation",
            "op2",
            "--head",
            "HEAD",
        ],
    );
    assert!(ok, "{said}");
    let reference = reference_fast_forward("refs/carry/mini/op2", "main", &head);
    let forward = [
        "carry",
        "place",
        "--from",
        "mini",
        "--operation",
        "op2",
        "--fast-forward",
        "main",
    ];
    let cases = [
        ("behind", "true", "fast-forwarded to"),
        (
            "ahead",
            &format!("git merge -q --ff-only {head} && git commit -q --allow-empty -m three"),
            "already ahead",
        ),
        (
            "same",
            &format!("git merge -q --ff-only {head}"),
            "already ahead",
        ),
        (
            "no-commits-there",
            "git switch -q --orphan main2 && git branch -D main && git branch -m main2 main",
            "diverged",
        ),
        (
            "diverged",
            "echo other > b.txt && git add b.txt && git commit -qm other",
            "diverged",
        ),
        ("elsewhere", "git switch -qc other", "not on main"),
        ("in-the-way", "echo mine > a.txt", "fast-forward refused"),
    ];
    for (name, before, expected) in cases {
        let prepared = lab.copy("receiver", &format!("{name}-prepared"));
        lab.sh(&prepared, before);
        let old = lab.copy(&format!("{name}-prepared"), &format!("{name}-old"));
        let new = lab.copy(&format!("{name}-prepared"), &format!("{name}-new"));
        let old_said = lab.sh(&old, &reference);
        let (ok, new_said) = lab.gitdcy(&new, &forward);
        assert!(ok, "{name}: {new_said}");
        assert_eq!(new_said, old_said, "{name}");
        assert!(new_said.starts_with(expected), "{name}: {new_said}");
        assert_eq!(lab.state(&new), lab.state(&old), "{name}");
    }
    // Nothing carried: it says so and changes nothing.
    let (ok, said) = lab.gitdcy(
        &lab.path("behind-new"),
        &[
            "carry",
            "place",
            "--from",
            "mini",
            "--operation",
            "none",
            "--fast-forward",
            "main",
        ],
    );
    assert!(!ok && said.contains("nothing was carried"), "{said}");
}

#[test]
fn aside_keeps_the_work_under_its_ref_like_the_old_stash_step() {
    let lab = Lab::new("aside");
    let (device, _) = device_and_receiver(&lab);
    lab.sh(&device, "git stash drop -q");
    let old = lab.copy("device", "old");
    let new = lab.copy("device", "new");
    let reference = "refs/x/switched/desk-1";
    let (ok, _) = lab.try_sh(
        &old,
        &format!(
            "set -eu\ngit stash push --include-untracked -m 'switch to desk' >/dev/null\ngit update-ref -m 'switch to desk' {reference} 'stash@{{0}}'\ngit stash drop -q 'stash@{{0}}'\necho saved\n"
        ),
    );
    assert!(ok);
    let (ok, said) = lab.gitdcy(
        &new,
        &[
            "carry",
            "aside",
            "--as",
            reference,
            "--message",
            "switch to desk",
        ],
    );
    assert!(ok, "{said}");
    let kept = |dir: &Path| {
        lab.sh(
            dir,
            &format!(
                "git status --porcelain --untracked-files=all && git stash list && \
                 git rev-parse {reference}^{{tree}} {reference}^2^{{tree}} {reference}^3^{{tree}} && \
                 git log -1 --format=%s {reference}"
            ),
        )
    };
    assert_eq!(kept(&new), kept(&old));
    assert_eq!(fs::read_to_string(new.join("a.txt")).unwrap(), "base\n");
    lab.sh(&new, &format!("git stash apply -q {reference}"));
    assert_eq!(
        fs::read_to_string(new.join("new file.txt")).unwrap(),
        "new\n"
    );

    // Nothing to set aside: an earlier stash is not taken in its place.
    let clean = lab.copy("receiver", "clean");
    lab.sh(&clean, "echo s > a.txt && git stash -q");
    let stashes = lab.sh(&clean, "git stash list --format=%H");
    let (ok, said) = lab.gitdcy(&clean, &["carry", "aside", "--as", reference]);
    assert!(!ok && said.contains("no local changes"), "{said}");
    assert_eq!(lab.sh(&clean, "git stash list --format=%H"), stashes);
    assert_eq!(lab.sh(&clean, "git for-each-ref refs/x/"), "");
    let (ok, said) = lab.gitdcy(&new, &["carry", "aside", "--as", "refs/heads/main"]);
    assert!(!ok && said.contains("not a ref of its own"), "{said}");
}

#[test]
fn relate_answers_in_json_for_scripts() {
    let lab = Lab::new("relate");
    let root = lab.path("");
    lab.sh(
        &root,
        "git init -q -b main repo && cd repo && git commit -q --allow-empty -m one && \
         git commit -q --allow-empty -m two",
    );
    let repo = lab.path("repo");
    let (ok, said) = lab.gitdcy(&repo, &["relate", "HEAD~1", "HEAD", "--format", "json"]);
    assert!(ok, "{said}");
    let value: serde_json::Value = serde_json::from_str(&said).unwrap();
    assert_eq!(value["schema_version"], "gitdcy.response.v1");
    let relation = &value["data"]["relations"][0];
    assert_eq!(relation["relation"], "behind");
    assert_eq!(
        (relation["ahead"].as_u64(), relation["behind"].as_u64()),
        (Some(0), Some(1))
    );
    let output = lab
        .command(env!("CARGO_BIN_EXE_gitdcy"), &repo)
        .args(["relate", "--stdin", "--list", "5", "--format", "json"])
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .spawn()
        .and_then(|mut child| {
            use std::io::Write;
            child.stdin.take().unwrap().write_all(
                format!(
                    "{}\tHEAD\tHEAD~1\n/nonexistent\tHEAD\tHEAD\n",
                    repo.display()
                )
                .as_bytes(),
            )?;
            child.wait_with_output()
        })
        .unwrap();
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    let relations = value["data"]["relations"].as_array().unwrap();
    assert_eq!(relations[0]["relation"], "ahead");
    assert_eq!(relations[0]["only_a"][0]["subject"], "two");
    assert_eq!(relations[1]["relation"], "missing");
    assert_eq!(value["outcome"], "partial");
    let (ok, said) = lab.gitdcy(&repo, &["relate", "HEAD"]);
    assert!(!ok && said.contains("name two commits"), "{said}");
}
