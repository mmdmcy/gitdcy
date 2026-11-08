package main

import (
	"strings"
	"testing"
)

func rulesTestRule(push Push, protected ...string) RepoRule {
	return RepoRule{Name: "example", Remotes: []string{"forge.example.com/team/example"}, Push: push, Protected: protected}
}

func rulesNoAlias(string) (string, bool) { return "", false }

func TestRulesURLsNormalizeWithoutUserPasswordPortOrDotGit(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:Org/repo.git":                           "github.com/org/repo",
		"ssh://git@forge.example.com:2222/team/repo.git":        "forge.example.com/team/repo",
		"https://user:s3cret@forge.example.com:3000/team/repo/": "forge.example.com/team/repo",
		"forge-alias:team/repo.git":                             "forge-alias/team/repo",
		"/srv/Git/repo.git/":                                    "/srv/Git/repo.git",
		"file:///srv/git/repo.git":                              "/srv/git/repo.git",
	} {
		if got := normalizeURL(url); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", url, got, want)
		}
	}
	if !isLocal(`C:\repos\x.git`) || !isLocal("./a:b") || isLocal("git@host:x") {
		t.Error("isLocal")
	}
}

func TestRulesARemotePrefixWithCapitalsMatchesAsGitHubDoes(t *testing.T) {
	push := Push{Kind: pushRemotes, Remotes: []string{"github.com/Example-Org/"}}
	for _, url := range []string{"git@github.com:example-org/repo.git", "https://github.com/Example-Org/repo"} {
		if !remoteAllowed(push, url, nil, rulesNoAlias) {
			t.Errorf("%s refused", url)
		}
	}
	if remoteAllowed(push, "https://github.com/example-org-other/repo", nil, rulesNoAlias) {
		t.Error("example-org-other allowed")
	}
}

