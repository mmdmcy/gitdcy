use anyhow::{bail, Context, Result};
use clap::{Parser, Subcommand, ValueEnum};
use gitdcy_core::{
    audit_all, audit_repo, clone_repo, commit, default_workspace_root, format_audit_block,
    install_global_ignore_template, load_or_discover_manifest, push, save_manifest, set_remote,
    set_suggested_origin_remote, set_wip_device_trusted, set_wip_device_trusted_globally,
    status_all, sync_repo, CloneRequest, Provider, RepoSafetyReport,
};
use std::path::PathBuf;

#[derive(Debug, Parser)]
#[command(name = "gitdcy")]
#[command(about = "Private Git-only multi-device workspace client")]
struct Args {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    Doctor,
    Status,
    Audit {
        #[arg(long)]
        all: bool,
        repo: Option<String>,
    },
    InstallIgnore {
        #[arg(long)]
        global: bool,
    },
    Sync {
        #[arg(long)]
        all: bool,
        repo: Option<String>,
    },
    Commit {
        repo: String,
        #[arg(short, long)]
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
        #[arg(long)]
        repo: Option<String>,
        #[arg(long)]
        all: bool,
    },
    Clone {
        url: String,
        #[arg(long)]
        root: Option<PathBuf>,
        #[arg(long, value_enum)]
        provider: Option<ProviderArg>,
        #[arg(long)]
        name: Option<String>,
    },
    SaveManifest,
}

#[derive(Debug, Clone, Copy, ValueEnum)]
enum ProviderArg {
    Github,
    Forgejo,
    Gitlab,
    Other,
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

fn main() -> Result<()> {
    let args = Args::parse();
    match args.command {
        Command::Doctor => doctor(),
        Command::Status => status(),
        Command::Audit { all, repo } => audit(all, repo),
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
    println!("device: {}", gitdcy_core::device_id());
    println!();

    let statuses = status_all(&manifest);
    let review = statuses
        .iter()
        .filter(|status| status.entry.review_required || status.last_error.is_some())
        .count();
    println!("review required: {review}");
    for status in statuses {
        if status.entry.review_required || status.last_error.is_some() {
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

fn status() -> Result<()> {
    let manifest = load_or_discover_manifest()?;
    for status in status_all(&manifest) {
        let branch = status.branch.as_deref().unwrap_or("-");
        println!(
            "{:<34} {:<28} {}",
            status.entry.id,
            branch,
            status.short_state()
        );
    }
    Ok(())
}

fn audit(all: bool, repo: Option<String>) -> Result<()> {
    if all && repo.is_some() {
        bail!("use either --all or a repo name, not both");
    }
    let manifest = load_or_discover_manifest()?;
    let mut fatal = 0;

    if all {
        for (entry, report) in audit_all(&manifest) {
            match report {
                Ok(report) => {
                    print_audit_report(&entry, &report);
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
        print_audit_report(entry, &report);
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

fn print_audit_report(entry: &gitdcy_core::RepoEntry, report: &RepoSafetyReport) {
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
