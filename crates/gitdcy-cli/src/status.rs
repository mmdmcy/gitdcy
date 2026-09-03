use anyhow::Result;
use clap::ValueEnum;
use gitdcy_core::{load_or_discover_manifest, status_all, RepoStatus};
use serde::Serialize;
use std::time::{SystemTime, UNIX_EPOCH};

#[derive(Debug, Clone, Copy, Default, ValueEnum)]
pub(crate) enum OutputFormat {
    #[default]
    Human,
    Json,
}

pub(crate) fn status(format: OutputFormat) -> Result<()> {
    let manifest = load_or_discover_manifest()?;
    let statuses = status_all(&manifest);
    match format {
        OutputFormat::Human => {
            for status in statuses {
                let branch = status.branch.as_deref().unwrap_or("-");
                println!(
                    "{:<34} {:<28} {}",
                    status.entry.id,
                    branch,
                    status.short_state()
                );
            }
        }
        OutputFormat::Json => {
            let envelope = StatusEnvelope::from_statuses(statuses);
            println!("{}", serde_json::to_string_pretty(&envelope)?);
        }
    }
    Ok(())
}

#[derive(Debug, Serialize)]
struct StatusEnvelope {
    schema_version: &'static str,
    component: &'static str,
    operation: &'static str,
    outcome: &'static str,
    data: StatusData,
    warnings: Vec<String>,
    error: Option<String>,
    correlation_id: String,
}

impl StatusEnvelope {
    fn from_statuses(statuses: Vec<RepoStatus>) -> Self {
        let mut warnings = Vec::new();
        let repos = statuses
            .into_iter()
            .map(|status| {
                if status.entry.review_required {
                    warnings.push(format!("{} requires review", status.entry.id));
                }
                if let Some(error) = &status.last_error {
                    warnings.push(format!("{}: {error}", status.entry.id));
                }
                if status.identity.is_blocking() {
                    warnings.push(format!(
                        "{}: {}",
                        status.entry.id,
                        status.identity.state_label()
                    ));
                }
                StatusRepo::from(status)
            })
            .collect();
        let outcome = if warnings.is_empty() {
            "success"
        } else {
            "partial"
        };
        Self {
            schema_version: "linuxmice.response.v1",
            component: "katteke.gitdcy",
            operation: "status",
            outcome,
            data: StatusData { repos },
            warnings,
            error: None,
            correlation_id: correlation_id(),
        }
    }
}

#[derive(Debug, Serialize)]
struct StatusData {
    repos: Vec<StatusRepo>,
}

#[derive(Debug, Serialize)]
struct StatusRepo {
    id: String,
    path: String,
    branch: Option<String>,
    tracking_branch: Option<String>,
    dirty: bool,
    changed_paths: usize,
    ahead: Option<u32>,
    behind: Option<u32>,
    incoming_wip: bool,
    incoming_wip_trusted: bool,
    outgoing_wip: bool,
    review_required: bool,
    safety: String,
    identity_state: String,
    identity_profile: Option<String>,
    identity_enforced: bool,
    identity_blocked: bool,
    last_error: Option<String>,
    state: String,
}

impl From<RepoStatus> for StatusRepo {
    fn from(status: RepoStatus) -> Self {
        let dirty = status.is_dirty();
        let changed_paths = status.dirty_paths.len();
        let incoming_wip = status.incoming_wip.is_some();
        let outgoing_wip = status.outgoing_wip.is_some();
        let review_required = status.entry.review_required;
        let safety = status.safety.short_state();
        let state = status.short_state();
        let identity_state = status.identity.state_label().to_string();
        let identity_profile = status.identity.profile.clone();
        let identity_enforced = status.identity.enforcement_enabled;
        let identity_blocked = status.identity.is_blocking();
        Self {
            id: status.entry.id,
            path: status.path.to_string_lossy().into_owned(),
            branch: status.branch,
            tracking_branch: status.tracking_branch,
            dirty,
            changed_paths,
            ahead: status.ahead,
            behind: status.behind,
            incoming_wip,
            incoming_wip_trusted: status.incoming_wip_trusted,
            outgoing_wip,
            review_required,
            safety,
            identity_state,
            identity_profile,
            identity_enforced,
            identity_blocked,
            last_error: status.last_error,
            state,
        }
    }
}

fn correlation_id() -> String {
    if let Ok(value) = std::env::var("LINUXMICE_CORRELATION_ID") {
        if !value.trim().is_empty() {
            return value;
        }
    }
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos())
        .unwrap_or_default();
    format!("gitdcy-{}-{nanos}", std::process::id())
}

#[cfg(test)]
mod tests {
    use super::{StatusData, StatusEnvelope, StatusRepo};

    #[test]
    fn json_status_keeps_the_versioned_envelope_shape() {
        let envelope = StatusEnvelope {
            schema_version: "linuxmice.response.v1",
            component: "katteke.gitdcy",
            operation: "status",
            outcome: "success",
            data: StatusData {
                repos: vec![StatusRepo {
                    id: "github/example".into(),
                    path: "/workspace/example".into(),
                    branch: Some("main".into()),
                    tracking_branch: Some("origin/main".into()),
                    dirty: false,
                    changed_paths: 0,
                    ahead: Some(0),
                    behind: Some(0),
                    incoming_wip: false,
                    incoming_wip_trusted: true,
                    outgoing_wip: false,
                    review_required: false,
                    safety: "public-safe".into(),
                    identity_state: "identity-unchecked".into(),
                    identity_profile: None,
                    identity_enforced: false,
                    identity_blocked: false,
                    last_error: None,
                    state: "clean".into(),
                }],
            },
            warnings: Vec::new(),
            error: None,
            correlation_id: "test-correlation".into(),
        };

        let value = serde_json::to_value(envelope).expect("serialize status envelope");
        assert_eq!(value["schema_version"], "linuxmice.response.v1");
        assert_eq!(value["component"], "katteke.gitdcy");
        assert_eq!(value["operation"], "status");
        assert_eq!(value["outcome"], "success");
        assert!(value["data"]["repos"].is_array());
        assert_eq!(
            value["data"]["repos"][0]["identity_state"],
            "identity-unchecked"
        );
        assert_eq!(value["data"]["repos"][0]["identity_enforced"], false);
        assert_eq!(value["data"]["repos"][0]["identity_blocked"], false);
        assert!(value["warnings"].is_array());
        assert!(value.get("error").is_some());
        assert_eq!(value["correlation_id"], "test-correlation");
    }
}
