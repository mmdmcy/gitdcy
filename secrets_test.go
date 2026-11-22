package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake tokens are put together at run time, so this file holds nothing
// token-shaped itself (other scanners read it too).

// secretFill is n characters taken in turn from alphabet.
func secretFill(alphabet string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[(i*7+3)%len(alphabet)])
	}
	return b.String()
}

const (
	secretHex   = "0123456789abcdef"
	secretAlnum = "aB3dE5gH7jK9mN1pQ2rS4tU6vW8xY0zC"
	secretUpper = "ABCDEFGHJKLMNPQRSTUVWXYZ234567"
)

func secretJoin(parts ...string) string { return strings.Join(parts, "") }

// secretCases are, per pattern, values that are refused and lines that are not.
func secretCases() []struct {
	name     string
	refused  []string
	notFound []string
} {
	pemHead := secretJoin("-----BEGIN ", "RSA ", "PRIVATE", " KEY-----")
	pemPlain := secretJoin("-----BEGIN ", "PRIVATE", " KEY-----")
	return []struct {
		name     string
		refused  []string
		notFound []string
	}{
		{"Supabase access token",
			[]string{secretJoin("sbp", "_", secretFill(secretHex, 40))},
			[]string{secretJoin("sbp", "_", secretFill(secretHex, 39)), secretJoin("sbp", "_", secretFill(secretHex, 41)), secretJoin("sbp", "_", strings.ToUpper(secretFill(secretHex, 40)))}},
		{"Supabase secret key",
			[]string{secretJoin("sb_", "secret_", secretFill(secretAlnum, 31))},
			[]string{secretJoin("sb_", "publishable_", secretFill(secretAlnum, 31)), secretJoin("sb_", "secret_", "short")}},
		{"GitHub token",
			[]string{secretJoin("gh", "p_", secretFill(secretAlnum, 36)), secretJoin("gh", "s_", secretFill(secretAlnum, 36)), secretJoin("gh", "o_", secretFill(secretAlnum, 36))},
			[]string{secretJoin("gh", "p_", secretFill(secretAlnum, 20)), secretJoin("gh", "x_", secretFill(secretAlnum, 36)), secretJoin("xgh", "p_", secretFill(secretAlnum, 36))}},
		{"GitHub fine-grained token",
			[]string{secretJoin("github", "_pat_", secretFill(secretAlnum, 22), "_", secretFill(secretAlnum, 59))},
			[]string{secretJoin("github", "_pat_", secretFill(secretAlnum, 30))}},
		{"Anthropic key",
			[]string{secretJoin("sk-", "ant-", "api03-", secretFill(secretAlnum, 93), "AA")},
			[]string{secretJoin("sk-", "ant-", "api03-..."), secretJoin("sk-", "ant-", strings.Repeat("x", 30))}},
		{"OpenAI key",
			[]string{secretJoin("sk-", "proj-", secretFill(secretAlnum, 120)), secretJoin("sk-", secretFill(secretAlnum, 48))},
			[]string{secretJoin("sk-", "proj-", "your-key-here"), secretJoin("sk-", secretFill(secretAlnum, 47)), secretJoin("sk-", secretFill(secretAlnum, 49))}},
		{"DeepSeek key",
			[]string{secretJoin("sk-", secretFill(secretHex, 32))},
			[]string{secretJoin("sk-", secretFill(secretHex, 31)), secretJoin("ta", "sk-", secretFill(secretHex, 32)), secretJoin("disk-", secretFill(secretHex, 32))}},
		{"AWS access key id",
			[]string{secretJoin("AK", "IA", secretFill(secretUpper, 16)), secretJoin("AS", "IA", secretFill(secretUpper, 16))},
			[]string{secretJoin("AK", "IA", "IOSFODNN7", "EXAMPLE"), secretJoin("AK", "IA", secretFill(secretUpper, 15)), secretJoin("X", "AK", "IA", secretFill(secretUpper, 16))}},
		{"Google API key",
			[]string{secretJoin("AI", "za", secretFill(secretAlnum+"_-", 35))},
			[]string{secretJoin("AI", "za", secretFill(secretAlnum, 34)), secretJoin("AI", "za", secretFill(secretAlnum, 36))}},
		{"Stripe live key",
			[]string{secretJoin("sk", "_live_", secretFill(secretAlnum, 24)), secretJoin("rk", "_live_", secretFill(secretAlnum, 24))},
			[]string{secretJoin("sk", "_test_", secretFill(secretAlnum, 24)), secretJoin("pk", "_live_", secretFill(secretAlnum, 24))}},
		{"Slack token",
			[]string{secretJoin("xo", "xb-", "1234567890-", "9876543210-", secretFill(secretAlnum, 24))},
			[]string{secretJoin("xo", "xb-", "not-a-token"), secretJoin("xo", "xq-", "1234567890-", secretFill(secretAlnum, 24))}},
		{"Hugging Face token",
			[]string{secretJoin("h", "f_", secretFill("aBcDeFgHiJkLmNoPqRsTuVwXyZ", 34))},
			[]string{secretJoin("h", "f_", secretFill(secretAlnum, 34)), secretJoin("h", "f_", secretFill("aBcDeFgHiJ", 33))}},
		{"private key",
			[]string{"\"" + pemPlain + "\\n" + secretFill(secretAlnum+"+/", 64)},
			[]string{pemHead, "\"" + pemPlain + "\\n" + secretFill(secretAlnum, 32), "if strings.Contains(data, \"" + pemHead + "\") {", secretJoin("-----BEGIN ", "PUBLIC", " KEY-----")}},
	}
}

