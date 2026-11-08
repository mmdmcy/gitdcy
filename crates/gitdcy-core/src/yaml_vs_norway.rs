//! The YAML reader against serde_norway, the YAML crate it replaces: every
//! settings file serde_norway writes reads back to the same values, and hand-
//! written files read the same or are refused (never read differently).
use crate::{
    yaml, CheckCommand, CheckProfile, GitIdentityProfile, LocalConfig, Provider, RepoEntry,
    VisibilityOverride, WorkspaceManifest,
};
use std::collections::BTreeMap;
use std::path::PathBuf;

struct Random(u64);

impl Random {
    fn next(&mut self) -> u64 {
        self.0 = self
            .0
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        self.0 >> 33
    }

    fn pick<'a, T>(&mut self, items: &'a [T]) -> &'a T {
        &items[self.next() as usize % items.len()]
    }

    fn text(&mut self) -> String {
        const WHOLE: &[&str] = &[
            "",
            " ",
            "true",
            "True",
            "FALSE",
            "yes",
            "no",
            "on",
            "~",
            "null",
            "Null",
            "123",
            "-7",
            "+3",
            "0x1f",
            "0o17",
            "1e3",
            "1.5",
            ".inf",
            ".nan",
            "-",
            "- a",
            "a: b",
            "a:",
            ":a",
            "#x",
            "x #y",
            "'q'",
            "\"dq\"",
            " lead",
            "trail ",
            "?",
            "? x",
            "*star",
            "&amp",
            "!tag",
            "%pct",
            "@at",
            "`tick",
            "[x]",
            "{y}",
            ",",
            "a, b",
            "|",
            ">",
            "--all",
            "-D",
            "~/Code",
            "/home/u/Documents/github/app",
            "github.com/org/app",
            "C:\\Users\\u",
            "é漢🙂",
            "tab\there",
            "ctl\u{7}",
            "a\"b'c",
            "back\\slash",
        ];
        const CHARS: &[char] = &[
            'a', 'b', 'Z', '0', '9', ' ', ':', '#', '\'', '"', '-', '?', '[', ']', '{', '}', ',',
            '&', '*', '!', '|', '>', '%', '@', '`', '~', '\\', '/', '.', '\t', 'é', '漢', '🙂',
            '=', '+', '_',
        ];
        if self.next() % 3 == 0 {
            return self.pick(WHOLE).to_string();
        }
        let len = self.next() % 12;
        (0..len).map(|_| *self.pick(CHARS)).collect()
    }

    fn texts(&mut self) -> Vec<String> {
        (0..self.next() % 4).map(|_| self.text()).collect()
    }

    fn maybe<T>(&mut self, make: impl FnOnce(&mut Self) -> T) -> Option<T> {
        (self.next() % 3 != 0).then(|| make(self))
    }

    fn map<T>(&mut self, mut make: impl FnMut(&mut Self) -> T) -> BTreeMap<String, T> {
        (0..self.next() % 4)
            .map(|_| (self.text(), make(self)))
            .collect()
    }

    fn provider(&mut self) -> Provider {
        *self.pick(&[
            Provider::Github,
            Provider::Forgejo,
            Provider::Gitlab,
            Provider::Other,
        ])
    }

    fn local(&mut self) -> LocalConfig {
        LocalConfig {
            workspace_root: self.maybe(|r| PathBuf::from(r.text())),
            scan_roots: self.maybe(|r| r.texts().into_iter().map(PathBuf::from).collect()),
            sync_remote_template: self.maybe(Self::text),
            origin_remote_templates: self.maybe(|r| r.map(Self::text)),
            local_sync_files: self.maybe(|r| r.map(Self::texts)),
            trusted_wip_devices: self.maybe(|r| r.map(Self::texts)),
            visibility_overrides: self.maybe(|r| {
                r.map(|r| *r.pick(&[VisibilityOverride::Public, VisibilityOverride::Private]))
            }),
            private_remote_patterns: self.maybe(Self::texts),
            public_export_remotes: self.maybe(Self::texts),
            ignore_profiles: self.maybe(|r| r.map(Self::texts)),
            require_identity: self.maybe(|r| r.next() % 2 == 0),
            identity_profiles: self.maybe(|r| {
                r.map(|r| GitIdentityProfile {
                    name: r.text(),
                    email: r.text(),
                    path_prefixes: r.texts().into_iter().map(PathBuf::from).collect(),
                    remote_patterns: r.texts(),
                    providers: (0..r.next() % 3).map(|_| r.provider()).collect(),
                })
            }),
            require_checks: self.maybe(|r| r.next() % 2 == 0),
            check_profiles: self.maybe(|r| {
                r.map(|r| CheckProfile {
                    path_prefixes: r.texts().into_iter().map(PathBuf::from).collect(),
                    remote_patterns: r.texts(),
                    providers: (0..r.next() % 3).map(|_| r.provider()).collect(),
                    checks: (0..r.next() % 3)
                        .map(|_| CheckCommand {
                            name: r.text(),
                            program: r.text(),
                            args: r.texts(),
                            timeout_seconds: r.maybe(|r| r.next() % 5000),
                        })
                        .collect(),
                    run_before_push: r.next() % 2 == 0,
                    require_clean_worktree: r.next() % 2 == 0,
                })
            }),
            rules_file: self.maybe(|r| PathBuf::from(r.text())),
            config_error: None,
        }
    }

    fn manifest(&mut self) -> WorkspaceManifest {
        WorkspaceManifest {
            workspace_root: PathBuf::from(self.text()),
            repos: (0..self.next() % 5)
                .map(|_| RepoEntry {
                    id: self.text(),
                    path: PathBuf::from(self.text()),
                    provider: self.provider(),
                    enabled: self.next() % 2 == 0,
                    primary_remote: self.maybe(Self::text),
                    wip_sync: self.next() % 2 == 0,
                    review_required: self.next() % 2 == 0,
                })
                .collect(),
        }
    }
}

