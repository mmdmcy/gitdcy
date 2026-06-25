use eframe::egui;
use gitdcy_core::{
    clone_repo, commit, default_workspace_root, discover_entries, load_or_discover_manifest,
    local_sync_file_enabled, push, save_manifest, set_local_sync_file, set_remote,
    set_wip_device_trusted, set_wip_device_trusted_globally, status_all, suggested_origin_remote,
    sync_remote_template, sync_repo, CloneRequest, Provider, RepoStatus, SyncReport,
    WorkspaceManifest, SYNC_REMOTE,
};
use std::path::PathBuf;
use std::sync::mpsc::{self, Receiver};
use std::thread;

fn main() -> eframe::Result<()> {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_inner_size([1180.0, 760.0])
            .with_min_inner_size([920.0, 560.0]),
        ..Default::default()
    };

    eframe::run_native(
        "GitDCY",
        options,
        Box::new(|_cc| Ok(Box::new(GitDcyApp::load()))),
    )
}

struct GitDcyApp {
    manifest: WorkspaceManifest,
    statuses: Vec<RepoStatus>,
    selected: Option<usize>,
    commit_message: String,
    clone_url: String,
    clone_name: String,
    clone_provider: Provider,
    origin_remote_url: String,
    sync_remote_url: String,
    logs: Vec<String>,
    busy: bool,
    rx: Option<Receiver<JobResult>>,
}

enum JobResult {
    Refreshed(Result<(WorkspaceManifest, Vec<RepoStatus>), String>),
    Synced(Vec<SyncReport>),
    Committed(Result<String, String>),
    Pushed(Result<String, String>),
    RemoteSet(Result<String, String>),
    Cloned(Result<PathBuf, String>),
    Saved(Result<String, String>),
}

impl GitDcyApp {
    fn load() -> Self {
        let manifest = load_or_discover_manifest().unwrap_or_else(|error| {
            eprintln!("failed to load manifest: {error}");
            WorkspaceManifest {
                workspace_root: default_workspace_root(),
                repos: Vec::new(),
            }
        });
        let statuses = status_all(&manifest);
        Self {
            manifest,
            statuses,
            selected: None,
            commit_message: String::new(),
            clone_url: String::new(),
            clone_name: String::new(),
            clone_provider: Provider::Github,
            origin_remote_url: String::new(),
            sync_remote_url: String::new(),
            logs: Vec::new(),
            busy: false,
            rx: None,
        }
    }

    fn poll_jobs(&mut self, ctx: &egui::Context) {
        let Some(rx) = &self.rx else {
            return;
        };
        let Ok(result) = rx.try_recv() else {
            ctx.request_repaint_after(std::time::Duration::from_millis(150));
            return;
        };

        self.busy = false;
        self.rx = None;
        match result {
            JobResult::Refreshed(result) => match result {
                Ok((manifest, statuses)) => {
                    self.manifest = manifest;
                    self.statuses = statuses;
                    self.selected = self.selected.filter(|idx| *idx < self.statuses.len());
                    self.log("refreshed repository status");
                }
                Err(error) => self.log(format!("refresh failed: {error}")),
            },
            JobResult::Synced(reports) => {
                for report in reports {
                    self.log_report(report);
                }
                self.start_refresh();
            }
            JobResult::Committed(result) => match result {
                Ok(message) => {
                    self.log(message);
                    self.commit_message.clear();
                    self.start_refresh();
                }
                Err(error) => self.log(format!("commit failed: {error}")),
            },
            JobResult::Pushed(result) => match result {
                Ok(message) => {
                    self.log(message);
                    self.start_refresh();
                }
                Err(error) => self.log(format!("push failed: {error}")),
            },
            JobResult::RemoteSet(result) => match result {
                Ok(message) => {
                    self.log(message);
                    self.start_refresh();
                }
                Err(error) => self.log(format!("remote setup failed: {error}")),
            },
            JobResult::Cloned(result) => match result {
                Ok(path) => {
                    self.log(format!("cloned {}", path.display()));
                    self.clone_url.clear();
                    self.clone_name.clear();
                    self.start_refresh_discovering();
                }
                Err(error) => self.log(format!("clone failed: {error}")),
            },
            JobResult::Saved(result) => match result {
                Ok(message) => self.log(message),
                Err(error) => self.log(format!("save failed: {error}")),
            },
        }
    }

