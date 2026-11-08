package main

// Paths as Rust's std::path treated them, so every path GitDCY prints or
// compares is the same as before: join without cleaning, file names and
// component-wise prefixes.

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

func isSep(c byte) bool {
	return c == '/' || (runtime.GOOS == "windows" && c == '\\')
}

// joinPath is Rust's Path::join: rel replaces base when it is absolute,
// else it is added after one separator; nothing is cleaned.
func joinPath(base, rel string) string {
	if rel == "" {
		if base == "" || isSep(base[len(base)-1]) {
			return base
		}
		return base + string(os.PathSeparator)
	}
	if filepath.IsAbs(rel) || isSep(rel[0]) || filepath.VolumeName(rel) != "" {
		return rel
	}
	if base == "" {
		return rel
	}
	if isSep(base[len(base)-1]) {
		return base + rel
	}
	return base + string(os.PathSeparator) + rel
}

// components is a path's parts as Rust's Path::components gave them,
// without "." parts and empty parts (the root is not included).
func components(p string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || isSep(p[i]) {
			part := p[start:i]
			if part != "" && part != "." {
				parts = append(parts, part)
			}
			start = i + 1
		}
	}
	return parts
}

// fileName is Rust's Path::file_name: the last part, false when there is
// none or it is "..".
func fileName(p string) (string, bool) {
	parts := components(p)
	if len(parts) == 0 {
		return "", false
	}
	last := parts[len(parts)-1]
	if last == ".." {
		return "", false
	}
	if runtime.GOOS == "windows" && len(parts) == 1 && filepath.VolumeName(p) != "" && strings.HasSuffix(filepath.VolumeName(p), last) {
		return "", false
	}
	return last, true
}

// pathStartsWith is Rust's Path::starts_with: whole components only.
func pathStartsWith(p, prefix string) bool {
	if filepath.IsAbs(prefix) != filepath.IsAbs(p) {
		return false
	}
	a, b := components(p), components(prefix)
	if len(b) > len(a) {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// canonicalize is Rust's fs::canonicalize: absolute, symbolic links
// resolved; an error when the path is not there.
func canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// homeDir is the home folder GitDCY expands `~/` with: $HOME, or
// %USERPROFILE% on Windows, as set.
func homeDir() (string, bool) {
	if runtime.GOOS == "windows" {
		return os.LookupEnv("USERPROFILE")
	}
	return os.LookupEnv("HOME")
}

// stdHomeDir is Rust's std::env::home_dir: $HOME (%USERPROFILE%) when set
// and not empty, else the user's entry in the password database.
func stdHomeDir() (string, bool) {
	name := "HOME"
	if runtime.GOOS == "windows" {
		name = "USERPROFILE"
	}
	if v := os.Getenv(name); v != "" {
		return v, true
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, true
	}
	return "", false
}

// expandHome turns "~" and "~/rest" into the home folder.
func expandHome(p string) string {
	if p == "~" {
		if home, ok := homeDir(); ok {
			return home
		}
		return p
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, ok := homeDir(); ok {
			return joinPath(home, rest)
		}
	}
	return p
}

// configDir is GitDCY's config folder, where the Rust build (and before it
// the directories crate, 6.0) put it: $XDG_CONFIG_HOME/gitdcy (when
// absolute, else ~/.config/gitdcy) on Linux and other Unix systems,
// ~/Library/Application Support/dev.gitdcy.GitDCY on macOS and
// %APPDATA%\gitdcy\GitDCY\config on Windows.
func configDir() (string, error) {
	if dir, ok := configDirFrom(runtime.GOOS, os.LookupEnv, stdHomeDir); ok {
		return dir, nil
	}
	return "", errf("could not determine config directory")
}

func configDirFrom(goos string, lookup func(string) (string, bool), home func() (string, bool)) (string, bool) {
	absolute := func(name string) (string, bool) {
		v, ok := lookup(name)
		if !ok || !isAbsFor(goos, v) {
			return "", false
		}
		return v, true
	}
	switch goos {
	case "windows":
		roaming, ok := absolute("APPDATA")
		if !ok {
			return "", false
		}
		return joinPath(joinPath(joinPath(roaming, "gitdcy"), "GitDCY"), "config"), true
	case "darwin", "ios":
		h, ok := home()
		if !ok {
			return "", false
		}
		return joinPath(h, "Library/Application Support/dev.gitdcy.GitDCY"), true
	}
	h, ok := home()
	if !ok {
		return "", false
	}
	base, ok := absolute("XDG_CONFIG_HOME")
	if !ok {
		base = joinPath(h, ".config")
	}
	return joinPath(base, "gitdcy"), true
}

func isAbsFor(goos, p string) bool {
	if goos == "windows" {
		return filepath.IsAbs(p)
	}
	return strings.HasPrefix(p, "/")
}

// withFileName is Rust's Path::with_file_name.
func withFileName(p, name string) string {
	dir := filepath.Dir(p)
	if dir == "." && !strings.HasPrefix(p, "."+string(os.PathSeparator)) {
		return name
	}
	return joinPath(dir, name)
}

// withExtension is Rust's Path::with_extension for a name with one.
func withExtension(p, ext string) string {
	name, ok := fileName(p)
	if !ok {
		return p
	}
	stem := name
	if i := strings.LastIndex(name, "."); i > 0 {
		stem = name[:i]
	}
	return withFileName(p, stem+"."+ext)
}
