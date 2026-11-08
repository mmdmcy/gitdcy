use clap::builder::{EnumValueParser, PossibleValue};
use clap::{Arg, ArgAction, ArgMatches, ValueEnum};
use gitdcy_core::bail;
use gitdcy_core::error::{Context, Result};
use gitdcy_core::{
    apply_policy, audit_all, audit_repo, check_repo, clone_repo, commit, default_workspace_root,
    format_audit_block, install_global_ignore_template, load_local_config,
    load_or_discover_manifest, policy_all, policy_report, push, save_manifest, set_remote,
    set_suggested_origin_remote, set_wip_device_trusted, set_wip_device_trusted_globally,
    status_all, sync_repo, CheckResultState, CloneRequest, Provider, RepoCheckReport,
    RepoPolicyReport, RepoSafetyReport, RepoStatus,
};
use std::path::PathBuf;

mod carry;
mod checkouts;
mod dashboard;
mod relate;
mod rules;
mod status;

use status::{status, OutputFormat};

#[derive(Debug)]
enum Command {
    Doctor,
    Status {
        format: OutputFormat,
    },
    Check {
        all: bool,
        repo: Option<String>,
    },
    Dashboard,
    Identity {
        command: IdentityCommand,
    },
    Audit {
        all: bool,
        explain: bool,
        repo: Option<String>,
    },
    Policy {
        command: PolicyCommand,
    },
    InstallIgnore {
        global: bool,
    },
    Sync {
        all: bool,
        repo: Option<String>,
    },
    Commit {
        repo: String,
        message: String,
    },
    Push {
        repo: String,
    },
    SetSyncRemote {
        repo: String,
        url: String,
    },
    SetOriginRemote {
        repo: String,
        url: Option<String>,
    },
    TrustDevice {
        device: String,
        repo: Option<String>,
        all: bool,
    },
    Clone {
        url: String,
        root: Option<PathBuf>,
        provider: Option<ProviderArg>,
        name: Option<String>,
    },
    SaveManifest,
    Rules(rules::RulesArgs),
    Checkouts(checkouts::CheckoutsArgs),
    Relate(relate::RelateArgs),
    Carry(carry::CarryArgs),
}

#[derive(Debug)]
enum PolicyCommand {
    Status { all: bool, repo: Option<String> },
    Plan { repo: String },
    Apply { repo: String },
}

#[derive(Debug)]
enum IdentityCommand {
    Status { all: bool, repo: Option<String> },
}

#[derive(Debug, Clone, Copy)]
enum ProviderArg {
    Github,
    Forgejo,
    Gitlab,
    Other,
}

impl ValueEnum for ProviderArg {
    fn value_variants<'a>() -> &'a [Self] {
        &[Self::Github, Self::Forgejo, Self::Gitlab, Self::Other]
    }

    fn to_possible_value(&self) -> Option<PossibleValue> {
        Some(PossibleValue::new(match self {
            Self::Github => "github",
            Self::Forgejo => "forgejo",
            Self::Gitlab => "gitlab",
            Self::Other => "other",
        }))
    }
}

impl From<ProviderArg> for Provider {
    fn from(value: ProviderArg) -> Self {
        match value {
            ProviderArg::Github => Provider::Github,
            ProviderArg::Forgejo => Provider::Forgejo,
            ProviderArg::Gitlab => Provider::Gitlab,
            ProviderArg::Other => Provider::Other,
        }
    }
}