    fn start_refresh(&mut self) {
        if self.busy {
            return;
        }
        let manifest = self.manifest.clone();
        self.spawn(move || {
            let statuses = status_all(&manifest);
            JobResult::Refreshed(Ok((manifest, statuses)))
        });
    }

    fn start_refresh_discovering(&mut self) {
        if self.busy {
            return;
        }
        let root = self.manifest.workspace_root.clone();
        self.spawn(move || {
            let mut roots = gitdcy_core::default_scan_roots();
            if !roots.iter().any(|item| item == &root) {
                roots.push(root.clone());
            }
            let result = discover_entries(&roots)
                .map(|repos| {
                    let manifest = WorkspaceManifest {
                        workspace_root: root,
                        repos,
                    };
                    let statuses = status_all(&manifest);
                    (manifest, statuses)
                })
                .map_err(|error| error.to_string());
            JobResult::Refreshed(result)
        });
    }

    fn start_sync_all(&mut self) {
        if self.busy {
            return;
        }
        let entries = self.manifest.repos.clone();
        self.spawn(move || {
            let reports = entries
                .iter()
                .filter(|entry| entry.enabled)
                .map(sync_repo)
                .collect();
            JobResult::Synced(reports)
        });
    }

    fn start_sync_selected(&mut self) {
        if self.busy {
            return;
        }
        let Some(entry) = self.selected_entry().cloned() else {
            self.log("select a repo first");
            return;
        };
        self.spawn(move || JobResult::Synced(vec![sync_repo(&entry)]));
    }

    fn start_commit_selected(&mut self) {
        if self.busy {
            return;
        }
        let Some(status) = self.selected_status().cloned() else {
            self.log("select a repo first");
            return;
        };
        let message = self.commit_message.trim().to_string();
        if message.is_empty() {
            self.log("commit message is required");
            return;
        }
        self.spawn(move || {
            let result = commit(&status.path, &message, &[])
                .map(|_| format!("committed {}", status.entry.id))
                .map_err(|error| error.to_string());
            JobResult::Committed(result)
        });
    }

    fn start_push_selected(&mut self) {
        if self.busy {
            return;
        }
        let Some(status) = self.selected_status().cloned() else {
            self.log("select a repo first");
            return;
        };
        self.spawn(move || {
            let result = push(&status.path)
                .map(|_| format!("pushed {}", status.entry.id))
                .map_err(|error| error.to_string());
            JobResult::Pushed(result)
        });
    }

    fn start_set_sync_remote(&mut self) {
        if self.busy {
            return;
        }
        let Some(status) = self.selected_status().cloned() else {
            self.log("select a repo first");
            return;
        };
        let url = self.sync_remote_url.trim().to_string();
        if url.is_empty() {
            self.log("sync remote URL is required");
            return;
        }
        self.spawn(move || {
            let result = set_remote(&status.path, SYNC_REMOTE, &url)
                .map(|_| format!("set sync remote for {}", status.entry.id))
                .map_err(|error| error.to_string());
            JobResult::RemoteSet(result)
        });
    }

    fn start_set_origin_remote(&mut self) {
        if self.busy {
            return;
        }
        let Some(status) = self.selected_status().cloned() else {
            self.log("select a repo first");
            return;
        };
        let url = self.origin_remote_url.trim().to_string();
        if url.is_empty() {
            self.log("origin remote URL is required");
            return;
        }
        self.spawn(move || {
            let result = set_remote(&status.path, "origin", &url)
                .map(|_| format!("set origin remote for {}", status.entry.id))
                .map_err(|error| error.to_string());
            JobResult::RemoteSet(result)
        });
    }

