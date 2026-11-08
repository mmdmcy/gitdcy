package main

// Running git: `git -C REPO ...` and the helpers every part uses.

import (
	"os"
	"os/exec"
	"time"
)

const (
	readOnlyGitOutputLimit = 256 * 1024
	readOnlyGitTimeout     = 2 * time.Second
)

// readOnlyStatus reports whether GITDCY_READ_ONLY_STATUS=1: a caller that
// polls unattended, so git reads are bounded and nothing is converted.
func readOnlyStatus() bool {
	v, ok := os.LookupEnv("GITDCY_READ_ONLY_STATUS")
	return ok && v == "1"
}

// gitCommand is `git -C repo args...`.
func gitCommand(repo string, args ...string) *exec.Cmd {
	return exec.Command("git", append([]string{"-C", repo}, args...)...)
}

// readingGitCommand is `git -C repo args...` for reading a repository that
// may come from anywhere: hooks and fsmonitor off (a repository's own config
// can name a program for either), no optional locks (so `git status` does
// not rewrite the index) and never a password prompt.
func readingGitCommand(repo string, args ...string) *exec.Cmd {
	cmd := gitCommand(repo, args...)
	setEnv(cmd, "GIT_CONFIG_COUNT", "2")
	setEnv(cmd, "GIT_CONFIG_KEY_0", "core.fsmonitor")
	setEnv(cmd, "GIT_CONFIG_VALUE_0", "false")
	setEnv(cmd, "GIT_CONFIG_KEY_1", "core.hooksPath")
	setEnv(cmd, "GIT_CONFIG_VALUE_1", "/dev/null")
	setEnv(cmd, "GIT_OPTIONAL_LOCKS", "0")
	setEnv(cmd, "GIT_TERMINAL_PROMPT", "0")
	return cmd
}

// gitCommandWithPaths is `git -C repo args... -- paths...`.
func gitCommandWithPaths(repo string, args []string, paths []string) *exec.Cmd {
	all := append(append(append([]string{}, args...), "--"), paths...)
	return gitCommand(repo, all...)
}

// gitCommandOutput runs a git command: bounded in read-only status mode.
func gitCommandOutput(cmd *exec.Cmd) (*procOutput, error) {
	if readOnlyStatus() {
		return runBounded(cmd, readOnlyGitOutputLimit, readOnlyGitTimeout)
	}
	out, err := runOutput(cmd)
	if err != nil {
		return nil, wrap(err, "run git command")
	}
	return out, nil
}

// commandError is what a failed command said (its standard error, else its
// standard output, trimmed), else "NAME failed with STATUS".
func commandError(name string, out *procOutput) string {
	stderr, stdout := trim(lossy(out.Stderr)), trim(lossy(out.Stdout))
	detail := stderr
	if detail == "" {
		detail = stdout
	}
	if detail == "" {
		return name + " failed with " + out.statusText()
	}
	return detail
}

// runGit runs cmd and fails with what git said when it fails.
func runGit(cmd *exec.Cmd) (*procOutput, error) {
	out, err := gitCommandOutput(cmd)
	if err != nil {
		return nil, wrap(err, "run git")
	}
	if !out.success() {
		return nil, errf("%s", commandError("git", out))
	}
	return out, nil
}

// git runs `git -C repo args...`.
func git(repo string, args ...string) error {
	_, err := runGit(gitCommand(repo, args...))
	return err
}

// gitOutput is the standard output of `git -C repo args...`.
func gitOutput(repo string, args ...string) (string, error) {
	out, err := runGit(gitCommand(repo, args...))
	if err != nil {
		return "", err
	}
	return lossy(out.Stdout), nil
}

// gitBytes is the raw standard output of `git -C repo args...`.
func gitBytes(repo string, args ...string) ([]byte, error) {
	out, err := runGit(gitCommand(repo, args...))
	if err != nil {
		return nil, err
	}
	return out.Stdout, nil
}

// gitEnv runs `git -C repo args...` with these environment variables
// (pairs: name, value, ...).
func gitEnv(repo string, env []string, args ...string) error {
	cmd := gitCommand(repo, args...)
	for i := 0; i+1 < len(env); i += 2 {
		setEnv(cmd, env[i], env[i+1])
	}
	_, err := runGit(cmd)
	return err
}

// gitOutputEnv is gitOutput with these environment variables.
func gitOutputEnv(repo string, env []string, args ...string) (string, error) {
	cmd := gitCommand(repo, args...)
	for i := 0; i+1 < len(env); i += 2 {
		setEnv(cmd, env[i], env[i+1])
	}
	out, err := runGit(cmd)
	if err != nil {
		return "", err
	}
	return lossy(out.Stdout), nil
}

// gitPaths runs `git -C repo args... -- paths...`.
func gitPaths(repo string, args []string, paths []string) error {
	_, err := runGit(gitCommandWithPaths(repo, args, paths))
	return err
}

// gitPathsEnv is gitPaths with these environment variables.
func gitPathsEnv(repo string, args []string, paths []string, env []string) error {
	cmd := gitCommandWithPaths(repo, args, paths)
	for i := 0; i+1 < len(env); i += 2 {
		setEnv(cmd, env[i], env[i+1])
	}
	_, err := runGit(cmd)
	return err
}
