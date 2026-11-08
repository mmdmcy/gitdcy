package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// What serde_norway 0.9.42 wrote for these settings (its output, frozen
// before it left the Rust build).
func yamlFixture(t *testing.T, name string) string {
	return readFile(t, filepath.Join("testdata", "yaml", name))
}

func TestAYAMLFileBecomesTOMLWithTheSameValuesAndAKeptCopy(t *testing.T) {
	for _, name := range []string{"local.yaml", "empty.yaml"} {
		text := yamlFixture(t, name)
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, "local.yaml"), []byte(text), 0o644))
		found := findSettings(dir, "local")
		if found == nil || !found.yaml {
			t.Fatal("not found as YAML")
		}
		read, err := found.parseLocalConfig(text)
		must(t, err)
		must(t, found.convert(text, &read))
		if _, err := os.Stat(filepath.Join(dir, "local.yaml")); err == nil {
			t.Fatal("the YAML is still there")
		}
		kept, _ := filepath.Glob(filepath.Join(dir, "local.yaml.bak-*"))
		if len(kept) != 1 || readFile(t, kept[0]) != text {
			t.Fatalf("kept: %v", kept)
		}
		found = findSettings(dir, "local")
		if found.yaml {
			t.Fatal("still YAML")
		}
		tomlText := readFile(t, found.path)
		back, err := found.parseLocalConfig(tomlText)
		must(t, err)
		if !reflect.DeepEqual(toJSONValue(&back), toJSONValue(&read)) {
			t.Fatalf("%s:\n%s", name, tomlText)
		}
	}
	text := yamlFixture(t, "workspace.yaml")
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(text), 0o644))
	found := findSettings(dir, "workspace")
	read, err := found.parseManifest(text)
	must(t, err)
	must(t, found.convert(text, &read))
	var back WorkspaceManifest
	tomlText := readFile(t, filepath.Join(dir, "workspace.toml"))
	must(t, tomlUnmarshal(tomlText, &back))
	if !reflect.DeepEqual(toJSONValue(&back), toJSONValue(&read)) {
		t.Fatal(tomlText)
	}
	if len(read.Repos) != 2 || read.Repos[1].ID != "other/x: y" {
		t.Fatalf("%+v", read.Repos)
	}
}

func TestWhatWouldBeLostStaysYAML(t *testing.T) {
	dir := t.TempDir()
	text := "rules_file: /r.toml\ncolour: blue\n"
	must(t, os.WriteFile(filepath.Join(dir, "local.yaml"), []byte(text), 0o644))
	found := findSettings(dir, "local")
	read, err := found.parseLocalConfig(text)
	must(t, err)
	err = found.convert(text, &read)
	if err == nil || !strings.Contains(err.Error(), "colour is not a GitDCY setting") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.yaml")); err != nil {
		t.Fatal("the YAML was moved")
	}
	if _, err := os.Stat(filepath.Join(dir, "local.toml")); err == nil {
		t.Fatal("a TOML was written")
	}
	// A none value is not a loss.
	text = "rules_file: /r.toml\nscan_roots: ~\n"
	read, err = yamlLocalConfig(text)
	must(t, err)
	if p, has := conversionProblem(text, &read); has {
		t.Fatal(p)
	}
}

func TestTOMLWinsAndAWriteMovesTheYAMLAside(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "local.yaml"), []byte("rules_file: /old.toml\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "local.toml"), []byte("rules_file = \"/new.toml\"\n"), 0o644))
	if found := findSettings(dir, "local"); found.yaml {
		t.Fatal("YAML won")
	}
	path, err := writeSettings(dir, "local", &LocalConfig{RulesFile: ptr("/written.toml")})
	must(t, err)
	if path != filepath.Join(dir, "local.toml") {
		t.Fatal(path)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.yaml")); err == nil {
		t.Fatal("the YAML is still there")
	}
	if !strings.Contains(readFile(t, path), "/written.toml") {
		t.Fatal(readFile(t, path))
	}
}

// The shipped example, uncommented, and the README's settings examples are
// settings GitDCY reads, every key kept.
func TestTheExamplesAreSettingsGitDCYReads(t *testing.T) {
	var out []string
	for _, line := range strings.Split(readFile(t, ".gitdcy.local.example.toml"), "\n") {
		rest, ok := strings.CutPrefix(line, "# ")
		switch {
		case line == "#":
			out = append(out, "")
		case ok && (strings.HasPrefix(rest, "[") || strings.Contains(rest, " = ")):
			out = append(out, rest)
		default:
			out = append(out, line)
		}
	}
	uncommented := strings.Join(out, "\n")
	blocks := []string{uncommented}
	for i, block := range strings.Split(readFile(t, "README.md"), "```toml\n") {
		if i == 0 {
			continue
		}
		block, _, _ = strings.Cut(block, "```")
		if !strings.Contains(block, "[[repo]]") && !strings.Contains(block, "[hygiene]") {
			blocks = append(blocks, block)
		}
	}
	for _, text := range blocks {
		var config LocalConfig
		if err := tomlUnmarshal(text, &config); err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		written, err := tomlMarshal(&config)
		must(t, err)
		writtenTree, err := tomlParse(written)
		must(t, err)
		givenTree, err := tomlParse(text)
		must(t, err)
		for _, key := range givenTree.keys {
			if _, ok := writtenTree.vals[key]; !ok {
				t.Fatalf("%s is not a setting:\n%s", key, text)
			}
		}
	}
	if len(blocks) < 5 {
		t.Fatalf("%d blocks", len(blocks))
	}
	var all LocalConfig
	must(t, tomlUnmarshal(uncommented, &all))
	if all.IdentityProfiles == nil || len(*all.IdentityProfiles) != 3 || len((*all.CheckProfiles)["rust-project"].Checks) != 3 || !reflect.DeepEqual((*all.TrustedWipDevices)["*"], []string{"laptop-name"}) {
		t.Fatalf("%+v", all)
	}
}
