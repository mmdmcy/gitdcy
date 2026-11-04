//! `gitdcy rules`: which rule applies to a repository, installing it into the
//! checkout, the check the pre-push hook runs, and the scan of every checkout.

use crate::status::{print_json, OutputFormat};
use anyhow::{Context, Result};
use clap::{Args, Subcommand};
use gitdcy_core::hygiene::Reminder;
use gitdcy_core::rules::{
    self, parse_pushed_refs, CheckReport, CheckRequest, InstallReport, RuleSource, RulesReport,
};
use gitdcy_core::scan::{self, CheckoutScan, ScanReport};
use serde::Serialize;
use std::io::Read;
use std::path::PathBuf;

#[derive(Debug, Args)]
#[command(after_help = "Examples:
  gitdcy rules                      which rule applies to this repository, and why
  gitdcy rules --format json        the same, for scripts
  gitdcy rules install              hook and a copy of the rule into .git/
  gitdcy rules check                what a push of this branch would be refused for
  gitdcy rules scan                 every checkout in the workspace against the rules
  gitdcy rules scan ~/code/app --since 2026-10-24

The rules file is the one given with --rules, else rules_file in GitDCY's
local.yaml, else rules.toml in GitDCY's config folder. With a [hygiene] table
in it, scan and check also remind of work left lying around: a dirty tree or a
pull not done, commits on no remote from before today, a branch not merged
after branch_days (3), and open pull requests (GitHub's, through gh).")]
pub(crate) struct RulesArgs {
    #[command(subcommand)]
    command: Option<RulesCommand>,
    /// The rules file.
    #[arg(long, global = true, value_name = "FILE")]
    rules: Option<PathBuf>,
    /// The repository (default: the current folder).
    #[arg(long, global = true, value_name = "PATH")]
    repo: Option<PathBuf>,
    #[arg(long, value_enum, default_value = "human", global = true)]
    format: OutputFormat,
}

#[derive(Debug, Subcommand)]
enum RulesCommand {
    /// Write a pre-push hook and a copy of this repository's rule into its
    /// git folder, so the rule holds on a machine without the rules file.
    Install,
    /// Refuse a push the rule does not allow, and read the commits it sends
    /// for personal data (what the pre-push hook runs).
    Check {
        /// Read the refs being pushed from standard input, as git gives them to a pre-push hook.
        #[arg(long)]
        stdin: bool,
        /// The remote's name (default: origin).
        remote: Option<String>,
        /// The remote's URL (default: its push URL).
        url: Option<String>,
    },
    /// Every checkout against the rules, read only: its rule, the remotes the
    /// rule forbids, personal data in what already reached a remote, and the
    /// reminders about work left lying around.
    Scan {
        /// Checkouts to read (default: --repo, else every repository in the workspace).
        #[arg(value_name = "PATH")]
        paths: Vec<String>,
        /// Read what reached a remote since then (git's --since: 2026-10-24, "3 days ago"; default 7 days).
        #[arg(long, value_name = "DATE")]
        since: Option<String>,
    },
}

/// Runs `gitdcy rules ...`; the exit code is 1 when a push is refused or the
/// command failed (`--format json` prints the envelope in both cases).
pub(crate) fn run(args: RulesArgs) -> Result<()> {
    let rules_path = rules::rules_file_path(args.rules.as_deref());
    let repo = args.repo.clone().unwrap_or_else(|| PathBuf::from("."));
    let gitdcy = std::env::current_exe().ok();
    let json = matches!(args.format, OutputFormat::Json);
    let failed = match args.command {
        None => {
            let report =
                rules_path.and_then(|path| rules::rules_report(&path, &repo, gitdcy.as_deref()));
            finish(
                "rules",
                report,
                json,
                |r| r.warnings.clone(),
                print_report,
                |_| false,
            )?
        }
        Some(RulesCommand::Install) => {
            let report =
                rules_path.and_then(|path| rules::install(&path, &repo, gitdcy.as_deref()));
            finish(
                "rules.install",
                report,
                json,
                |_| Vec::new(),
                print_install,
                |_| false,
            )?
        }
        Some(RulesCommand::Check { stdin, remote, url }) => {
            let request = check_request(stdin, remote, url);
            let hook = stdin;
            let report = rules_path
                .and_then(|path| request.and_then(|request| rules::check(&path, &repo, &request)));
            finish(
                "rules.check",
                report,
                json,
                |_| Vec::new(),
                |r| print_check(r, hook),
                |r| r.refused,
            )?
        }
        Some(RulesCommand::Scan { paths, since }) => {
            let paths = match (paths.is_empty(), &args.repo) {
                (true, Some(repo)) => vec![repo.display().to_string()],
                _ => paths,
            };
            let report = rules_path.and_then(|path| {
                let paths = crate::checkouts::given_or_workspace(paths)?;
                scan::scan(&path, &paths, since.as_deref(), gitdcy.as_deref(), "gh")
            });
            finish(
                "rules.scan",
                report,
                json,
                scan_warnings,
                print_scan,
                |_| false,
            )?
        }
    };
    if failed {
        std::process::exit(1);
    }
    Ok(())
}

fn check_request(stdin: bool, remote: Option<String>, url: Option<String>) -> Result<CheckRequest> {
    let refs = if stdin {
        let mut text = String::new();
        std::io::stdin()
            .read_to_string(&mut text)
            .context("reading the refs being pushed from standard input")?;
        Some(parse_pushed_refs(&text)?)
    } else {
        None
    };
    Ok(CheckRequest { remote, url, refs })
}

/// Prints the result; true when the command should exit 1.
fn finish<T: Serialize>(
    operation: &'static str,
    result: Result<T>,
    json: bool,
    warnings: impl Fn(&T) -> Vec<String>,
    human: impl Fn(&T),
    refused: impl Fn(&T) -> bool,
) -> Result<bool> {
    if json {
        let warnings = result.as_ref().map(&warnings).unwrap_or_default();
        print_json(operation, &result, warnings)?;
        return Ok(result.as_ref().map_or(true, refused));
    }
    let report = result?;
    human(&report);
    Ok(refused(&report))
}

fn source_label(source: Option<RuleSource>) -> &'static str {
    match source {
        Some(RuleSource::RulesFile) => "the rules file",
        Some(RuleSource::InstalledCopy) => "the copy in the git folder",
        None => "",
    }
}