/// The command line: every subcommand, argument and help text.
fn cli() -> clap::Command {
    let all = || Arg::new("all").long("all").action(ArgAction::SetTrue);
    let repo = || Arg::new("repo").value_name("REPO").action(ArgAction::Set);
    let name = |id: &'static str, value: &'static str| {
        Arg::new(id).value_name(value).action(ArgAction::Set)
    };
    let needs_one = |command: clap::Command| {
        command
            .subcommand_required(true)
            .arg_required_else_help(true)
    };
    needs_one(clap::Command::new("gitdcy").about("Private Git-only multi-device workspace client"))
        .subcommands([
            clap::Command::new("doctor"),
            clap::Command::new("status").arg(status::format_arg()),
            clap::Command::new("check").arg(all()).arg(repo()),
            clap::Command::new("dashboard"),
            needs_one(clap::Command::new("identity"))
                .subcommand(clap::Command::new("status").arg(all()).arg(repo())),
            clap::Command::new("audit")
                .arg(all())
                .arg(
                    Arg::new("explain")
                        .long("explain")
                        .action(ArgAction::SetTrue),
                )
                .arg(repo()),
            needs_one(clap::Command::new("policy")).subcommands([
                clap::Command::new("status").arg(all()).arg(repo()),
                clap::Command::new("plan").arg(repo().required(true)),
                clap::Command::new("apply").arg(repo().required(true)),
            ]),
            clap::Command::new("install-ignore")
                .arg(Arg::new("global").long("global").action(ArgAction::SetTrue)),
            clap::Command::new("sync").arg(all()).arg(repo()),
            clap::Command::new("commit").arg(repo().required(true)).arg(
                name("message", "MESSAGE")
                    .short('m')
                    .long("message")
                    .required(true),
            ),
            clap::Command::new("push").arg(repo().required(true)),
            clap::Command::new("set-sync-remote")
                .arg(repo().required(true))
                .arg(name("url", "URL").required(true)),
            clap::Command::new("set-origin-remote")
                .arg(repo().required(true))
                .arg(name("url", "URL")),
            clap::Command::new("trust-device")
                .arg(name("device", "DEVICE").required(true))
                .arg(repo().long("repo"))
                .arg(all()),
            clap::Command::new("clone")
                .arg(name("url", "URL").required(true))
                .arg(
                    name("root", "ROOT")
                        .long("root")
                        .value_parser(clap::value_parser!(PathBuf)),
                )
                .arg(
                    name("provider", "PROVIDER")
                        .long("provider")
                        .value_parser(EnumValueParser::<ProviderArg>::new()),
                )
                .arg(name("name", "NAME").long("name")),
            clap::Command::new("save-manifest"),
            rules::command().about(
                "Each repository's rules: where it may be pushed, branches that take only a pull \
             request, and the personal-data check before a push",
            ),
            checkouts::command().about(
                "Checkouts against their upstream: up to date, behind, ahead, diverged, local \
             changes or missing; `update` fast-forwards only what is safe",
            ),
            relate::command().about(
                "How two commits of a repository relate: equal, ahead, behind, diverged or \
             unrelated, with the commits only on each side counted",
            ),
            carry::command().about(
                "Moving work between two checkouts of a repository: send it, place it (branches \
             only where the name is free), set it aside",
            ),
        ])
}

/// What the command line asked for, from what `cli()` matched.
fn parse(matches: &ArgMatches) -> Command {
    let flag = |m: &ArgMatches, id: &str| m.get_flag(id);
    let text = |m: &ArgMatches, id: &str| m.get_one::<String>(id).cloned();
    let given = |m: &ArgMatches, id: &str| text(m, id).unwrap_or_default();
    let (name, m) = matches.subcommand().expect("a subcommand is required");
    match name {
        "doctor" => Command::Doctor,
        "status" => Command::Status {
            format: status::format(m),
        },
        "check" => Command::Check {
            all: flag(m, "all"),
            repo: text(m, "repo"),
        },
        "dashboard" => Command::Dashboard,
        "identity" => {
            let (_, m) = m.subcommand().expect("a subcommand is required");
            Command::Identity {
                command: IdentityCommand::Status {
                    all: flag(m, "all"),
                    repo: text(m, "repo"),
                },
            }
        }
        "audit" => Command::Audit {
            all: flag(m, "all"),
            explain: flag(m, "explain"),
            repo: text(m, "repo"),
        },
        "policy" => Command::Policy {
            command: match m.subcommand().expect("a subcommand is required") {
                ("status", m) => PolicyCommand::Status {
                    all: flag(m, "all"),
                    repo: text(m, "repo"),
                },
                ("plan", m) => PolicyCommand::Plan {
                    repo: given(m, "repo"),
                },
                (_, m) => PolicyCommand::Apply {
                    repo: given(m, "repo"),
                },
            },
        },
        "install-ignore" => Command::InstallIgnore {
            global: flag(m, "global"),
        },
        "sync" => Command::Sync {
            all: flag(m, "all"),
            repo: text(m, "repo"),
        },
        "commit" => Command::Commit {
            repo: given(m, "repo"),
            message: given(m, "message"),
        },
        "push" => Command::Push {
            repo: given(m, "repo"),
        },
        "set-sync-remote" => Command::SetSyncRemote {
            repo: given(m, "repo"),
            url: given(m, "url"),
        },
        "set-origin-remote" => Command::SetOriginRemote {
            repo: given(m, "repo"),
            url: text(m, "url"),
        },
        "trust-device" => Command::TrustDevice {
            device: given(m, "device"),
            repo: text(m, "repo"),
            all: flag(m, "all"),
        },
        "clone" => Command::Clone {
            url: given(m, "url"),
            root: m.get_one::<PathBuf>("root").cloned(),
            provider: m.get_one::<ProviderArg>("provider").copied(),
            name: text(m, "name"),
        },
        "save-manifest" => Command::SaveManifest,
        "rules" => Command::Rules(rules::RulesArgs::from_matches(m)),
        "checkouts" => Command::Checkouts(checkouts::CheckoutsArgs::from_matches(m)),
        "relate" => Command::Relate(relate::RelateArgs::from_matches(m)),
        _ => Command::Carry(carry::CarryArgs::from_matches(m)),
    }
}

