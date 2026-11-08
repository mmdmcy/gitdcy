package main

// What every test shares: a home folder of its own, so no test reads or
// writes this machine's real settings (~/.config/gitdcy, the global git
// config), and the gitdcy binary built once for tests that run it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var testHome string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitdcy-test-home-")
	if err != nil {
		panic(err)
	}
	testHome = dir
	for _, name := range []string{"HOME", "USERPROFILE"} {
		os.Setenv(name, dir)
	}
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	os.Setenv("APPDATA", filepath.Join(dir, "AppData"))
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, ".gitconfig"))
	for _, name := range []string{"GITDCY_READ_ONLY_STATUS", "GITDCY_CORRELATION_ID", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		os.Unsetenv(name)
	}
	code := m.Run()
	os.RemoveAll(dir)
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

var (
	binOnce sync.Once
	binDir  string
	binPath string
	binErr  error
)

// gitdcyBinary is this package built as a program, once per test run.
func gitdcyBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "gitdcy-test-bin-")
		if binErr != nil {
			return
		}
		name := "gitdcy"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binPath = filepath.Join(binDir, name)
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			binErr = &buildError{string(out)}
		}
	})
	if binErr != nil {
		t.Fatalf("building gitdcy: %v", binErr)
	}
	return binPath
}

type buildError struct{ out string }

func (e *buildError) Error() string { return e.out }