    fn start_clone(&mut self) {
        if self.busy {
            return;
        }
        let url = self.clone_url.trim().to_string();
        if url.is_empty() {
            self.log("clone URL is required");
            return;
        }
        let name = (!self.clone_name.trim().is_empty()).then(|| self.clone_name.trim().to_string());
        let request = CloneRequest {
            url,
            workspace_root: self.manifest.workspace_root.clone(),
            provider: Some(self.clone_provider),
            name,
        };
        self.spawn(move || {
            JobResult::Cloned(clone_repo(&request).map_err(|error| error.to_string()))
        });
    }

    fn start_save_manifest(&mut self) {
        if self.busy {
            return;
        }
        let manifest = self.manifest.clone();
        self.spawn(move || {
            let result = save_manifest(&manifest)
                .map(|_| format!("saved manifest with {} repos", manifest.repos.len()))
                .map_err(|error| error.to_string());
            JobResult::Saved(result)
        });
    }

    fn spawn<F>(&mut self, job: F)
    where
        F: FnOnce() -> JobResult + Send + 'static,
    {
        let (tx, rx) = mpsc::channel();
        self.busy = true;
        self.rx = Some(rx);
        thread::spawn(move || {
            let _ = tx.send(job());
        });
    }

    fn selected_status(&self) -> Option<&RepoStatus> {
        self.selected.and_then(|idx| self.statuses.get(idx))
    }

    fn selected_entry(&self) -> Option<&gitdcy_core::RepoEntry> {
        self.selected_status().map(|status| &status.entry)
    }

    fn log(&mut self, message: impl Into<String>) {
        self.logs.push(message.into());
        if self.logs.len() > 200 {
            self.logs.drain(0..self.logs.len() - 200);
        }
    }

    fn log_report(&mut self, report: SyncReport) {
        self.log(format!("sync {}", report.repo_id));
        for action in report.actions {
            self.log(format!("  {action}"));
        }
        if let Some(blocked) = report.blocked {
            self.log(format!("  blocked: {blocked}"));
        }
    }

    fn suggested_sync_remote(&self, status: &RepoStatus) -> String {
        if let Some(url) = status.remotes.get(SYNC_REMOTE) {
            return url.clone();
        }
        if let Some(template) = sync_remote_template() {
            let repo_name = status
                .path
                .file_name()
                .and_then(|name| name.to_str())
                .unwrap_or("repo");
            return template
                .replace("{repo}", repo_name)
                .replace("{id}", &status.entry.id);
        }
        let repo_name = status
            .path
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("repo");
        format!("ssh://git@example.internal/owner/{repo_name}.git")
    }

    fn suggested_origin_remote(&self, status: &RepoStatus) -> String {
        if let Some(url) = status.remotes.get("origin") {
            return url.clone();
        }
        suggested_origin_remote(&status.entry).unwrap_or_else(|| {
            let repo_name = status
                .path
                .file_name()
                .and_then(|name| name.to_str())
                .unwrap_or("repo");
            format!("https://example.invalid/{repo_name}.git")
        })
    }
}

impl eframe::App for GitDcyApp {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = ui.ctx().clone();
        self.poll_jobs(&ctx);

        egui::Panel::top("toolbar").show_inside(ui, |ui| {
            ui.horizontal_wrapped(|ui| {
                ui.heading("GitDCY");
                ui.separator();
                ui.label(format!("Root: {}", self.manifest.workspace_root.display()));
                ui.separator();
                ui.add_enabled_ui(!self.busy, |ui| {
                    if ui.button("Refresh").clicked() {
                        self.start_refresh();
                    }
                    if ui.button("Sync All").clicked() {
                        self.start_sync_all();
                    }
                    if ui.button("Sync Selected").clicked() {
                        self.start_sync_selected();
                    }
                    if ui.button("Save Manifest").clicked() {
                        self.start_save_manifest();
                    }
                });
                if self.busy {
                    ui.spinner();
                    ui.label("Working");
                }
            });
        });

