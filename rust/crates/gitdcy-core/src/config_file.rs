//! GitDCY's settings files are TOML: `local.toml` and `workspace.toml` in its
//! config folder, and `.gitdcy.local.toml` in the folder it runs in. Until
//! 2026-11-08 they were YAML (`NAME.yaml`). A YAML file without its TOML is
//! still read (by `yaml.rs`, never guessed) and converted where allowed: the
//! TOML is written beside it, read back to the same values, and only then is
//! the YAML moved aside as `NAME.yaml.bak-DATE`. A file that would lose
//! something in TOML stays YAML and is read as before; `gitdcy doctor` names
//! it and why.

use crate::error::{Context, Result};
use crate::{bail, yaml};
use serde::de::DeserializeOwned;
use serde::Serialize;
use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// A settings file that is there: its TOML, else its YAML.
pub(crate) struct Found {
    path: PathBuf,
    yaml: bool,
}

fn there(path: &Path) -> bool {
    // Anything but "not found" counts, so an unreadable file is an error
    // later, never silently no settings.
    !matches!(fs::symlink_metadata(path), Err(e) if e.kind() == std::io::ErrorKind::NotFound)
}

pub(crate) fn toml_path(folder: &Path, name: &str) -> PathBuf {
    folder.join(format!("{name}.toml"))
}

pub(crate) fn yaml_path(folder: &Path, name: &str) -> PathBuf {
    folder.join(format!("{name}.yaml"))
}

pub(crate) fn find(folder: &Path, name: &str) -> Option<Found> {
    let toml = toml_path(folder, name);
    if there(&toml) {
        return Some(Found {
            path: toml,
            yaml: false,
        });
    }
    let yaml = yaml_path(folder, name);
    there(&yaml).then_some(Found {
        path: yaml,
        yaml: true,
    })
}

impl Found {
    pub(crate) fn path(&self) -> &Path {
        &self.path
    }

    pub(crate) fn is_yaml(&self) -> bool {
        self.yaml
    }

    pub(crate) fn parse<T: DeserializeOwned>(&self, text: &str) -> Result<T> {
        Ok(if self.yaml {
            yaml::from_str(text)?
        } else {
            toml::from_str(text)?
        })
    }

    /// Converts this YAML file, read as `value` from `text`, to TOML; once
    /// per file and process. Nothing for a TOML file.
    pub(crate) fn convert<T: Serialize + DeserializeOwned>(
        &self,
        text: &str,
        value: &T,
    ) -> Result<()> {
        static TRIED: Mutex<BTreeSet<PathBuf>> = Mutex::new(BTreeSet::new());
        if !self.yaml
            || !TRIED
                .lock()
                .is_ok_and(|mut tried| tried.insert(self.path.clone()))
        {
            return Ok(());
        }
        if let Some(problem) = problem(text, value) {
            bail!("{problem}");
        }
        let toml = self.path.with_extension("toml");
        write_toml(&toml, value)?;
        match crate::rules::keep_aside(&self.path) {
            Ok(_) => Ok(()),
            // Another gitdcy converted it at the same moment.
            Err(_) if !self.path.exists() => Ok(()),
            Err(error) => Err(error),
        }
    }
}

/// What `value`, read from the YAML `text`, would lose as TOML: a value TOML
/// cannot hold, a value that reads back differently, or a key that is no
/// GitDCY setting (it had no effect, but it is the owner's). None: nothing.
pub(crate) fn problem<T: Serialize + DeserializeOwned>(text: &str, value: &T) -> Option<String> {
    let toml_text = match toml::to_string_pretty(value) {
        Ok(text) => text,
        Err(error) => return Some(format!("TOML cannot hold a value in it: {error}")),
    };
    let back: T = match toml::from_str(&toml_text) {
        Ok(back) => back,
        Err(error) => return Some(format!("its TOML does not read back: {error}")),
    };
    let (Ok(kept), Ok(again)) = (serde_json::to_value(value), serde_json::to_value(&back)) else {
        return Some("its values cannot be compared".to_string());
    };
    if again != kept {
        return Some("its TOML reads back different values".to_string());
    }
    let tree = yaml::parse(text).ok().flatten()?;
    yaml::first_dropped_key(&tree, &kept)
        .map(|key| format!("{key} is not a GitDCY setting and would be lost"))
}

/// Writes `value` as `NAME.toml` in `folder`; a `NAME.yaml` there, which it
/// replaces, is moved aside.
pub(crate) fn write<T: Serialize>(folder: &Path, name: &str, value: &T) -> Result<PathBuf> {
    fs::create_dir_all(folder)
        .with_context(|| format!("create config directory {}", folder.display()))?;
    let path = toml_path(folder, name);
    write_toml(&path, value)?;
    let yaml = yaml_path(folder, name);
    if there(&yaml) {
        crate::rules::keep_aside(&yaml)?;
    }
    Ok(path)
}

