use anyhow::{bail, Context, Result};
use clap::{Parser, Subcommand, ValueEnum};
use crossterm::{
    event::{self, Event, KeyCode},
    execute,
    terminal::{disable_raw_mode, enable_raw_mode, EnterAlternateScreen, LeaveAlternateScreen},
};
use gitdcy_core::{
    apply_policy, audit_all, audit_repo, clone_repo, commit, default_workspace_root,
    format_audit_block, install_global_ignore_template, load_or_discover_manifest, policy_all,
    policy_report, push, save_manifest, set_remote, set_suggested_origin_remote,
    set_wip_device_trusted, set_wip_device_trusted_globally, status_all, sync_repo, CloneRequest,
    Provider, RepoPolicyReport, RepoSafetyReport, RepoStatus,
};
use ratatui::{
    backend::CrosstermBackend,
    layout::{Constraint, Direction, Layout},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, List, ListItem, Paragraph, Row, Table, TableState},
    Terminal,
};
use std::io;
use std::path::PathBuf;
use std::time::Duration;

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
    Dashboard,
    Audit {
        #[arg(long)]
        all: bool,
        #[arg(long)]
        explain: bool,
        repo: Option<String>,
    },
    Policy {
        #[command(subcommand)]
        command: PolicyCommand,
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

#[derive(Debug, Subcommand)]
enum PolicyCommand {
    Status {
        #[arg(long)]
        all: bool,
        repo: Option<String>,
    },
    Plan {
        repo: String,
    },
    Apply {
        repo: String,
    },
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
        Command::Dashboard => dashboard(),
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

fn dashboard() -> Result<()> {
    enable_raw_mode().context("enable raw mode")?;
    let mut stdout = io::stdout();
    execute!(stdout, EnterAlternateScreen).context("enter alternate screen")?;
    let backend = CrosstermBackend::new(stdout);
    let mut terminal = Terminal::new(backend).context("create terminal")?;
    let result = run_dashboard(&mut terminal);
    disable_raw_mode().ok();
    execute!(terminal.backend_mut(), LeaveAlternateScreen).ok();
    terminal.show_cursor().ok();
    result
}

fn run_dashboard(terminal: &mut Terminal<CrosstermBackend<io::Stdout>>) -> Result<()> {
    let mut manifest = load_or_discover_manifest()?;
    let mut statuses = status_all(&manifest);
    let mut selected = 0usize;
    let mut log =
        "q quit | r refresh | j/k or arrows move | s sync selected | S sync all safe".to_string();

    loop {
        if selected >= statuses.len() {
            selected = statuses.len().saturating_sub(1);
        }
        terminal.draw(|frame| {
            let area = frame.area();
            let chunks = Layout::default()
                .direction(Direction::Vertical)
                .constraints([
                    Constraint::Min(12),
                    Constraint::Length(9),
                    Constraint::Length(2),
                ])
                .split(area);
            let main = Layout::default()
                .direction(Direction::Horizontal)
                .constraints([Constraint::Percentage(58), Constraint::Percentage(42)])
                .split(chunks[0]);

            let rows = statuses.iter().map(|status| {
                let branch = status.branch.as_deref().unwrap_or("-");
                let safety = status.safety.short_state();
                Row::new(vec![
                    status.entry.id.clone(),
                    branch.to_string(),
                    status.short_state(),
                    safety,
                ])
            });
            let mut table_state = TableState::default();
            if !statuses.is_empty() {
                table_state.select(Some(selected));
            }
            let table = Table::new(
                rows,
                [
                    Constraint::Percentage(32),
                    Constraint::Percentage(18),
                    Constraint::Percentage(30),
                    Constraint::Percentage(20),
                ],
            )
            .header(
                Row::new(vec!["repo", "branch", "state", "safety"]).style(
                    Style::default()
                        .fg(Color::Yellow)
                        .add_modifier(Modifier::BOLD),
                ),
            )
            .block(
                Block::default()
                    .borders(Borders::ALL)
                    .title("GitDCY Dashboard"),
            )
            .row_highlight_style(Style::default().add_modifier(Modifier::REVERSED));
            frame.render_stateful_widget(table, main[0], &mut table_state);

            let detail = statuses
                .get(selected)
                .map(status_detail_lines)
                .unwrap_or_else(|| vec![Line::from("No repositories discovered.")]);
            let detail = Paragraph::new(detail).block(
                Block::default()
                    .borders(Borders::ALL)
                    .title("Selected Repo"),
            );
            frame.render_widget(detail, main[1]);

            let findings = statuses
                .get(selected)
                .map(finding_items)
                .unwrap_or_default();
            let findings =
                List::new(findings).block(Block::default().borders(Borders::ALL).title("Findings"));
            frame.render_widget(findings, chunks[1]);

            let footer = Paragraph::new(log.as_str()).block(Block::default().borders(Borders::ALL));
            frame.render_widget(footer, chunks[2]);
        })?;

        if !event::poll(Duration::from_millis(250))? {
            continue;
        }
        let Event::Key(key) = event::read()? else {
            continue;
        };
        match key.code {
            KeyCode::Char('q') => return Ok(()),
            KeyCode::Char('r') => {
                manifest = load_or_discover_manifest()?;
                statuses = status_all(&manifest);
                log = "refreshed".to_string();
            }
            KeyCode::Down | KeyCode::Char('j') => {
                if selected + 1 < statuses.len() {
                    selected += 1;
                }
            }
            KeyCode::Up | KeyCode::Char('k') => {
                selected = selected.saturating_sub(1);
            }
            KeyCode::Char('s') => {
                if let Some(status) = statuses.get(selected) {
                    if status.safety.has_fatal_findings() {
                        log = format!("sync blocked for {}: safety findings", status.entry.id);
                    } else {
                        let report = sync_repo(&status.entry);
                        log = if let Some(blocked) = report.blocked {
                            format!("sync blocked for {}: {blocked}", status.entry.id)
                        } else {
                            format!("synced {}", status.entry.id)
                        };
                        statuses = status_all(&manifest);
                    }
                }
            }
            KeyCode::Char('S') => {
                let mut synced = 0usize;
                let mut blocked = 0usize;
                for status in &statuses {
                    if status.safety.has_fatal_findings() {
                        blocked += 1;
                        continue;
                    }
                    let report = sync_repo(&status.entry);
                    if report.blocked.is_some() {
                        blocked += 1;
                    } else {
                        synced += 1;
                    }
                }
                statuses = status_all(&manifest);
                log = format!("sync all safe: {synced} synced, {blocked} blocked/skipped");
            }
            _ => {}
        }
    }
}

fn status_detail_lines(status: &RepoStatus) -> Vec<Line<'static>> {
    let mut lines = vec![
        Line::from(vec![
            Span::styled("id: ", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(status.entry.id.clone()),
        ]),
        Line::from(vec![
            Span::styled("path: ", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(status.path.display().to_string()),
        ]),
        Line::from(vec![
            Span::styled("branch: ", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(status.branch.clone().unwrap_or_else(|| "-".to_string())),
        ]),
        Line::from(vec![
            Span::styled("tracking: ", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(
                status
                    .tracking_branch
                    .clone()
                    .unwrap_or_else(|| "-".to_string()),
            ),
        ]),
        Line::from(vec![
            Span::styled(
                "visibility: ",
                Style::default().add_modifier(Modifier::BOLD),
            ),
            Span::raw(status.safety.visibility.label().to_string()),
        ]),
        Line::from(vec![
            Span::styled("state: ", Style::default().add_modifier(Modifier::BOLD)),
            Span::raw(status.short_state()),
        ]),
    ];
    if status.remotes.is_empty() {
        lines.push(Line::from("remotes: -"));
    } else {
        lines.push(Line::from("remotes:"));
        for (name, url) in &status.remotes {
            lines.push(Line::from(format!("  {name}: {url}")));
        }
    }
    lines
}

fn finding_items(status: &RepoStatus) -> Vec<ListItem<'static>> {
    if status.safety.findings.is_empty() {
        return vec![ListItem::new("No safety findings.")];
    }
    status
        .safety
        .findings
        .iter()
        .map(|finding| {
            let path = finding.path.as_deref().unwrap_or("-");
            ListItem::new(format!(
                "[{}] {path}: {}",
                finding.severity.label(),
                finding.reason
            ))
        })
        .collect()
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
