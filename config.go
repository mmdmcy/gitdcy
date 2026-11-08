package main

// GitDCY's settings files are TOML: local.toml and workspace.toml in its
// config folder, and .gitdcy.local.toml in the folder it runs in. Until
// 2026-11-08 they were YAML (NAME.yaml). A YAML file without its TOML is
// still read (by yaml.go, never guessed) and converted where allowed: the
// TOML is written beside it, read back to the same values, and only then is
// the YAML moved aside as NAME.yaml.bak-DATE. A file that would lose
// something in TOML stays YAML and is read as before; `gitdcy doctor` names
// it and why.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The settings files, as NAME.toml (until 2026-11-08 NAME.yaml, which is
// still read and converted).
const (
	localConfigName = "local"
	manifestName    = "workspace"
	folderConfig    = ".gitdcy.local"
)

var convertYAML atomic.Bool

// convertYAMLSettings lets this process convert the YAML settings files it
// reads to TOML. main calls it first; a test that does not never writes to
// the settings it reads.
func convertYAMLSettings() { convertYAML.Store(true) }

func mayConvert() bool { return convertYAML.Load() && !readOnlyStatus() }

// foundFile is a settings file that is there: its TOML, else its YAML.
type foundFile struct {
	path string
	yaml bool
}

// there reports anything but "not found", so an unreadable file is an error
// later, never silently no settings.
func there(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !notFound(err)
}

func tomlPath(folder, name string) string { return joinPath(folder, name+".toml") }
func yamlPath(folder, name string) string { return joinPath(folder, name+".yaml") }

func findSettings(folder, name string) *foundFile {
	if t := tomlPath(folder, name); there(t) {
		return &foundFile{path: t}
	}
	if y := yamlPath(folder, name); there(y) {
		return &foundFile{path: y, yaml: true}
	}
	return nil
}

// parseLocalConfig reads a local settings file's text.
func (f *foundFile) parseLocalConfig(text string) (LocalConfig, error) {
	if f.yaml {
		return yamlLocalConfig(text)
	}
	var c LocalConfig
	err := tomlUnmarshal(text, &c)
	return c, err
}

// parseManifest reads a workspace file's text.
func (f *foundFile) parseManifest(text string) (WorkspaceManifest, error) {
	if f.yaml {
		return yamlManifest(text)
	}
	var m WorkspaceManifest
	err := tomlUnmarshal(text, &m)
	return m, err
}

var (
	triedMu sync.Mutex
	tried   = map[string]bool{}
)

// convert converts this YAML file, read as value from text, to TOML; once
// per file and process. Nothing for a TOML file.
func (f *foundFile) convert(text string, value any) error {
	if !f.yaml {
		return nil
	}
	triedMu.Lock()
	again := tried[f.path]
	tried[f.path] = true
	triedMu.Unlock()
	if again {
		return nil
	}
	if p, ok := conversionProblem(text, value); ok {
		return errf("%s", p)
	}
	if err := writeTOMLFile(withExtension(f.path, "toml"), value); err != nil {
		return err
	}
	if _, err := keepAside(f.path); err != nil {
		if _, statErr := os.Stat(f.path); statErr != nil {
			// Another gitdcy converted it at the same moment.
			return nil
		}
		return err
	}
	return nil
}

// conversionProblem is what value, read from the YAML text, would lose as
// TOML: a value TOML cannot hold, a value that reads back differently, or a
// key that is no GitDCY setting (it had no effect, but it is the owner's).
func conversionProblem(text string, value any) (string, bool) {
	tomlText, err := tomlMarshal(value)
	if err != nil {
		return "TOML cannot hold a value in it: " + err.Error(), true
	}
	back := reflect.New(reflect.TypeOf(value).Elem())
	if err := tomlUnmarshal(tomlText, back.Interface()); err != nil {
		return "its TOML does not read back: " + err.Error(), true
	}
	kept, again := toJSONValue(value), toJSONValue(back.Interface())
	if !reflect.DeepEqual(kept, again) {
		return "its TOML reads back different values", true
	}
	tree, err := yamlParse(text)
	if err != nil || tree == nil {
		return "", false
	}
	if key, ok := yamlFirstDroppedKey(tree, kept); ok {
		return key + " is not a GitDCY setting and would be lost", true
	}
	return "", false
}

