package main

// Stand-in for yaml.go (the YAML settings reader), which replaces this file.

type yamlNode struct{}

// yamlParse parses YAML settings text into its tree; nil for an empty
// document.
func yamlParse(text string) (*yamlNode, error) { return nil, errf("no YAML reader yet") }

// yamlLocalConfig reads YAML settings text as the Rust build typed it.
func yamlLocalConfig(text string) (LocalConfig, error) {
	return LocalConfig{}, errf("no YAML reader yet")
}

// yamlManifest reads a YAML workspace file as the Rust build typed it.
func yamlManifest(text string) (WorkspaceManifest, error) {
	return WorkspaceManifest{}, errf("no YAML reader yet")
}

// yamlFirstDroppedKey is the first key in the tree that kept (the typed
// settings as JSON values) does not hold, as a dotted path.
func yamlFirstDroppedKey(node *yamlNode, kept any) (string, bool) { return "", false }