fn print_report(report: &RulesReport) {
    println!("{}", report.repository);
    match &report.rule {
        Some(rule) => {
            println!(
                "  rule: {} (from {}; matched by {})",
                rule.name,
                source_label(report.source),
                report.matched_by.join(", ")
            );
            println!("  push: {}", report.push.as_deref().unwrap_or("anywhere"));
            if !rule.protected.is_empty() {
                println!(
                    "  only through a pull request: {}",
                    rule.protected.join(", ")
                );
            }
            if !rule.note.trim().is_empty() {
                println!("  note: {}", rule.note.trim());
            }
        }
        None if !report.conflicts.is_empty() => {
            println!("  rules that all match: {}", report.matched_by.join("; "));
        }
        None => {
            println!("  no rule: pushes may go anywhere; the personal-data check still runs");
            if let Some(first) = report.first_commits.first() {
                println!(
                    "  first commit: {first} (first_commit in a [[repo]] of {} gives it a rule)",
                    report.rules_file
                );
            }
        }
    }
    for remote in &report.remotes {
        println!(
            "  {} -> {}: {}",
            remote.name,
            remote.url.as_deref().unwrap_or("(no URL)"),
            if remote.allowed {
                "allowed"
            } else {
                "NOT allowed"
            }
        );
    }
    if report.rule.is_some() {
        println!(
            "  hook: {}; copy of the rule: {}",
            if report.hook.installed && report.hook.current && report.hook.runs {
                "installed"
            } else if report.hook.installed {
                "installed, see below"
            } else {
                "not installed"
            },
            match (report.copy.present, report.copy.current) {
                (true, Some(true)) => "current",
                (true, _) => "out of date",
                (false, _) => "not installed",
            }
        );
    }
    println!(
        "  personal-data check: private mail addresses{}{}",
        if report.personal_words == 0 {
            String::new()
        } else {
            format!(" and {} personal word(s)", report.personal_words)
        },
        if report.allowed_addresses == 0 {
            String::new()
        } else {
            format!("; {} address(es) allowed", report.allowed_addresses)
        }
    );
    for warning in &report.warnings {
        println!("  ! {warning}");
    }
}

fn print_install(report: &InstallReport) {
    println!("{}: rule {}", report.repository, report.rule);
    println!(
        "  copy {}: {}",
        report.copy,
        if report.copy_written {
            "written"
        } else {
            "already current"
        }
    );
    println!(
        "  hook {}: {}",
        report.hook,
        if report.hook_written {
            "written"
        } else {
            "already current"
        }
    );
    for kept in &report.kept {
        println!("  kept the previous file as {kept}");
    }
    if let Some(chained) = &report.chained {
        println!("  the hook that was there ({chained}) runs after the check");
    }
}