        egui::Panel::left("repo_list")
            .resizable(true)
            .default_size(430.0)
            .show_inside(ui, |ui| {
                ui.heading("Repositories");
                ui.add_space(6.0);
                let mut clicked_idx = None;
                egui::ScrollArea::vertical().show(ui, |ui| {
                    for (idx, status) in self.statuses.iter().enumerate() {
                        let selected = self.selected == Some(idx);
                        let branch = status.branch.as_deref().unwrap_or("-");
                        let title = format!("{}  {}", status.entry.id, branch);
                        let response = ui.selectable_label(selected, title);
                        if response.clicked() {
                            clicked_idx = Some(idx);
                        }
                        if selected {
                            ui.small(status.short_state());
                            ui.add_space(4.0);
                        }
                    }
                });
                if let Some(idx) = clicked_idx {
                    self.selected = Some(idx);
                    if let Some(status) = self.statuses.get(idx) {
                        self.origin_remote_url = self.suggested_origin_remote(status);
                        self.sync_remote_url = self.suggested_sync_remote(status);
                    }
                }
            });

        egui::CentralPanel::default().show_inside(ui, |ui| {
            ui.columns(2, |columns| {
                self.repo_detail(&mut columns[0]);
                self.clone_panel(&mut columns[1]);
            });
            ui.separator();
            ui.heading("Log");
            egui::ScrollArea::vertical()
                .stick_to_bottom(true)
                .max_height(220.0)
                .show(ui, |ui| {
                    for line in &self.logs {
                        ui.monospace(line);
                    }
                });
        });
    }
}

