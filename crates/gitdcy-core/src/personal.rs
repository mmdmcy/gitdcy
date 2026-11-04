//! The personal-data check on what a push sends.
//!
//! Every commit that would leave is read: its added lines, its message, and its
//! author and committer. Two things are found:
//!
//! - consumer mail addresses (gmail, hotmail, outlook, icloud, ...) that are not
//!   one of the repository's own git identities or on the owner's allow list;
//! - the owner's own words (names, phone numbers), from the rules file.
//!
//! The detectors here are generic; the words and the allow list are the
//! owner's, read from their private rules file, never from a repository. A
//! finding names the commit and the place and shows the value masked, so a
//! refusal does not print the data it refuses.

use super::git_output;
use anyhow::Result;
use serde::Serialize;
use std::collections::BTreeSet;
use std::path::Path;

/// Mail providers people use privately; a repository has no business gaining
/// one of these unless it is the committer's own identity.
const CONSUMER_MAIL: &[&str] = &[
    "gmail.com",
    "googlemail.com",
    "hotmail.com",
    "hotmail.nl",
    "hotmail.be",
    "outlook.com",
    "outlook.be",
    "live.com",
    "live.nl",
    "live.be",
    "icloud.com",
    "me.com",
    "yahoo.com",
    "yahoo.nl",
    "ymail.com",
    "protonmail.com",
    "proton.me",
    "pm.me",
    "gmx.com",
    "gmx.de",
    "gmx.net",
    "ziggo.nl",
    "kpnmail.nl",
    "hetnet.nl",
    "planet.nl",
    "home.nl",
    "xs4all.nl",
    "upcmail.nl",
    "telfort.nl",
    "casema.nl",
    "quicknet.nl",
    "telenet.be",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum FindingKind {
    ConsumerAddress,
    PersonalWord,
}

impl FindingKind {
    pub fn label(self) -> &'static str {
        match self {
            Self::ConsumerAddress => "a private mail address",
            Self::PersonalWord => "a word from the personal list",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Finding {
    /// Abbreviated commit hash.
    pub commit: String,
    /// `file:line`, `message`, `author` or `committer`.
    pub place: String,
    pub kind: FindingKind,
    /// The value, masked: enough to recognise it, not enough to spread it.
    pub shown: String,
}

/// The owner's own words and the addresses that belong in commits.
#[derive(Debug, Clone, Default)]
pub struct ScanConfig {
    pub words: Vec<String>,
    pub allow: Vec<String>,
}

/// A private address -> its first letter, `…@` and its domain; a word -> its
/// first letter and its place in the list.
fn mask(kind: FindingKind, value: &str, word_index: usize) -> String {
    let first: String = value.chars().take(1).collect();
    match kind {
        FindingKind::ConsumerAddress => {
            let domain = value.rsplit_once('@').map(|(_, d)| d).unwrap_or("");
            format!("{first}…@{domain}")
        }
        FindingKind::PersonalWord => {
            format!("{first}… (word {} of the personal list)", word_index + 1)
        }
    }
}

/// The mail address around the `@` at `at` in `line`, when it looks like one.
fn address_at(line: &str, at: usize) -> Option<&str> {
    let local = |c: char| c.is_ascii_alphanumeric() || "._%+-".contains(c);
    let domain = |c: char| c.is_ascii_alphanumeric() || ".-".contains(c);
    let start = line[..at]
        .char_indices()
        .rev()
        .take_while(|(_, c)| local(*c))
        .last()
        .map(|(i, _)| i)?;
    let end = at
        + 1
        + line[at + 1..]
            .char_indices()
            .take_while(|(_, c)| domain(*c))
            .last()
            .map(|(i, c)| i + c.len_utf8())?;
    let address = line[start..end].trim_end_matches('.');
    address.contains('@').then_some(address)
}

/// Everything personal in one line: consumer mail addresses not in `allowed`
/// and the configured words, each once, masked.
pub fn personal_in(
    line: &str,
    words: &[String],
    allowed: &BTreeSet<String>,
) -> Vec<(FindingKind, String)> {
    let mut seen = Vec::new();
    let mut out = Vec::new();
    let lower = line.to_lowercase();
    for (at, _) in lower.match_indices('@') {
        let Some(address) = address_at(&lower, at) else {
            continue;
        };
        let domain = address.rsplit_once('@').map(|(_, d)| d).unwrap_or("");
        if CONSUMER_MAIL.contains(&domain)
            && !allowed.contains(address)
            && !seen.contains(&address.to_string())
        {
            seen.push(address.to_string());
            out.push((
                FindingKind::ConsumerAddress,
                mask(FindingKind::ConsumerAddress, address, 0),
            ));
        }
    }
    for (index, word) in words.iter().enumerate() {
        let word = word.trim().to_lowercase();
        if !word.is_empty() && lower.contains(&word) && !seen.contains(&word) {
            out.push((
                FindingKind::PersonalWord,
                mask(FindingKind::PersonalWord, &word, index),
            ));
            seen.push(word);
        }
    }
    out
}

/// The allow list plus the repository's own git identities (local and global).
fn own_identities(repo: &Path, allow: &[String]) -> BTreeSet<String> {
    let mut set: BTreeSet<String> = allow.iter().map(|a| a.trim().to_lowercase()).collect();
    if let Ok(out) = git_output(repo, ["config", "--get-all", "user.email"]) {
        set.extend(out.lines().map(|l| l.trim().to_lowercase()));
    }
    set
}

/// What to read when no refs are given: the commits not yet on the upstream,
/// or, without one, the commits on no remote.
pub fn default_range(repo: &Path) -> Vec<String> {
    for upstream in ["@{push}", "@{upstream}"] {
        if git_output(repo, ["rev-parse", "--verify", "-q", upstream]).is_ok() {
            return vec![format!("{upstream}..HEAD")];
        }
    }
    vec!["HEAD".into(), "--not".into(), "--remotes".into()]
}

/// Reads the commits in `range` (git log syntax) for personal data; returns
/// how many commits were read and what was found.
pub fn scan_commits(
    repo: &Path,
    range: &[String],
    config: &ScanConfig,
) -> Result<(usize, Vec<Finding>)> {
    let allowed = own_identities(repo, &config.allow);
    let mut args: Vec<String> = [
        "-c",
        "core.quotePath=false",
        "log",
        "-p",
        "-U0",
        "--no-color",
        "--no-renames",
        "--no-ext-diff",
        "--no-textconv",
        "--src-prefix=a/",
        "--dst-prefix=b/",
        "--format=%x1e%h%x1f%an <%ae>%x1f%cn <%ce>%x1f%B%x1f",
    ]
    .into_iter()
    .map(str::to_string)
    .collect();
    args.extend(range.iter().cloned());
    let log = git_output(repo, &args)?;
    let mut findings = Vec::new();
    let mut commits = 0;
    for record in log.split('\x1e').skip(1) {
        commits += 1;
        let mut parts = record.splitn(5, '\x1f');
        let (Some(hash), Some(author), Some(committer), Some(message), Some(diff)) = (
            parts.next(),
            parts.next(),
            parts.next(),
            parts.next(),
            parts.next(),
        ) else {
            continue;
        };
        let mut note = |place: String, text: &str| {
            for (kind, shown) in personal_in(text, &config.words, &allowed) {
                findings.push(Finding {
                    commit: hash.to_string(),
                    place: place.clone(),
                    kind,
                    shown,
                });
            }
        };
        note("author".into(), author);
        note("committer".into(), committer);
        note("message".into(), message);
        let mut path = String::new();
        let mut line_no = 0usize;
        for line in diff.lines() {
            if let Some(file) = line.strip_prefix("+++ b/") {
                path = file.to_string();
            } else if let Some(hunk) = line.strip_prefix("@@ ") {
                line_no = hunk
                    .split_whitespace()
                    .nth(1)
                    .and_then(|plus| plus.trim_start_matches('+').split(',').next())
                    .and_then(|n| n.parse().ok())
                    .unwrap_or(0);
            } else if let Some(added) = line.strip_prefix('+') {
                if !line.starts_with("+++") {
                    note(format!("{path}:{line_no}"), added);
                    line_no += 1;
                }
            }
        }
    }
    Ok((commits, findings))
}

#[cfg(test)]
mod tests {
    use super::*;

    // Addresses are put together here so this file holds none itself (the
    // check reads it too when it is pushed).
    fn at(local: &str, domain: &str) -> String {
        format!("{local}@{domain}")
    }

    #[test]
    fn consumer_addresses_and_words_are_found_masked() {
        let (me, someone, proton) = (
            at("me", "gmail.com"),
            at("Someone", "Gmail.com"),
            at("x", "proton.me"),
        );
        let allowed: BTreeSet<String> = [me.clone()].into();
        let words = vec!["Jane Doe".to_string()];
        assert_eq!(
            personal_in(
                &format!("signed in as {someone}, plan on {proton}."),
                &words,
                &allowed
            ),
            vec![
                (FindingKind::ConsumerAddress, "s…@gmail.com".to_string()),
                (FindingKind::ConsumerAddress, "x…@proton.me".to_string()),
            ]
        );
        assert_eq!(
            personal_in("met jane doe gisteren", &words, &allowed),
            vec![(
                FindingKind::PersonalWord,
                "j… (word 1 of the personal list)".to_string()
            )]
        );
    }

    #[test]
    fn own_identity_company_addresses_and_handles_are_not_found() {
        let me = at("me", "gmail.com");
        let allowed: BTreeSet<String> = [me.clone()].into();
        let words = vec!["Jane Doe".to_string()];
        assert!(personal_in(&format!("author {me}"), &words, &allowed).is_empty());
        let company = format!(
            "{} and {}",
            at("info", "example.com"),
            at("noreply", "github.com")
        );
        assert!(personal_in(&company, &words, &allowed).is_empty());
        assert!(personal_in("a lone @ sign and @handle", &words, &allowed).is_empty());
        assert!(personal_in("nothing here", &[" ".to_string()], &allowed).is_empty());
    }

    #[test]
    fn the_same_address_twice_on_a_line_is_one_finding() {
        let someone = at("someone", "hotmail.com");
        let found = personal_in(&format!("{someone} {someone}"), &[], &BTreeSet::new());
        assert_eq!(found.len(), 1);
    }
}