fn main() -> Result<()> {
    gitdcy_core::convert_yaml_settings();
    match parse(&cli().get_matches()) {
        Command::Doctor => doctor(),
        Command::Status { format } => status(format),
        Command::Check { all, repo } => check(all, repo),
        Command::Dashboard => dashboard::run(),
        Command::Identity { command } => identity(command),
        Command::Audit { all, explain, repo } => audit(all, explain, repo),
        Command::Policy { command } => policy(command),
        Command::InstallIgnore { global } => install_ignore(global),
        Command::Sync { all, repo } => sync(all, repo),
        Command::Commit { repo, message } => {
            let entry = find_repo(&repo)?;
            commit(&entry.path, &message, &[])?;
            println!("committed {}", entry.id);
            Ok(())
        }
        Command::Push { repo } => {
            let entry = find_repo(&repo)?;
            push(&entry.path)?;
            println!("pushed {}", entry.id);
            Ok(())
        }
        Command::SetSyncRemote { repo, url } => {
            let entry = find_repo(&repo)?;
            set_remote(&entry.path, gitdcy_core::SYNC_REMOTE, &url)?;
            println!("set sync remote for {}", entry.id);
            Ok(())
        }
        Command::SetOriginRemote { repo, url } => {
            let entry = find_repo(&repo)?;
            let url = if let Some(url) = url {
                set_remote(&entry.path, "origin", &url)?;
                url
            } else {
                set_suggested_origin_remote(&entry)?
            };
            println!("set origin for {} -> {}", entry.id, url);
            Ok(())
        }
        Command::TrustDevice { device, repo, all } => {
            if all && repo.is_some() {
                bail!("use either --all or --repo, not both");
            }
            let scope = if all {
                "all repos".to_string()
            } else {
                repo.clone().context("pass --all or --repo <repo>")?
            };
            let path = if all {
                set_wip_device_trusted_globally(&device, true)?
            } else {
                let repo = repo.context("pass --all or --repo <repo>")?;
                let entry = find_repo(&repo)?;
                set_wip_device_trusted(&entry, &device, true)?
            };
            println!("trusted {device} for {scope} ({})", path.display());
            Ok(())
        }
        Command::Clone {
            url,
            root,
            provider,
            name,
        } => {
            let destination = clone_repo(&CloneRequest {
                url,
                workspace_root: root.unwrap_or_else(default_workspace_root),
                provider: provider.map(Into::into),
                name,
            })?;
            println!("{}", destination.display());
            Ok(())
        }
        Command::Rules(args) => rules::run(args),
        Command::Checkouts(args) => checkouts::run(args),
        Command::Relate(args) => relate::run(args),
        Command::Carry(args) => carry::run(args),
        Command::SaveManifest => {
            let manifest = load_or_discover_manifest()?;
            save_manifest(&manifest)?;
            println!("saved manifest with {} repos", manifest.repos.len());
            Ok(())
        }
    }
}

fn doctor() -> Result<()> {
    let manifest = load_or_discover_manifest()?;
    println!("workspace root: {}", manifest.workspace_root.display());
    println!("repos: {}", manifest.repos.len());
    println!("config: {}", gitdcy_core::manifest_path()?.display());
    for note in gitdcy_core::config_notes() {
        println!("config note: {note}");
    }
    println!("device: {}", gitdcy_core::device_id());
    println!();

    let statuses = status_all(&manifest);
    let review = statuses
        .iter()
        .filter(|status| {
            status.entry.review_required
                || status.last_error.is_some()
                || status.identity.is_blocking()
        })
        .count();
    let identity_blocked = statuses
        .iter()
        .filter(|status| status.identity.is_blocking())
        .count();
    println!("review required: {review}");
    println!("identity blocked: {identity_blocked}");
    for status in statuses {
        if status.entry.review_required
            || status.last_error.is_some()
            || status.identity.is_blocking()
        {
            println!(
                "- {} | {} | {}",
                status.entry.id,
                status.path.display(),
                status.short_state()
            );
        }
    }
    Ok(())
}

fn identity(command: IdentityCommand) -> Result<()> {
    match command {
        IdentityCommand::Status { all, repo } => identity_status(all, repo),
    }
}