/// Whole or not at all: a temporary file beside it, then a rename.
fn write_toml<T: Serialize>(path: &Path, value: &T) -> Result<()> {
    let text = toml::to_string_pretty(value)
        .with_context(|| format!("write {} as TOML", path.display()))?;
    let name = path
        .file_name()
        .map_or_else(Default::default, |n| n.to_string_lossy());
    let temporary = path.with_file_name(format!(".{name}.{}.tmp", std::process::id()));
    fs::write(&temporary, text).with_context(|| format!("write {}", temporary.display()))?;
    fs::rename(&temporary, path).with_context(|| {
        let _ = fs::remove_file(&temporary);
        format!("write {}", path.display())
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{LocalConfig, WorkspaceManifest};

    fn folder(name: &str) -> PathBuf {
        let dir =
            std::env::temp_dir().join(format!("gitdcy-config-file-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn json<T: Serialize>(value: &T) -> serde_json::Value {
        serde_json::to_value(value).unwrap()
    }

    // What serde_norway 0.9.42 wrote for these settings (its output, frozen
    // before it left GitDCY).
    const LOCAL: &str = include_str!("../tests/yaml/local.yaml");
    const MANIFEST: &str = include_str!("../tests/yaml/workspace.yaml");
    const EMPTY: &str = include_str!("../tests/yaml/empty.yaml");

    #[test]
    fn a_yaml_file_becomes_toml_with_the_same_values_and_a_kept_copy() {
        for (name, text) in [("local", LOCAL), ("local-empty", EMPTY)] {
            let dir = folder(name);
            fs::write(dir.join("local.yaml"), text).unwrap();
            let found = find(&dir, "local").unwrap();
            assert!(found.is_yaml());
            let read: LocalConfig = found.parse(text).unwrap();
            found.convert(text, &read).unwrap();
            assert!(!dir.join("local.yaml").exists());
            let kept: Vec<_> = fs::read_dir(&dir)
                .unwrap()
                .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
                .filter(|n| n.starts_with("local.yaml.bak-"))
                .collect();
            assert_eq!(kept.len(), 1, "{kept:?}");
            assert_eq!(fs::read_to_string(dir.join(&kept[0])).unwrap(), text);
            let found = find(&dir, "local").unwrap();
            assert!(!found.is_yaml());
            let toml = fs::read_to_string(found.path()).unwrap();
            let back: LocalConfig = found.parse(&toml).unwrap();
            assert_eq!(json(&back), json(&read), "{toml}");
        }
        let dir = folder("workspace");
        fs::write(dir.join("workspace.yaml"), MANIFEST).unwrap();
        let found = find(&dir, "workspace").unwrap();
        let read: WorkspaceManifest = found.parse(MANIFEST).unwrap();
        found.convert(MANIFEST, &read).unwrap();
        let toml = fs::read_to_string(dir.join("workspace.toml")).unwrap();
        let back: WorkspaceManifest = toml::from_str(&toml).unwrap();
        assert_eq!(json(&back), json(&read), "{toml}");
        assert_eq!(read.repos.len(), 2);
        assert_eq!(read.repos[1].id, "other/x: y");
    }

    #[test]
    fn what_would_be_lost_stays_yaml() {
        let dir = folder("unknown");
        let text = "rules_file: /r.toml\ncolour: blue\n";
        fs::write(dir.join("local.yaml"), text).unwrap();
        let found = find(&dir, "local").unwrap();
        let read: LocalConfig = found.parse(text).unwrap();
        let error = found.convert(text, &read).unwrap_err().to_string();
        assert!(error.contains("colour is not a GitDCY setting"), "{error}");
        assert!(dir.join("local.yaml").exists());
        assert!(!dir.join("local.toml").exists());
        // A none value is not a loss.
        let text = "rules_file: /r.toml\nscan_roots: ~\n";
        let read: LocalConfig = yaml::from_str(text).unwrap();
        assert_eq!(problem(text, &read), None);
    }

    #[test]
    fn toml_wins_and_a_write_moves_the_yaml_aside() {
        let dir = folder("write");
        fs::write(dir.join("local.yaml"), "rules_file: /old.toml\n").unwrap();
        fs::write(dir.join("local.toml"), "rules_file = \"/new.toml\"\n").unwrap();
        let found = find(&dir, "local").unwrap();
        assert!(!found.is_yaml());
        let config = LocalConfig {
            rules_file: Some(PathBuf::from("/written.toml")),
            ..LocalConfig::default()
        };
        let path = write(&dir, "local", &config).unwrap();
        assert_eq!(path, dir.join("local.toml"));
        assert!(!dir.join("local.yaml").exists());
        assert!(fs::read_to_string(&path).unwrap().contains("/written.toml"));
    }

    /// The shipped example, uncommented, and the README's settings examples
    /// are settings GitDCY reads, every key kept.
    #[test]
    fn the_examples_are_settings_gitdcy_reads() {
        let example = include_str!("../../../../.gitdcy.local.example.toml");
        let uncommented: String = example
            .lines()
            .map(|line| match line.strip_prefix("# ") {
                _ if line == "#" => "",
                Some(rest) if rest.starts_with('[') || rest.contains(" = ") => rest,
                _ => line,
            })
            .collect::<Vec<_>>()
            .join("\n");
        let readme = include_str!("../../../../README.md");
        let blocks = readme
            .split("```toml\n")
            .skip(1)
            .filter_map(|block| block.split("```").next())
            .filter(|block| !block.contains("[[repo]]") && !block.contains("[hygiene]"));
        let mut checked = 0;
        for text in std::iter::once(uncommented.as_str()).chain(blocks) {
            let config: LocalConfig =
                toml::from_str(text).unwrap_or_else(|e| panic!("{e}\n{text}"));
            let written: toml::Table = toml::from_str(&toml::to_string(&config).unwrap()).unwrap();
            let given: toml::Table = toml::from_str(text).unwrap();
            for key in given.keys() {
                assert!(written.contains_key(key), "{key} is not a setting:\n{text}");
            }
            checked += 1;
        }
        assert!(checked >= 5, "{checked}");
        let all: LocalConfig = toml::from_str(&uncommented).unwrap();
        assert_eq!(all.identity_profiles.map(|p| p.len()), Some(3));
        assert_eq!(all.check_profiles.unwrap()["rust-project"].checks.len(), 3);
        assert_eq!(all.trusted_wip_devices.unwrap()["*"], ["laptop-name"]);
    }
}
