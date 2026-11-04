//! `gitdcy rules`: which rule applies to a repository, installing it into the
//! checkout, and the check the pre-push hook runs.

use crate::status::{correlation_id, OutputFormat};
use anyhow::{Context, Result};
use clap::{Args, Subcommand};
use gitdcy_core::rules::{
    self, parse_pushed_refs, CheckReport, CheckRequest, InstallReport, RuleSource, RulesReport,
};
use serde::Serialize;
use std::io::Read;
use std::path::PathBuf;

#[derive(Debug, Args)]
#[command(after_help = "Examples:
  gitdcy rules                      which rule applies to this repository, and why
  gitdcy rules --format json        the same, for scripts
  gitdcy rules install              hook and a copy of the rule into .git/
  gitdcy rules check                what a push of this branch would be refused for

The rules file is the one given with --rules, else rules_file in GitDCY's
local.yaml, else rules.toml in GitDCY's config folder.")]
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
}

#[derive(Serialize)]
struct Envelope<T: Serialize> {
    schema_version: &'static str,
    component: &'static str,
    operation: &'static str,
    outcome: &'static str,
    data: Option<T>,
    warnings: Vec<String>,
    error: Option<ErrorBody>,
    correlation_id: String,
}

#[derive(Serialize)]
struct ErrorBody {
    message: String,
}

fn print_json<T: Serialize>(
    operation: &'static str,
    result: &Result<T>,
    warnings: Vec<String>,
) -> Result<()> {
    let envelope = Envelope {
        schema_version: "linuxmice.response.v1",
        component: "katteke.gitdcy",
        operation,
        outcome: match (result, warnings.is_empty()) {
            (Err(_), _) => "error",
            (Ok(_), true) => "success",
            (Ok(_), false) => "partial",
        },
        data: result.as_ref().ok(),
        warnings,
        error: result.as_ref().err().map(|error| ErrorBody {
            message: format!("{error:#}"),
        }),
        correlation_id: correlation_id(),
    };
    println!("{}", serde_json::to_string_pretty(&envelope)?);
    Ok(())
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

/// In the hook a clean push prints nothing; by hand, it says so.
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
        return;
    }
    let out = &mut std::io::stderr();
    let _ = write_check(out, report);
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
