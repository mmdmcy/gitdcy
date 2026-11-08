package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func yamlS(text string, plain bool) *yamlNode {
	return &yamlNode{kind: yamlScalar, text: text, plain: plain}
}

func yamlL(items ...*yamlNode) *yamlNode { return &yamlNode{kind: yamlSeq, items: items} }

// yamlM is a mapping from key, value, key, value, ...
func yamlM(pairs ...any) *yamlNode {
	node := &yamlNode{kind: yamlMap}
	for i := 0; i < len(pairs); i += 2 {
		node.entries = append(node.entries, yamlEntry{key: pairs[i].(string), value: pairs[i+1].(*yamlNode)})
	}
	return node
}

// yamlRender is the tree as one line of text, with each scalar's line when
// withLines (the side-by-side dump's form).
func yamlRender(n *yamlNode, withLines bool) string {
	switch n.kind {
	case yamlScalar:
		kind := "q"
		if n.plain {
			kind = "p"
		}
		if withLines {
			kind = strconv.Itoa(n.line) + ":" + kind
		}
		return kind + ":" + jsonPretty(n.text)
	case yamlSeq:
		parts := make([]string, len(n.items))
		for i, item := range n.items {
			parts[i] = yamlRender(item, withLines)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	parts := make([]string, len(n.entries))
	for i, e := range n.entries {
		parts[i] = jsonPretty(e.key) + ": " + yamlRender(e.value, withLines)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func yamlTree(t *testing.T, text string) string {
	t.Helper()
	node, err := yamlParse(text)
	if err != nil || node == nil {
		t.Fatalf("%q: %v, %v", text, node, err)
	}
	return yamlRender(node, false)
}

func TestYAMLReadsTheBlockStyleAndWhatPeopleWriteByHand(t *testing.T) {
	text := `# a comment
workspace_root: /home/u/Code   # trailing comment
'*': [a, 'b c', "d\te"]
empty_list: []
empty_map: {}
nothing:
tilde: ~
quoted: 'it''s'
url: https://example.com/a#b
repos:
- id: github/app
  enabled: true
  remotes:
  - origin
- id: other
indented:
  - one
  -
    deep: value
`
	want := yamlM(
		"workspace_root", yamlS("/home/u/Code", true),
		"*", yamlL(yamlS("a", true), yamlS("b c", false), yamlS("d\te", false)),
		"empty_list", yamlL(),
		"empty_map", yamlM(),
		"nothing", yamlS("", true),
		"tilde", yamlS("~", true),
		"quoted", yamlS("it's", false),
		"url", yamlS("https://example.com/a#b", true),
		"repos", yamlL(
			yamlM(
				"id", yamlS("github/app", true),
				"enabled", yamlS("true", true),
				"remotes", yamlL(yamlS("origin", true)),
			),
			yamlM("id", yamlS("other", true)),
		),
		"indented", yamlL(
			yamlS("one", true),
			yamlM("deep", yamlS("value", true)),
		),
	)
	if got := yamlTree(t, text); got != yamlRender(want, false) {
		t.Fatalf("got  %s\nwant %s", got, yamlRender(want, false))
	}
	if node, err := yamlParse("# only comments\n\n"); node != nil || err != nil {
		t.Fatalf("only comments: %v, %v", node, err)
	}
	if got, want := yamlTree(t, "---\na: 1\n"), yamlRender(yamlM("a", yamlS("1", true)), false); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// Escapes: \u00e9, \x41, a backslash and a quote.
	escaped := `a: "\` + `u00e9\x41\\ \""` + "\n"
	if got, want := yamlTree(t, escaped), yamlRender(yamlM("a", yamlS("éA\\ \"", false)), false); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestYAMLRefusesWhatItDoesNotRead(t *testing.T) {
	for _, c := range []struct{ text, why string }{
		{"a: &x 1\nb: *x\n", "anchor"},
		{"a: *x\n", "alias"},
		{"a: !!str 1\n", "tag"},
		{"a: |\n  text\n", "several lines"},
		{"a: >\n  text\n", "several lines"},
		{"a: {b: 1}\n", "one line"},
		{"a: 1\n---\nb: 2\n", "second document"},
		{"a:\n\tb: 1\n", "tab"},
		{"a: 'open\n", "does not end"},
		{"a: 1\na: 2\n", "second time"},
		{"a: 1\n  b: 2\n", "indentation"},
		{"a: b: c\n", "colon"},
		{"just text\n", "key"},
		{"a: [b, [c]]\n", "list item"},
		{"a: \"\\q\"\n", "escape"},
		{"%YAML 1.2\na: 1\n", "directive"},
		{"- - a\n", "list inside a list"},
		{"a: - b\n", "list on the line"},
	} {
		_, err := yamlParse(c.text)
		if err == nil {
			t.Fatalf("%q: read", c.text)
		}
		if !strings.Contains(err.Error(), c.why) || !strings.HasPrefix(err.Error(), "line ") {
			t.Fatalf("%q: %v", c.text, err)
		}
	}
}

type yamlTestKind string

func (yamlTestKind) enumVariants() []string { return []string{"github"} }

type yamlTestSettings struct {
	Name    string        `json:"name" toml:",required"`
	Count   *uint64       `json:"count"`
	On      bool          `json:"on" toml:",required"`
	Missing *string       `json:"missing"`
	List    []string      `json:"list" toml:",required"`
	Kind    *yamlTestKind `json:"kind"`
}

func TestYAMLTypedValuesAreReadAsTheYAMLWritersReaderReadThem(t *testing.T) {
	var settings yamlTestSettings
	if err := yamlDecode("name: 123\ncount: 0x10\non: True\nmissing: ~\nlist: [~, 'x']\nkind: github\n", &settings); err != nil {
		t.Fatal(err)
	}
	want := yamlTestSettings{Name: "123", Count: ptr(uint64(16)), On: true, List: []string{"~", "x"}, Kind: ptr(yamlTestKind("github"))}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("got %s\nwant %s", jsonPretty(settings), jsonPretty(want))
	}
	// A quoted null is text; yes is not a bool.
	var quoted yamlTestSettings
	if err := yamlDecode("name: x\ncount:\non: false\nmissing: 'null'\nlist: []\n", &quoted); err != nil {
		t.Fatal(err)
	}
	if quoted.Missing == nil || *quoted.Missing != "null" || quoted.Count != nil {
		t.Fatalf("got %s", jsonPretty(quoted))
	}
	for text, message := range map[string]string{
		"name: x\non: yes\nlist: []\n":              `line 2: invalid type: string "yes", expected a boolean`,
		"name: x\non: true\nlist: []\ncount: '5'\n": `line 4: invalid type: string "5", expected u64`,
		"name: x\non: true\nlist: []\ncount: -5\n":  "invalid value: integer `-5`, expected u64",
		"name: x\non: true\nlist: x\n":              `line 3: invalid type: string "x", expected a sequence`,
		"name: x\non: true\nlist: []\nkind: gh\n":   "unknown variant `gh`, expected `github`",
		"name: x\nlist: []\n":                       "missing field `on`",
		"- name: x\n":                               "invalid type: sequence, expected struct yamlTestSettings",
	} {
		var s yamlTestSettings
		if err := yamlDecode(text, &s); err == nil || err.Error() != message {
			t.Fatalf("%q: %v, want %s", text, err, message)
		}
	}
}

func yamlFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "yaml", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestYAMLReadsBackWhatSerdeNorwayWrote(t *testing.T) {
	local := LocalConfig{
		WorkspaceRoot:         ptr("~/Code"),
		ScanRoots:             &[]string{"/home/u/Documents/github", " lead"},
		SyncRemoteTemplate:    ptr("ssh://git@example.internal/o/{repo}.git"),
		OriginRemoteTemplates: &map[string]string{"github": "true", "gitlab": "a: b # c"},
		LocalSyncFiles:        &map[string][]string{"github/app": {".env", "-D"}},
		TrustedWipDevices:     &map[string][]string{"*": {"laptop", "123"}},
		VisibilityOverrides:   &map[string]VisibilityOverride{"github/private": "private"},
		PrivateRemotePatterns: &[]string{},
		PublicExportRemotes:   &[]string{"~", "'q'", "é\t\a"},
		IgnoreProfiles:        &map[string][]string{},
		RequireIdentity:       ptr(true),
		IdentityProfiles: &map[string]GitIdentityProfile{"main": {
			Name:           `A "B"`,
			Email:          "a@b.c",
			PathPrefixes:   []string{"~/x"},
			RemotePatterns: []string{},
			Providers:      []Provider{ProviderForgejo},
		}},
		CheckProfiles: &map[string]CheckProfile{"rust": {
			PathPrefixes:   []string{},
			RemotePatterns: []string{"github.com/o/a"},
			Providers:      []Provider{},
			Checks: []CheckCommand{{
				Name:           "clippy",
				Program:        "cargo",
				Args:           []string{"--", "-D", "warnings"},
				TimeoutSeconds: ptr(uint64(900)),
			}},
			RunBeforePush:        false,
			RequireCleanWorktree: true,
		}},
		RulesFile: ptr("/home/u/fleet/repos.toml"),
	}
	manifest := WorkspaceManifest{
		WorkspaceRoot: "/home/u/Code",
		Repos: []RepoEntry{
			{ID: "github/app", Path: "/home/u/Code/github/app", Provider: ProviderGithub, Enabled: true, PrimaryRemote: ptr("origin"), ReviewRequired: true},
			{ID: "other/x: y", Path: "/home/u/Code/x", Provider: ProviderOther, WipSync: true},
		},
	}
	read, err := yamlLocalConfig(yamlFixture(t, "local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonPretty(read), jsonPretty(local); got != want {
		t.Fatalf("local.yaml:\n%s\nwant\n%s", got, want)
	}
	readManifest, err := yamlManifest(yamlFixture(t, "workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonPretty(readManifest), jsonPretty(manifest); got != want {
		t.Fatalf("workspace.yaml:\n%s\nwant\n%s", got, want)
	}
	read, err = yamlLocalConfig(yamlFixture(t, "empty.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonPretty(read), jsonPretty(LocalConfig{}); got != want {
		t.Fatalf("empty.yaml:\n%s\nwant\n%s", got, want)
	}
}

func TestYAMLFirstDroppedKey(t *testing.T) {
	for text, want := range map[string]string{
		"rules_file: /r.toml\ncolour: blue\n":                             "colour",
		"colour: ~\nidentity_profiles:\n  main:\n    nme: x\n":            "identity_profiles.main.nme",
		"check_profiles:\n  a:\n    checks:\n    - name: x\n      y: 1\n": "check_profiles.a.checks.0.y",
		"workspace_root: ~/Code\ncolour:\n":                               "",
	} {
		c, err := yamlLocalConfig(text)
		if err != nil {
			t.Fatal(err)
		}
		node, _ := yamlParse(text)
		got, _ := yamlFirstDroppedKey(node, toJSONValue(c))
		if got != want {
			t.Fatalf("%q: %q, want %q", text, got, want)
		}
	}
}

// TestYAMLDump writes, for every file in $GITDCY_YAML_DUMP, its tree, its
// settings and workspace as JSON (or the error) and the first key a
// conversion would drop, to $GITDCY_YAML_DUMP_OUT: the form the Rust
// reader's dump had, to compare the two side by side.
func TestYAMLDump(t *testing.T) {
	dir := os.Getenv("GITDCY_YAML_DUMP")
	if dir == "" {
		t.Skip("GITDCY_YAML_DUMP (a folder of YAML files) and GITDCY_YAML_DUMP_OUT (a file) are not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		out.WriteString("== " + entry.Name() + "\n")
		node, err := yamlParse(text)
		switch {
		case err != nil:
			out.WriteString("tree: error: " + err.Error() + "\n")
		case node == nil:
			out.WriteString("tree: none\n")
		default:
			out.WriteString("tree: " + yamlRender(node, true) + "\n")
		}
		local, err := yamlLocalConfig(text)
		yamlDumpTyped(&out, "local", local, err, node)
		manifest, err := yamlManifest(text)
		yamlDumpTyped(&out, "manifest", manifest, err, node)
	}
	if err := os.WriteFile(os.Getenv("GITDCY_YAML_DUMP_OUT"), []byte(out.String()), 0o666); err != nil {
		t.Fatal(err)
	}
}

func yamlDumpTyped(out *strings.Builder, what string, value any, err error, node *yamlNode) {
	if err != nil {
		out.WriteString(what + ": error: " + err.Error() + "\n")
		return
	}
	out.WriteString(what + ": " + jsonPretty(value) + "\n")
	dropped := "-"
	if key, ok := yamlFirstDroppedKey(node, toJSONValue(value)); ok {
		dropped = key
	}
	out.WriteString(what + " dropped: " + dropped + "\n")
}