fn identity_status(all: bool, repo: Option<String>) -> Result<()> {
    if all && repo.is_some() {
        bail!("use either --all or a repo name, not both");
    }
    let manifest = load_or_discover_manifest()?;
    let profiles = load_local_config()
        .identity_profiles
        .map(|profiles| profiles.len())
        .unwrap_or(0);
    println!("configured identity profiles: {profiles}");

    let statuses = status_all(&manifest);
    let selected = if all {
        statuses
    } else {
        let repo = repo.context("pass --all or a repo id/name")?;
        vec![statuses
            .into_iter()
            .find(|status| {
                status.entry.id == repo || status.entry.id.ends_with(&format!("/{repo}"))
            })
            .with_context(|| format!("repo not found: {repo}"))?]
    };

    let mut blocked = 0;
    for status in selected {
        print_identity_status(&status);
        if status.identity.is_blocking() {
            blocked += 1;
        }
    }
    if blocked > 0 {
        bail!(
            "identity check found {blocked} blocked repo{}",
            if blocked == 1 { "" } else { "s" }
        );
    }
    Ok(())
}

fn print_identity_status(status: &RepoStatus) {
    let profile = status.identity.profile.as_deref().unwrap_or("-");
    println!(
        "{} | state={} | enforced={} | profile={} | expected={} | actual={}",
        status.entry.id,
        status.identity.state_label(),
        status.identity.enforcement_enabled,
        profile,
        status.identity.expected_display(),
        status.identity.actual_display()
    );
    if status.identity.committer_name != status.identity.actual_name
        || status.identity.committer_email != status.identity.actual_email
    {
        println!("  committer: {}", status.identity.committer_display());
    }
    if !status.identity.environment_overrides.is_empty() {
        println!(
            "  environment overrides: {}",
            status.identity.environment_overrides.join(", ")
        );
    }
    if status.identity.state_label() != "identity-ok" {
        println!("  {}", status.identity.message);
    }
}

fn check(all: bool, repo: Option<String>) -> Result<()> {
    if all && repo.is_some() {
        bail!("use either --all or a repo name, not both");
    }
    let manifest = load_or_discover_manifest()?;
    let mut failures = 0;

    if all {
        for entry in manifest.repos.iter().filter(|entry| entry.enabled) {
            let report = check_repo(entry);
            print_check_report(entry, &report);
            if report.is_blocking() {
                failures += 1;
            }
        }
    } else {
        let repo = repo.context("pass --all or a repo id/name")?;
        let entry = manifest
            .repos
            .iter()
            .find(|entry| entry.id == repo || entry.id.ends_with(&format!("/{repo}")))
            .with_context(|| format!("repo not found: {repo}"))?;
        let report = check_repo(entry);
        print_check_report(entry, &report);
        if report.is_blocking() {
            failures += 1;
        }
    }

    if failures > 0 {
        bail!(
            "checks blocked {failures} repo{}",
            if failures == 1 { "" } else { "s" }
        );
    }
    Ok(())
}

fn print_check_report(entry: &gitdcy_core::RepoEntry, report: &RepoCheckReport) {
    let profile = report.profile.as_deref().unwrap_or("-");
    let worktree = match report.worktree_clean {
        Some(true) => "clean",
        Some(false) => "dirty",
        None => "unknown",
    };
    println!(
        "{} | state={} | profile={} | worktree={}",
        entry.id,
        report.state_label(),
        profile,
        worktree
    );
    for result in &report.results {
        println!(
            "  [{}] {} ({} ms): {}",
            result.state.label(),
            result.name,
            result.duration_ms,
            result.command
        );
        if result.state != CheckResultState::Passed && !result.output.trim().is_empty() {
            for line in result.output.lines() {
                println!("    {line}");
            }
        }
    }
    println!("  {}", report.message);
}

fn audit(all: bool, explain: bool, repo: Option<String>) -> Result<()> {
    if all && repo.is_some() {
        bail!("use either --all or a repo name, not both");
    }
    let manifest = load_or_discover_manifest()?;
    let mut fatal = 0;

    if all {
        for (entry, report) in audit_all(&manifest) {
            match report {
                Ok(report) => {
                    print_audit_report(&entry, &report, explain);
                    fatal += report.fatal_count();
                }
                Err(error) => {
                    fatal += 1;
                    println!("{}: audit failed: {error}", entry.id);
                }
            }
        }
    } else {
        let repo = repo.context("pass --all or a repo id/name")?;
        let entry = manifest
            .repos
            .iter()
            .find(|entry| entry.id == repo || entry.id.ends_with(&format!("/{repo}")))
            .with_context(|| format!("repo not found: {repo}"))?;
        let report = audit_repo(entry)?;
        print_audit_report(entry, &report, explain);
        fatal += report.fatal_count();
    }

    if fatal > 0 {
        bail!(
            "audit found {fatal} blocking finding{}",
            if fatal == 1 { "" } else { "s" }
        );
    }
    Ok(())
}