impl GitDcyApp {
    fn repo_detail(&mut self, ui: &mut egui::Ui) {
        ui.heading("Selected Repo");
        let Some(status) = self.selected_status().cloned() else {
            ui.label("Select a repository.");
            return;
        };

        egui::Grid::new("repo_detail_grid")
            .num_columns(2)
            .striped(true)
            .show(ui, |ui| {
                ui.label("ID");
                ui.monospace(&status.entry.id);
                ui.end_row();
                ui.label("Path");
                ui.monospace(status.path.display().to_string());
                ui.end_row();
                ui.label("Branch");
                ui.monospace(status.branch.as_deref().unwrap_or("-"));
                ui.end_row();
                ui.label("Tracking Branch");
                ui.monospace(status.tracking_branch.as_deref().unwrap_or("-"));
                ui.end_row();
                ui.label("State");
                ui.monospace(status.short_state());
                ui.end_row();
            });

        ui.add_space(12.0);
        ui.horizontal_wrapped(|ui| {
            ui.add_enabled_ui(!self.busy, |ui| {
                if ui.button("Sync").clicked() {
                    self.start_sync_selected();
                }
                if ui.button("Push").clicked() {
                    self.start_push_selected();
                }
            });
        });

        ui.add_space(12.0);
        ui.label("Commit message");
        ui.text_edit_singleline(&mut self.commit_message);
        ui.add_enabled_ui(!self.busy, |ui| {
            if ui.button("Commit All Non-Ignored Changes").clicked() {
                self.start_commit_selected();
            }
        });

        ui.add_space(12.0);
        ui.heading("Changed Files");
        egui::ScrollArea::vertical()
            .max_height(260.0)
            .show(ui, |ui| {
                if status.dirty_paths.is_empty() {
                    ui.label("No tracked or non-ignored changes.");
                } else {
                    for changed in &status.dirty_paths {
                        ui.horizontal(|ui| {
                            ui.monospace(match changed.kind {
                                gitdcy_core::ChangeKind::Tracked => "tracked",
                                gitdcy_core::ChangeKind::New => "new",
                                gitdcy_core::ChangeKind::Local => "local",
                            });
                            ui.monospace(&changed.path);
                        });
                    }
                }
            });

        ui.add_space(12.0);
        ui.heading("Origin Remote");
        if self.origin_remote_url.trim().is_empty() {
            self.origin_remote_url = self.suggested_origin_remote(&status);
        }
        ui.text_edit_singleline(&mut self.origin_remote_url);
        ui.add_enabled_ui(!self.busy, |ui| {
            if ui.button("Set/Update Origin Remote").clicked() {
                self.start_set_origin_remote();
            }
        });

        ui.add_space(12.0);
        ui.heading("Private WIP Remote");
        if self.sync_remote_url.trim().is_empty() {
            self.sync_remote_url = self.suggested_sync_remote(&status);
        }
        ui.text_edit_singleline(&mut self.sync_remote_url);
        ui.add_enabled_ui(!self.busy, |ui| {
            if ui.button("Set/Update Sync Remote").clicked() {
                self.start_set_sync_remote();
            }
        });

        if let Some(wip) = &status.incoming_wip {
            ui.add_space(12.0);
            ui.heading("Incoming WIP Device");
            ui.horizontal_wrapped(|ui| {
                ui.label("Device");
                ui.monospace(&wip.device);
            });
            if status.incoming_wip_trusted {
                ui.label("Trusted for this repo.");
            } else {
                ui.add_enabled_ui(!self.busy, |ui| {
                    if ui.button("Trust Incoming Device").clicked() {
                        match set_wip_device_trusted(&status.entry, &wip.device, true) {
                            Ok(path) => {
                                self.log(format!(
                                    "trusted {} for {} ({})",
                                    wip.device,
                                    status.entry.id,
                                    path.display()
                                ));
                                self.start_refresh();
                            }
                            Err(error) => self.log(format!("device trust failed: {error}")),
                        }
                    }
                    if ui.button("Trust Device For All Repos").clicked() {
                        match set_wip_device_trusted_globally(&wip.device, true) {
                            Ok(path) => {
                                self.log(format!(
                                    "trusted {} for all repos ({})",
                                    wip.device,
                                    path.display()
                                ));
                                self.start_refresh();
                            }
                            Err(error) => self.log(format!("device trust failed: {error}")),
                        }
                    }
                });
            }
        }

        ui.add_space(12.0);
        ui.heading("Private Local Files");
        let mut env_enabled = local_sync_file_enabled(&status.entry, ".env");
        ui.add_enabled_ui(!self.busy, |ui| {
            if ui
                .checkbox(&mut env_enabled, "Sync .env through private WIP")
                .changed()
            {
                match set_local_sync_file(&status.entry, ".env", env_enabled) {
                    Ok(path) => {
                        let state = if env_enabled { "enabled" } else { "disabled" };
                        self.log(format!(
                            "{state} .env private WIP sync for {} ({})",
                            status.entry.id,
                            path.display()
                        ));
                        self.start_refresh();
                    }
                    Err(error) => self.log(format!("local file setting failed: {error}")),
                }
            }
        });
    }

    fn clone_panel(&mut self, ui: &mut egui::Ui) {
        ui.heading("Clone Repo");
        ui.label("URL");
        ui.text_edit_singleline(&mut self.clone_url);
        ui.label("Optional name");
        ui.text_edit_singleline(&mut self.clone_name);

        egui::ComboBox::from_label("Provider folder")
            .selected_text(self.clone_provider.folder())
            .show_ui(ui, |ui| {
                ui.selectable_value(&mut self.clone_provider, Provider::Github, "github");
                ui.selectable_value(&mut self.clone_provider, Provider::Forgejo, "forgejo");
                ui.selectable_value(&mut self.clone_provider, Provider::Gitlab, "gitlab");
                ui.selectable_value(&mut self.clone_provider, Provider::Other, "other");
            });

        ui.add_enabled_ui(!self.busy, |ui| {
            if ui.button("Clone Into Workspace").clicked() {
                self.start_clone();
            }
        });

        ui.add_space(18.0);
        ui.heading("Remotes");
        if let Some(status) = self.selected_status() {
            egui::Grid::new("remote_grid")
                .num_columns(2)
                .striped(true)
                .show(ui, |ui| {
                    for (name, url) in &status.remotes {
                        ui.monospace(name);
                        ui.monospace(url);
                        ui.end_row();
                    }
                });
        } else {
            ui.label("Select a repository.");
        }
    }
}