// toJSONValue is v as JSON values (map[string]any, []any, string, float64,
// bool, nil), for comparing settings as serde_json::Value compared them.
func toJSONValue(v any) any {
	var out any
	if err := json.Unmarshal([]byte(jsonPretty(v)), &out); err != nil {
		return nil
	}
	return out
}

// writeSettings writes value as NAME.toml in folder; a NAME.yaml there,
// which it replaces, is moved aside.
func writeSettings(folder, name string, value any) (string, error) {
	if err := os.MkdirAll(folder, 0o777); err != nil {
		return "", wrapf(err, "create config directory %s", folder)
	}
	path := tomlPath(folder, name)
	if err := writeTOMLFile(path, value); err != nil {
		return "", err
	}
	if y := yamlPath(folder, name); there(y) {
		if _, err := keepAside(y); err != nil {
			return "", err
		}
	}
	return path, nil
}

// writeTOMLFile writes whole or not at all: a temporary file beside it,
// then a rename.
func writeTOMLFile(path string, value any) error {
	text, err := tomlMarshal(value)
	if err != nil {
		return wrapf(err, "write %s as TOML", path)
	}
	name, _ := fileName(path)
	temporary := withFileName(path, fmt.Sprintf(".%s.%d.tmp", name, os.Getpid()))
	if err := os.WriteFile(temporary, []byte(text), 0o666); err != nil {
		return wrapf(err, "write %s", temporary)
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return wrapf(err, "write %s", path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Loading and saving

func localConfigPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return tomlPath(dir, localConfigName), nil
}

func manifestPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return tomlPath(dir, manifestName), nil
}

// loadLocalConfig is this device's settings merged with the current
// folder's.
func loadLocalConfig() LocalConfig {
	config := loadSavedLocalConfig()
	if here, err := os.Getwd(); err == nil {
		config = mergeLocalConfig(config, readLocalConfigFile(here, folderConfig, func() bool {
			return mayConvert() && gitIgnoresConverted(here)
		}))
	}
	return config
}

// loadSavedLocalConfig is this device's own settings (local.toml).
func loadSavedLocalConfig() LocalConfig {
	dir, err := configDir()
	if err != nil {
		return LocalConfig{}
	}
	return readLocalConfigFile(dir, localConfigName, mayConvert)
}

func saveLocalConfig(config LocalConfig) (string, error) {
	// Writing an unreadable file's stand-in would replace the owner's
	// settings.
	if config.ConfigError != nil {
		return "", errf("%s; fix it before GitDCY changes it", *config.ConfigError)
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	path, err := writeSettings(dir, localConfigName, &config)
	return path, wrap(err, "write local config")
}

// gitIgnoresConverted reports whether git ignores a folder's YAML settings
// file and what the conversion leaves (.gitdcy.local.toml and the kept
// YAML): it never puts settings in front of `git add` or takes a tracked
// file away.
func gitIgnoresConverted(folder string) bool {
	names := []string{folderConfig + ".yaml", folderConfig + ".toml", folderConfig + ".yaml.bak-" + today()}
	cmd := exec.Command("git", append([]string{"-C", folder, "check-ignore", "--"}, names...)...)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		if _, ok := err.(*exec.ExitError); !ok {
			return false
		}
	}
	ignored := lines(lossy(out))
	for _, n := range names {
		if !contains(ignored, n) {
			return false
		}
	}
	return true
}

func readLocalConfigFile(folder, name string, may func() bool) LocalConfig {
	found := findSettings(folder, name)
	if found == nil {
		return LocalConfig{}
	}
	text, err := readText(found.path)
	if err != nil {
		return invalidLocalConfig(fmt.Sprintf("could not read GitDCY config %s: %s", found.path, err))
	}
	config, err := found.parseLocalConfig(text)
	if err != nil {
		return invalidLocalConfig(fmt.Sprintf("could not parse GitDCY config %s: %s", found.path, err))
	}
	if found.yaml && may() {
		// A file that does not convert stays YAML; doctor says why.
		_ = found.convert(text, &config)
	}
	return config
}

// configNotes lists the settings files that are still YAML or stand beside
// their TOML, each with why: what `gitdcy doctor` lists after its config
// line.
func configNotes() []string {
	var notes []string
	dir, err := configDir()
	if err != nil {
		return notes
	}
	type place struct{ folder, name string }
	places := []place{{dir, localConfigName}, {dir, manifestName}}
	if here, err := os.Getwd(); err == nil {
		places = append(places, place{here, folderConfig})
	}
	for _, p := range places {
		y := yamlPath(p.folder, p.name)
		if _, err := os.Stat(y); err != nil {
			continue
		}
		t := tomlPath(p.folder, p.name)
		if _, err := os.Stat(t); err == nil {
			notes = append(notes, fmt.Sprintf("%s is not read: %s is", y, t))
			continue
		}
		text, _ := readText(y)
		var problem string
		var has bool
		var readErr error
		if p.name == manifestName {
			var m WorkspaceManifest
			if m, readErr = yamlManifest(text); readErr == nil {
				problem, has = conversionProblem(text, &m)
			}
		} else {
			var c LocalConfig
			if c, readErr = yamlLocalConfig(text); readErr == nil {
				problem, has = conversionProblem(text, &c)
			}
		}
		var why string
		switch {
		case readErr != nil:
			why = "cannot be read: " + readErr.Error()
		case has:
			why = "is read as YAML, not converted to TOML: " + problem
		case !mayConvert():
			why = "is read as YAML, not converted while GITDCY_READ_ONLY_STATUS=1 (or by a library user)"
		case p.name == folderConfig:
			why = fmt.Sprintf("is read as YAML; it is converted once git ignores %s.toml and %s.yaml.bak-* here", folderConfig, folderConfig)
		default:
			why = fmt.Sprintf("is read as YAML; converting it failed (can %s be written?)", p.folder)
		}
		notes = append(notes, y+" "+why)
	}
	return notes
}

// invalidLocalConfig stands in for settings that could not be read: an
// identity profile that matches nothing, so writes fail closed.
func invalidLocalConfig(message string) LocalConfig {
	return LocalConfig{
		RequireIdentity:  ptr(true),
		IdentityProfiles: &map[string]GitIdentityProfile{"__invalid_local_config__": {}},
		ConfigError:      &message,
	}
}

func mergeLocalConfig(base, next LocalConfig) LocalConfig {
	if next.WorkspaceRoot != nil {
		base.WorkspaceRoot = next.WorkspaceRoot
	}
	if next.ScanRoots != nil {
		base.ScanRoots = next.ScanRoots
	}
	if next.SyncRemoteTemplate != nil {
		base.SyncRemoteTemplate = next.SyncRemoteTemplate
	}
	if next.OriginRemoteTemplates != nil {
		m := copyMap(base.OriginRemoteTemplates)
		for k, v := range *next.OriginRemoteTemplates {
			m[k] = v
		}
		base.OriginRemoteTemplates = &m
	}
	if next.LocalSyncFiles != nil {
		base.LocalSyncFiles = mergeListMap(base.LocalSyncFiles, *next.LocalSyncFiles)
	}
	if next.TrustedWipDevices != nil {
		base.TrustedWipDevices = mergeListMap(base.TrustedWipDevices, *next.TrustedWipDevices)
	}
	if next.VisibilityOverrides != nil {
		m := copyMap(base.VisibilityOverrides)
		for k, v := range *next.VisibilityOverrides {
			m[k] = v
		}
		base.VisibilityOverrides = &m
	}
	if next.PrivateRemotePatterns != nil {
		base.PrivateRemotePatterns = mergeOptionalList(base.PrivateRemotePatterns, *next.PrivateRemotePatterns)
	}
	if next.PublicExportRemotes != nil {
		base.PublicExportRemotes = mergeOptionalList(base.PublicExportRemotes, *next.PublicExportRemotes)
	}
	if next.IgnoreProfiles != nil {
		base.IgnoreProfiles = mergeListMap(base.IgnoreProfiles, *next.IgnoreProfiles)
	}
	if next.RequireIdentity != nil {
		base.RequireIdentity = next.RequireIdentity
	}
	if next.IdentityProfiles != nil {
		m := copyMap(base.IdentityProfiles)
		for k, v := range *next.IdentityProfiles {
			m[k] = v
		}
		base.IdentityProfiles = &m
	}
	if next.RequireChecks != nil {
		base.RequireChecks = next.RequireChecks
	}
	if next.CheckProfiles != nil {
		m := copyMap(base.CheckProfiles)
		for k, v := range *next.CheckProfiles {
			m[k] = v
		}
		base.CheckProfiles = &m
	}
	// rules_file is not taken from next: a .gitdcy.local.toml in the
	// current folder could come with a repository and point elsewhere.
	if next.ConfigError != nil {
		base.ConfigError = next.ConfigError
	}
	return base
}

func copyMap[V any](m *map[string]V) map[string]V {
	out := map[string]V{}
	if m != nil {
		for k, v := range *m {
			out[k] = v
		}
	}
	return out
}

func mergeOptionalList(base *[]string, next []string) *[]string {
	list := []string{}
	if base != nil {
		list = append(list, *base...)
	}
	list = append(list, next...)
	list = sortDedup(list)
	return &list
}

func mergeListMap(base *map[string][]string, next map[string][]string) *map[string][]string {
	m := map[string][]string{}
	if base != nil {
		for k, v := range *base {
			m[k] = append([]string{}, v...)
		}
	}
	for k, v := range next {
		m[k] = sortDedup(append(m[k], v...))
	}
	return &m
}

// sortDedup sorts and drops repeats (Rust's sort then dedup).
func sortDedup(list []string) []string {
	sort.Strings(list)
	out := list[:0]
	for i, v := range list {
		if i == 0 || v != list[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func ensureScanRoot(config *LocalConfig, root string) {
	if config.ScanRoots == nil {
		config.ScanRoots = &[]string{}
	}
	roots := *config.ScanRoots
	found := false
	for _, r := range roots {
		if expandHome(r) == root {
			found = true
		}
	}
	if !found {
		roots = append(roots, root)
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i] < roots[j] })
	config.ScanRoots = &roots
}

func workspaceRootWithConfig(config LocalConfig) string {
	if config.WorkspaceRoot != nil {
		return expandHome(*config.WorkspaceRoot)
	}
	home, ok := homeDir()
	if !ok {
		home = "."
	}
	return joinPath(home, "Code")
}

func defaultWorkspaceRoot() string {
	return workspaceRootWithConfig(LocalConfig{WorkspaceRoot: loadLocalConfig().WorkspaceRoot})
}

func setWorkspaceRoot(root string) (string, error) {
	root = expandHome(root)
	config := loadSavedLocalConfig()
	config.WorkspaceRoot = &root
	ensureScanRoot(&config, root)
	return saveLocalConfig(config)
}

func defaultScanRoots() []string {
	config := loadLocalConfig()
	if config.ScanRoots != nil && len(*config.ScanRoots) > 0 {
		roots := []string{}
		for _, r := range *config.ScanRoots {
			roots = append(roots, expandHome(r))
		}
		return roots
	}
	return defaultCandidateScanRoots()
}

func defaultCandidateScanRoots() []string {
	roots := []string{}
	if !readOnlyStatus() {
		roots = append(roots, defaultWorkspaceRoot())
	}
	if home, ok := homeDir(); ok {
		for _, provider := range []string{"github", "forgejo", "gitlab"} {
			root := joinPath(joinPath(home, "Documents"), provider)
			if _, err := os.Stat(root); err == nil && !contains(roots, root) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

func addScanRoot(root string) (string, error) {
	root = expandHome(root)
	config := loadSavedLocalConfig()
	if config.ScanRoots == nil || len(*config.ScanRoots) == 0 {
		ensureScanRoot(&config, workspaceRootWithConfig(config))
	}
	ensureScanRoot(&config, root)
	return saveLocalConfig(config)
}

func syncRemoteTemplate() (string, bool) {
	c := loadLocalConfig()
	if c.SyncRemoteTemplate != nil && trim(*c.SyncRemoteTemplate) != "" {
		return *c.SyncRemoteTemplate, true
	}
	return "", false
}

func localSyncFileEnabled(entry RepoEntry, file string) bool {
	f, ok := safeRelativeLocalSyncPath(file)
	if !ok {
		return false
	}
	return contains(configuredLocalSyncFiles(entry, loadLocalConfig()), f)
}

func setLocalSyncFile(entry RepoEntry, file string, enabled bool) (string, error) {
	f, ok := safeRelativeLocalSyncPath(file)
	if !ok {
		return "", errf("invalid local sync file path: %s", file)
	}
	config := loadSavedLocalConfig()
	files := copyListMap(config.LocalSyncFiles)
	repoFiles := files[entry.ID]
	if enabled {
		if !contains(repoFiles, f) {
			repoFiles = append(repoFiles, f)
			sort.Strings(repoFiles)
		}
		files[entry.ID] = repoFiles
		config.LocalSyncFiles = &files
	} else {
		repoFiles = removeAll(repoFiles, f)
		if len(repoFiles) == 0 {
			delete(files, entry.ID)
		} else {
			files[entry.ID] = repoFiles
		}
		config.LocalSyncFiles = &files
		if len(files) == 0 {
			config.LocalSyncFiles = nil
		}
	}
	return saveLocalConfig(config)
}

func copyListMap(m *map[string][]string) map[string][]string {
	out := map[string][]string{}
	if m != nil {
		for k, v := range *m {
			out[k] = append([]string{}, v...)
		}
	}
	return out
}

func removeAll(list []string, value string) []string {
	out := []string{}
	for _, v := range list {
		if v != value {
			out = append(out, v)
		}
	}
	return out
}

func wipDeviceTrusted(entry RepoEntry, device string) bool {
	return wipDeviceTrustedWithConfig(entry, device, loadLocalConfig())
}

func setWipDeviceTrusted(entry RepoEntry, device string, trusted bool) (string, error) {
	return setWipDeviceTrustedForKey(entry.ID, device, trusted)
}

func setWipDeviceTrustedGlobally(device string, trusted bool) (string, error) {
	return setWipDeviceTrustedForKey("*", device, trusted)
}

func setWipDeviceTrustedForKey(key, device string, trusted bool) (string, error) {
	d, ok := normalizeDeviceID(device)
	if !ok {
		return "", errf("invalid device: %s", device)
	}
	config := loadSavedLocalConfig()
	devices := copyListMap(config.TrustedWipDevices)
	list := devices[key]
	if trusted {
		if !contains(list, d) {
			list = append(list, d)
			sort.Strings(list)
		}
		devices[key] = list
		config.TrustedWipDevices = &devices
	} else {
		list = removeAll(list, d)
		if len(list) == 0 {
			delete(devices, key)
		} else {
			devices[key] = list
		}
		config.TrustedWipDevices = &devices
		if len(devices) == 0 {
			config.TrustedWipDevices = nil
		}
	}
	return saveLocalConfig(config)
}

func suggestedOriginRemote(entry RepoEntry) (string, bool) {
	c := loadLocalConfig()
	if c.OriginRemoteTemplates == nil {
		return "", false
	}
	template, ok := (*c.OriginRemoteTemplates)[entry.Provider.folder()]
	if !ok || trim(template) == "" {
		return "", false
	}
	return applyRemoteTemplate(template, entry), true
}

func applyRemoteTemplate(template string, entry RepoEntry) string {
	name, ok := fileName(entry.Path)
	if !ok {
		name = "repo"
	}
	out := strings.ReplaceAll(template, "{repo}", name)
	out = strings.ReplaceAll(out, "{id}", entry.ID)
	return strings.ReplaceAll(out, "{provider}", entry.Provider.folder())
}

// loadOrDiscoverManifest is the saved workspace, else the repositories found
// under the scan roots.
func loadOrDiscoverManifest() (WorkspaceManifest, error) {
	dir, err := configDir()
	if err != nil {
		return WorkspaceManifest{}, err
	}
	if found := findSettings(dir, manifestName); found != nil {
		text, err := readText(found.path)
		if err != nil {
			return WorkspaceManifest{}, wrapf(err, "read manifest %s", found.path)
		}
		manifest, err := found.parseManifest(text)
		if err != nil {
			return WorkspaceManifest{}, wrapf(err, "parse manifest %s", found.path)
		}
		if mayConvert() {
			// A file that does not convert stays YAML; doctor says why.
			_ = found.convert(text, &manifest)
		}
		return manifest, nil
	}
	var roots []string
	if readOnlyStatus() {
		roots = defaultCandidateScanRoots()
	} else {
		roots = defaultScanRoots()
	}
	repos, err := discoverEntries(roots)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	return WorkspaceManifest{WorkspaceRoot: defaultWorkspaceRoot(), Repos: repos}, nil
}

func saveManifest(manifest WorkspaceManifest) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	_, err = writeSettings(dir, manifestName, &manifest)
	return wrap(err, "write manifest")
}