fn print_audit_report(entry: &gitdcy_core::RepoEntry, report: &RepoSafetyReport, explain: bool) {
    println!(
        "{} | visibility={} | {}",
        entry.id,
        report.visibility.label(),
        report.short_state()
    );
    if report.findings.is_empty() {
        return;
    }
    println!("{}", format_audit_block(entry, report));
    if explain {
        for finding in &report.findings {
            let path = finding.path.as_deref().unwrap_or("-");
            println!("  fix {path}: {}", finding.remediation);
        }
    }
}

fn policy(command: PolicyCommand) -> Result<()> {
    match command {
        PolicyCommand::Status { all, repo } => policy_status(all, repo),
        PolicyCommand::Plan { repo } => {
            let entry = find_repo(&repo)?;
            let report = policy_report(&entry)?;
            print_policy_report(&entry, &report, true);
            Ok(())
        }
        PolicyCommand::Apply { repo } => {
            let entry = find_repo(&repo)?;
            let actions = apply_policy(&entry)?;
            if actions.is_empty() {
                println!("{} policy already applied", entry.id);
            } else {
                for action in actions {
                    println!("{}: {}", entry.id, action.description);
                }
            }
            Ok(())
        }
    }
}

fn policy_status(all: bool, repo: Option<String>) -> Result<()> {
    if all && repo.is_some() {
        bail!("use either --all or a repo name, not both");
    }
    let manifest = load_or_discover_manifest()?;
    if all {
        for (entry, report) in policy_all(&manifest) {
            match report {
                Ok(report) => print_policy_report(&entry, &report, false),
                Err(error) => println!("{} | policy failed: {error}", entry.id),
            }
        }
        return Ok(());
    }
    let repo = repo.context("pass --all or a repo id/name")?;
    let entry = manifest
        .repos
        .iter()
        .find(|entry| entry.id == repo || entry.id.ends_with(&format!("/{repo}")))
        .with_context(|| format!("repo not found: {repo}"))?;
    let report = policy_report(entry)?;
    print_policy_report(entry, &report, true);
    Ok(())
}

fn print_policy_report(entry: &gitdcy_core::RepoEntry, report: &RepoPolicyReport, verbose: bool) {
    println!(
        "{} | visibility={} | {}",
        entry.id,
        report.policy.visibility.label(),
        report.short_state()
    );
    if !verbose {
        return;
    }
    for finding in &report.findings {
        let path = finding.path.as_deref().unwrap_or("-");
        println!(
            "  [{}] {path}: {}",
            finding.severity.label(),
            finding.message
        );
    }
    if report.actions.is_empty() {
        println!("  no policy actions");
    } else {
        for action in &report.actions {
            println!("  action: {}", action.description);
        }
    }
}

fn install_ignore(global: bool) -> Result<()> {
    if !global {
        bail!("pass --global to install the managed global Git ignore block");
    }
    let path = install_global_ignore_template()?;
    println!("installed GitDCY global ignore block in {}", path.display());
    Ok(())
}

fn sync(all: bool, repo: Option<String>) -> Result<()> {
    let manifest = load_or_discover_manifest()?;
    if all {
        for entry in manifest.repos.iter().filter(|entry| entry.enabled) {
            print_report(sync_repo(entry));
        }
        return Ok(());
    }

    let repo = repo.context("pass --all or a repo id/name")?;
    let entry = manifest
        .repos
        .iter()
        .find(|entry| entry.id == repo || entry.id.ends_with(&format!("/{repo}")))
        .with_context(|| format!("repo not found: {repo}"))?;
    print_report(sync_repo(entry));
    Ok(())
}

fn print_report(report: gitdcy_core::SyncReport) {
    println!("{}", report.repo_id);
    for action in &report.actions {
        println!("  - {action}");
    }
    if let Some(blocked) = &report.blocked {
        println!("  blocked: {blocked}");
    }
}

fn find_repo(name: &str) -> Result<gitdcy_core::RepoEntry> {
    let manifest = load_or_discover_manifest()?;
    manifest
        .repos
        .into_iter()
        .find(|entry| entry.id == name || entry.id.ends_with(&format!("/{name}")))
        .with_context(|| format!("repo not found: {name}"))
}