/// In the hook a clean push prints only its reminders; by hand, it says so.
/// The reminders go to standard error and never refuse a push.
fn print_check(report: &CheckReport, hook: bool) {
    if !report.refused {
        if !hook {
            println!(
                "{}: {} commit(s) in {}{}: clean",
                report.repository,
                report.commits,
                report.range,
                report
                    .rule
                    .as_deref()
                    .map(|r| format!(", rule {r}"))
                    .unwrap_or_default()
            );
        }
    } else {
        let _ = write_check(&mut std::io::stderr(), report);
    }
    if let Some(line) = reminder_line(&report.reminders) {
        eprintln!(
            "gitdcy rules: left lying around in {}: {line}",
            report.repository
        );
    }
}

fn reminder_line(reminders: &[Reminder]) -> Option<String> {
    (!reminders.is_empty()).then(|| {
        reminders
            .iter()
            .map(|r| r.text.as_str())
            .collect::<Vec<_>>()
            .join("; ")
    })
}

/// What could not be read as asked.
fn scan_warnings(report: &ScanReport) -> Vec<String> {
    let mut warnings: Vec<String> = report
        .checkouts
        .iter()
        .filter(|c| c.state != "ok" || !c.detail.is_empty())
        .map(|c| format!("{}: {}", c.path, c.detail))
        .collect();
    if !report.rules_file_found {
        warnings.push(format!(
            "no rules file at {}: no repository has a rule, and there are no reminders",
            report.rules_file
        ));
    }
    warnings
}

/// Whether a checkout has anything to say.
fn notable(c: &CheckoutScan) -> bool {
    c.state != "ok"
        || c.rule.is_some()
        || !c.conflicts.is_empty()
        || !c.forbidden.is_empty()
        || !c.pushed_findings.is_empty()
        || c.hygiene.as_ref().is_some_and(|h| !h.reminders.is_empty())
}

/// The checkouts with something to say, then a count of the rest.
fn print_scan(report: &ScanReport) {
    let pushed: usize = report.checkouts.iter().map(|c| c.pushed_commits).sum();
    println!(
        "{} checkout(s) against {}; {} commit(s) that reached a remote since {} read for personal data{}",
        report.checkouts.len(),
        report.rules_file,
        pushed,
        report.since,
        if report.hygiene.is_some() {
            ""
        } else {
            "; no reminders (the rules file has no [hygiene])"
        }
    );
    let mut quiet = 0;
    for c in &report.checkouts {
        if !notable(c) {
            quiet += 1;
            continue;
        }
        let rule = match (&c.rule, &c.push) {
            (Some(rule), Some(push)) => format!(": rule {rule} (push {push})"),
            (Some(rule), None) => format!(": rule {rule}"),
            _ => String::new(),
        };
        println!("  {}{rule}", c.path);
        if c.state != "ok" {
            println!("    {}: {}", c.state, c.detail);
            continue;
        }
        if !c.conflicts.is_empty() {
            println!(
                "    ! rules {} all match it: its pushes are refused until one is left",
                c.conflicts.join(", ")
            );
        }
        for remote in &c.forbidden {
            println!(
                "    ! remote {} -> {}: its rule does not allow it, a push there is refused",
                remote.name,
                remote.url.as_deref().unwrap_or("(no URL)")
            );
        }
        if let Some(hook) = c.hook.as_ref().filter(|hook| !hook.runs) {
            println!(
                "    ! git runs hooks from {} and the pre-push there does not run the check",
                hook.hooks_path.as_deref().unwrap_or("elsewhere")
            );
        }
        for finding in &c.pushed_findings {
            println!(
                "    ! pushed commit {} {}: {} ({})",
                finding.commit,
                finding.place,
                finding.kind.label(),
                finding.shown
            );
        }
        for reminder in c.hygiene.iter().flat_map(|h| &h.reminders) {
            println!("    - {}: {}", reminder.text, reminder.fix);
        }
    }
    if quiet > 0 {
        println!("  {quiet} more checkout(s): no rule and nothing left lying around");
    }
}

fn write_check(out: &mut impl std::io::Write, report: &CheckReport) -> std::io::Result<()> {
    writeln!(
        out,
        "gitdcy rules: push from {} to {} REFUSED",
        report.repository,
        report
            .url
            .as_deref()
            .or(report.remote.as_deref())
            .unwrap_or("(no remote)")
    )?;
    for finding in &report.findings {
        writeln!(
            out,
            "  commit {} {}: {} ({})",
            finding.commit,
            finding.place,
            finding.kind.label(),
            finding.shown
        )?;
    }
    for reason in &report.reasons {
        writeln!(out, "  {reason}")?;
    }
    Ok(())
}