func TestSecretsEachPatternFindsItsTokensAndNotItsLookalikes(t *testing.T) {
	for _, c := range secretCases() {
		for _, value := range c.refused {
			for _, line := range []string{value, "KEY=" + value, "token: \"" + value + "\" here"} {
				if c.name == "private key" {
					line = value
				}
				got := secretsIn(line)
				if len(got) != 1 || got[0].kind != findingToken || !strings.HasPrefix(got[0].shown, c.name+": ") {
					t.Errorf("%s: %q: got %v", c.name, line, got)
				}
			}
		}
		for _, line := range c.notFound {
			if got := secretsIn(line); len(got) != 0 {
				t.Errorf("%s: %q is no token: got %v", c.name, line, got)
			}
		}
	}
}

func TestSecretsTheMaskShowsSixCharactersAndTheLength(t *testing.T) {
	for _, c := range secretCases() {
		for _, value := range c.refused {
			for _, f := range secretsIn(value) {
				shown := strings.TrimPrefix(f.shown, c.name+": ")
				before, after, ok := strings.Cut(shown, "…")
				if !ok || len(before) > 6 || !strings.HasPrefix(after, " of ") || !strings.HasSuffix(after, " characters") {
					t.Errorf("%s: %q", c.name, f.shown)
				}
				// No seven characters of the value appear anywhere in it.
				value := strings.TrimSpace(value)
				for i := 0; i+7 <= len(value); i++ {
					if strings.Contains(f.shown, value[i:i+7]) {
						t.Errorf("%s: %q shows %q", c.name, f.shown, value[i:i+7])
					}
				}
			}
		}
	}
}

func TestSecretsAKeyFileIsItsHeaderThenALineOfTheKey(t *testing.T) {
	head := secretJoin("-----BEGIN ", "OPENSSH ", "PRIVATE", " KEY-----")
	body := secretFill(secretAlnum+"+/", 70)
	var pem secretPEM
	if _, _, ok := pem.next("id:1", head); ok {
		t.Fatal("a header alone is no finding yet")
	}
	at, f, ok := pem.next("id:2", body)
	if !ok || at != "id:1" || f.kind != findingToken || f.shown != "private key: -----B… of 35 characters" {
		t.Errorf("got %q %v %v", at, f, ok)
	}
	for _, next := range []string{"MIIEpA" + "...", "paste the private key here", ""} {
		pem = secretPEM{}
		pem.next("x:1", head)
		if _, _, ok := pem.next("x:2", next); ok {
			t.Errorf("%q after the header is no key", next)
		}
	}
}

func TestSecretsATokenIsFoundWhateverTheAllowList(t *testing.T) {
	token := secretJoin("sbp", "_", secretFill(secretHex, 40))
	got := personalIn("token "+token, nil, map[string]bool{token: true, strings.ToLower(token): true})
	if len(got) != 1 || got[0].kind != findingToken {
		t.Errorf("got %v", got)
	}
}