fn json<T: serde::Serialize>(value: &T) -> serde_json::Value {
    serde_json::to_value(value).unwrap()
}

#[test]
fn everything_serde_norway_writes_reads_back_the_same() {
    let mut random = Random(2026_11_08);
    let mut checked = 0;
    for _ in 0..4000 {
        let local = random.local();
        let text = serde_norway::to_string(&local).unwrap();
        let ours: LocalConfig =
            yaml::from_str(&text).unwrap_or_else(|e| panic!("{e}\n---\n{text}"));
        assert_eq!(json(&ours), json(&local), "\n{text}");
        let manifest = random.manifest();
        let text = serde_norway::to_string(&manifest).unwrap();
        let ours: WorkspaceManifest =
            yaml::from_str(&text).unwrap_or_else(|e| panic!("{e}\n---\n{text}"));
        assert_eq!(json(&ours), json(&manifest), "\n{text}");
        checked += 2;
    }
    assert_eq!(checked, 8000);
}

#[test]
fn a_value_over_several_lines_is_refused_not_misread() {
    let local = LocalConfig {
        sync_remote_template: Some("first\nsecond".into()),
        ..LocalConfig::default()
    };
    let text = serde_norway::to_string(&local).unwrap();
    assert!(yaml::from_str::<LocalConfig>(&text).is_err(), "{text}");
}

/// Hand-written files: read as serde_norway reads them, or refused.
#[test]
fn hand_written_files_read_the_same_or_are_refused() {
    let example = include_str!("../../../.gitdcy.local.example.yaml");
    // Every setting, written the way the example shows them.
    let full = "\
workspace_root: ~/Code
rules_file: ~/path/to/private/rules.toml
require_identity: true
identity_profiles:
  github-main:
    name: YOUR_GITHUB_NAME
    email: YOUR_GITHUB_NOREPLY_EMAIL
    path_prefixes:
      - ~/Documents/your-private-workspace/project-a
    remote_patterns:
      - github.com/your-account/project-a
  forgejo-selfhosted:
    name: YOUR_FORGEJO_NAME
    email: YOUR_FORGEJO_EMAIL
    providers:
      - forgejo
require_checks: false
check_profiles:
  rust-project:
    path_prefixes:
      - ~/Documents/your-workspace/project-a
    run_before_push: true
    require_clean_worktree: true
    checks:
      - name: format
        program: cargo
        args: [fmt, --all, --check]
      - name: clippy
        program: cargo
        args: [clippy, --all-targets, --all-features, --, -D, warnings]
        timeout_seconds: 900
scan_roots:
  - ~/Code
  - ~/Documents/github
sync_remote_template: \"ssh://git@example.internal/owner/{repo}.git\"
origin_remote_templates:
  github: \"https://github.com/YOUR_GITHUB_USERNAME/{repo}.git\"
  gitlab: \"git@gitlab.com:YOUR_GITLAB_USERNAME/{repo}.git\"
local_sync_files:
  github/my-app:
    - .env
    - .env.local
trusted_wip_devices:
  \"*\":
    - laptop-name
  github/my-app:
    - laptop-name
visibility_overrides:
  github/private-app: private
  gitlab/public-export: public
private_remote_patterns:
  - forgejo-easy
public_export_remotes:
  - public
ignore_profiles:
  \"*\":
    - .idea/
";
    let read: Vec<(&str, String)> = vec![
        ("example as shipped", example.to_string()),
        ("empty", String::new()),
        ("comments only", "# nothing\n\n".to_string()),
        ("every setting", full.to_string()),
        (
            "quotes and nulls",
            "workspace_root: '~/Code'\nsync_remote_template: \"ssh://git@h/o/{repo}.git\"\n\
             require_identity: True\nrequire_checks: ~\nrules_file: null\n"
                .to_string(),
        ),
        (
            "indented lists",
            "scan_roots:\n  - ~/Code   # main\n  - '~/Documents/github'\n\
             trusted_wip_devices:\n  \"*\": [laptop, 'desk top']\n  github/app:\n    - phone\n"
                .to_string(),
        ),
        (
            "a plain number as text",
            "sync_remote_template: 123\n".to_string(),
        ),
        (
            "an unknown setting",
            "colour: blue\nrules_file: /r.toml\n".to_string(),
        ),
        ("a document start", "---\nrules_file: /r.toml\n".to_string()),
    ];
    for (what, text) in &read {
        let theirs = serde_norway::from_str::<LocalConfig>(text);
        let ours = yaml::from_str::<LocalConfig>(text);
        match (&theirs, &ours) {
            (Ok(theirs), Ok(ours)) => assert_eq!(json(ours), json(theirs), "{what}:\n{text}"),
            _ => panic!(
                "{what}: serde_norway {:?}, ours {:?}\n{text}",
                theirs.as_ref().err().map(ToString::to_string),
                ours.as_ref().err().map(ToString::to_string)
            ),
        }
    }
    // What serde_norway reads and this reader refuses, on purpose.
    for text in [
        "base: &b /x\nworkspace_root: *b\n",
        "sync_remote_template: |\n  a\n",
        "origin_remote_templates: {github: x}\n",
        "rules_file: !!str /r\n",
    ] {
        assert!(
            serde_norway::from_str::<LocalConfig>(text).is_ok(),
            "{text}"
        );
        assert!(yaml::from_str::<LocalConfig>(text).is_err(), "{text}");
    }
}