func TestRulesRemotePrefixesEndAtAPathBoundary(t *testing.T) {
	push := Push{Kind: pushRemotes, Remotes: []string{"github.com/example-org/"}}
	for url, want := range map[string]bool{
		"git@github.com:Example-Org/repo.git":              true,
		"https://github.com/example-org/repo":              true,
		"https://github.com/example-org-other/repo":        false,
		"https://evil.example/github.com/example-org/repo": false,
		"git@github.com:someone/repo.git":                  false,
	} {
		if got := remoteAllowed(push, url, nil, rulesNoAlias); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
}

func TestRulesSelfhostedMeansListedHostsLocalPathsAndAliasesToThem(t *testing.T) {
	hosts := []string{"forge.example.com", "192.0.2.10"}
	push := Push{Kind: pushSelfhosted}
	for url, want := range map[string]bool{
		"ssh://git@192.0.2.10:2222/me/repo.git":     true,
		"http://forge.example.com:3000/me/repo.git": true,
		"/srv/git/repo.git":                         true,
		"ssh://localhost/srv/repo.git":              true,
		"https://github.com/me/repo":                false,
		"git@github.com:me/repo.git":                false,
	} {
		if got := remoteAllowed(push, url, hosts, rulesNoAlias); got != want {
			t.Errorf("%s: %v, want %v", url, got, want)
		}
	}
	alias := func(a string) (string, bool) { return "forge.example.com", a == "forge" }
	if !remoteAllowed(push, "forge:me/repo.git", hosts, alias) {
		t.Error("an alias to a listed host is refused")
	}
	// An alias is SSH's; an https URL to a name is not resolved through it.
	if remoteAllowed(push, "https://forge/me/repo.git", hosts, alias) {
		t.Error("an https URL went through an SSH alias")
	}
	if remoteAllowed(push, "ssh://git@forge.example.com:2222/x", nil, rulesNoAlias) {
		t.Error("allowed with no hosts listed")
	}
	for _, unsafe := range []string{"-oProxyCommand=x", "host;x"} {
		if _, ok := sshHostname(unsafe); ok {
			t.Errorf("ssh -G ran for %q", unsafe)
		}
	}
}

func TestRulesNowhereRefusesEveryRemoteAndAnywhereNone(t *testing.T) {
	for _, url := range []string{"/srv/git/repo.git", "git@github.com:me/repo.git", "ssh://localhost/x"} {
		if remoteAllowed(Push{Kind: pushNowhere}, url, []string{"localhost"}, rulesNoAlias) {
			t.Errorf("nowhere allowed %s", url)
		}
		if !remoteAllowed(Push{}, url, nil, rulesNoAlias) {
			t.Errorf("anywhere refused %s", url)
		}
	}
}

func TestRulesProtectedBranchesTakeNoDirectPush(t *testing.T) {
	r := rulesTestRule(Push{}, "main", "development")
	if got := protectedIn(r, []string{"feat/x", "development", "development"}); strings.Join(got, ",") != "development" {
		t.Errorf("got %v", got)
	}
	if got := protectedIn(r, []string{"feat/x"}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestRulesTheRulesFileReadsPushAsAWordOrAList(t *testing.T) {
	file, err := rulesParse(`
selfhosted = ["forge.example.com"]
[[repo]]
name = "a"
first_commit = "0123456789abcdef0123456789abcdef01234567"
push = "selfhosted"
protected = ["main"]
note = "stays home"
[[repo]]
name = "b"
remotes = ["github.com/example-org/b"]
push = ["github.com/example-org/"]
[[repo]]
name = "c"
remotes = ["forge.example.com/me/c"]
`)
	if err != nil {
		t.Fatal(chainText(err))
	}
	if err := file.validate(); err != nil {
		t.Fatal(err)
	}
	if !file.Repos[0].Push.equal(Push{Kind: pushSelfhosted}) ||
		!file.Repos[1].Push.equal(Push{Kind: pushRemotes, Remotes: []string{"github.com/example-org/"}}) ||
		!file.Repos[2].Push.equal(Push{}) {
		t.Errorf("push read as %+v", file.Repos)
	}
	text, err := tomlMarshal(&file)
	if err != nil {
		t.Fatal(err)
	}
	again, err := rulesParse(text)
	if err != nil {
		t.Fatalf("%s\n%s", chainText(err), text)
	}
	if !again.equal(&file) {
		t.Errorf("read back differently:\n%s", text)
	}
}

func TestRulesARulesFileWithMistakesIsRefused(t *testing.T) {
	for _, text := range []string{
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npush = \"github\"\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npush = []\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npushh = \"nowhere\"\n",
		"selfhost = [\"x\"]\n",
	} {
		if _, err := rulesParse(text); err == nil {
			t.Errorf("read: %s", text)
		}
	}
	for _, text := range []string{
		"[[repo]]\nname = \"a\"\npush = \"nowhere\"\n",
		"[[repo]]\nname = \"a\"\nfirst_commit = \"abc123\"\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\n[[repo]]\nname = \"a\"\nremotes = [\"x/z\"]\n",
		"[[repo]]\nname = \" \"\nremotes = [\"x/y\"]\n",
	} {
		file, err := rulesParse(text)
		if err != nil {
			t.Fatalf("%s: %s", text, chainText(err))
		}
		if file.validate() == nil {
			t.Errorf("valid: %s", text)
		}
	}
}

// The rules file's mistakes are shown as the Rust build showed them: the
// first under its keys in sorted order, an unknown key under the key, a
// missing name under the [[repo]] header or the inline table, serde's
// "expected `a` or `b`" (each text as the Rust build printed it).
func TestRulesTOMLMistakesAreShownAsTheTomlCrateShowedThem(t *testing.T) {
	for text, want := range map[string]string{
		"zzz = 1\naaa = 2\n": "TOML parse error at line 2, column 1\n  |\n2 | aaa = 2\n  | ^^^\n" +
			"unknown field `aaa`, expected one of `selfhosted`, `personal`, `allow`, `hygiene`, `repo`\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\npush = \"github\"\nfirst_commit = \"abc\"\nprotected = 5\n": "TOML parse error at line 6, column 13\n  |\n6 | protected = 5\n  |             ^\n" +
			"invalid type: integer `5`, expected a sequence\n",
		"hygiene.days = 3\n": "TOML parse error at line 1, column 9\n  |\n1 | hygiene.days = 3\n  |         ^^^^\n" +
			"unknown field `days`, expected `branch_days` or `skip`\n",
		"[[repo]]\nname = \"a\"\nremotes = [\"x/y\"]\n\n  [[repo]]   # indented\n  remotes = [\"x/z\"]\n": "TOML parse error at line 5, column 3\n  |\n5 |   [[repo]]   # indented\n  |   ^^^^^^^^\n" +
			"missing field `name`\n",
		"repo = [{remotes = [\"x/y\"]}]\n": "TOML parse error at line 1, column 9\n  |\n1 | repo = [{remotes = [\"x/y\"]}]\n  |         ^^^^^^^^^^^^^^^^^^^\n" +
			"missing field `name`\n",
	} {
		_, err := rulesParse(text)
		if err == nil || err.Error() != want {
			t.Errorf("%q:\n%v\nwant\n%s", text, err, want)
		}
	}
}

func TestRulesTheHookChecksThenChainsAndStopsOnARefusal(t *testing.T) {
	text := rulesHookText("pre-push.bak-2026-01-02", "/opt/it's/gitdcy")
	for _, want := range []string{
		rulesHookMark,
		`rules check --stdin "$1" "$2" || exit 1`,
		`"$(dirname "$0")/$chained" "$@" || exit 1`,
		`gitdcy='/opt/it'\''s/gitdcy'`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the hook lacks %s:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "#!/bin/sh\n") || strings.Contains(text, "--no-verify") {
		t.Errorf("hook:\n%s", text)
	}
	if got := rulesChainedOf(text); got != "pre-push.bak-2026-01-02" {
		t.Errorf("chained %q", got)
	}
	if got := rulesChainedOf(rulesHookText("", "")); got != "" {
		t.Errorf("chained %q", got)
	}
}

func TestRulesPushedRefsAreReadFromTheHookInput(t *testing.T) {
	refs, err := parsePushedRefs("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n\n")
	if err != nil || len(refs) != 1 {
		t.Fatalf("%v %v", refs, err)
	}
	if _, err := parsePushedRefs("nonsense\n"); err == nil {
		t.Error("nonsense read")
	}
	if _, err := parsePushedRefs("refs/heads/main --output=/tmp/x refs/heads/main 0000000000000000000000000000000000000000\n"); err == nil {
		t.Error("an option read as a commit")
	}
	if refs[0].RemoteRef != "refs/heads/main" || !rulesIsZero(refs[0].RemoteSha) || rulesIsZero("") {
		t.Errorf("%+v", refs[0])
	}
}

func TestRulesDatesAreCivilDays(t *testing.T) {
	for days, want := range map[int64]string{0: "1970-01-01", 11016: "2000-02-29", 20761: "2026-11-04"} {
		if got := civilDate(days); got != want {
			t.Errorf("civilDate(%d) = %s, want %s", days, got, want)
		}
	}
}