func TestRulesCmdAPushWithATokenIsRefusedWithoutShowingIt(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	token := secretJoin("sbp", "_", secretFill(secretHex, 40))
	if err := os.MkdirAll(filepath.Join(work, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.commit(work, ".cursor/mcp.json", "{\"token\": \""+token+"\"}\n")
	out := s.gitdcy(work, "rules", "check")
	said := out.text()
	if out.code != 1 || !strings.Contains(said, ".cursor/mcp.json:1: a token or private key (Supabase access token: "+token[:6]+"… of 44 characters)") ||
		!strings.Contains(said, "rotate each one at its provider") {
		t.Fatal(said)
	}
	if strings.Contains(said, token[:7]) {
		t.Errorf("the token is shown: %s", said)
	}

	// A key file: its header and the key's first line.
	s.ok(work, "reset", "-q", "--hard", "HEAD~1")
	s.commit(work, "id_test", secretJoin("-----BEGIN ", "OPENSSH ", "PRIVATE", " KEY-----")+"\n"+secretFill(secretAlnum+"+/", 70)+"\n")
	if said := s.gitdcy(work, "rules", "check").text(); !strings.Contains(said, "id_test:1: a token or private key (private key: -----B… of 35 characters)") {
		t.Fatal(said)
	}

	// The message is read too, and the allow list does not pass a token.
	s.ok(work, "reset", "-q", "--hard", "HEAD~1")
	s.rules("allow = [\"" + token + "\"]\n")
	s.ok(work, "commit", "-q", "--allow-empty", "-m", "deploy with "+token)
	out = s.gitdcy(work, "rules", "check", "--format", "json")
	v := rulesJSON(t, out)
	if out.code != 1 || rulesAt(v, "data", "findings", 0, "kind") != "token" || rulesAt(v, "data", "findings", 0, "place") != "message" {
		t.Fatal(out.text())
	}
	if strings.Contains(out.text(), token[:7]) {
		t.Errorf("the token is shown: %s", out.text())
	}
}

func TestRulesCmdAScanReportsATokenThatReachedARemote(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	token := secretJoin("sk-", secretFill(secretHex, 32))
	s.commit(work, "config.txt", "key = "+token+"\n")
	s.ok(work, "push", "-q", "origin", "main")
	out := s.gitdcy(s.root, "rules", "scan", "--format", "json", work)
	v := rulesJSON(t, out)
	app := rulesAt(v, "data", "checkouts", 0)
	if out.code != 0 || rulesLen(rulesAt(app, "pushed_findings")) != 1 || rulesAt(app, "pushed_findings", 0, "kind") != "token" {
		t.Fatal(out.text())
	}
	said := s.gitdcy(s.root, "rules", "scan", work).text()
	if !strings.Contains(said, "pushed commit") || !strings.Contains(said, "DeepSeek key: "+token[:6]+"… of 35 characters") {
		t.Error(said)
	}
	if strings.Contains(out.text()+said, token[:7]) {
		t.Error("the token is shown")
	}
}

// The rules file lists the personal words itself: the commit that adds one
// to it is not refused for that word. The word elsewhere, and a token in the
// rules file, still are.
func TestRulesCmdTheRulesFileInTheRepositoryIsNotReadForItsOwnWords(t *testing.T) {
	s := newRulesSandbox(t)
	fleet, _ := s.repo("fleet")
	s.rules("personal = []\n")
	s.ok(fleet, "add", "repos.toml")
	s.ok(fleet, "commit", "-qm", "rules")
	s.ok(fleet, "push", "-q", "origin", "main")
	s.ok(fleet, "branch", "-q", "--set-upstream-to=origin/main")

	s.rules("personal = [\"Jane Doe\"]\n")
	s.ok(fleet, "commit", "-qam", "a personal word")
	if out := s.gitdcy(fleet, "rules", "check"); out.code != 0 {
		t.Fatalf("adding a word to the rules file passes: %s", out.text())
	}
	s.ok(fleet, "push", "-q", "origin", "main")
	out := s.gitdcy(s.root, "rules", "scan", "--format", "json", fleet)
	if rulesLen(rulesAt(rulesJSON(t, out), "data", "checkouts", 0, "pushed_findings")) != 0 {
		t.Errorf("a scan does not read the rules file for its words: %s", out.text())
	}

	s.commit(fleet, "notes.txt", "met jane doe today\n")
	if out := s.gitdcy(fleet, "rules", "check"); out.code != 1 || !strings.Contains(out.text(), "notes.txt:1: a word from the personal list") {
		t.Fatalf("the word in another file is refused: %s", out.text())
	}

	s.ok(fleet, "reset", "-q", "--hard", "HEAD~1")
	token := secretJoin("gh", "p_", secretFill(secretAlnum, 36))
	s.rules("personal = [\"Jane Doe\"]\n# " + token + "\n")
	s.ok(fleet, "commit", "-qam", "a token")
	if out := s.gitdcy(fleet, "rules", "check"); out.code != 1 || !strings.Contains(out.text(), "repos.toml:2: a token or private key (GitHub token") {
		t.Fatalf("a token in the rules file is refused: %s", out.text())
	}
}
