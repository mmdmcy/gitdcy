package main

// Tokens and private keys in what a push sends.
//
// The same read as the personal-data check (personal.go): every added line
// and the message of each commit that would leave, and of each commit a scan
// reads. Only prefixed formats a provider documents are looked for, each
// with its exact length or alphabet and a boundary on both sides, so a
// finding is almost always a real credential; there is no entropy rule.
//
// A token is never allowed by value: the allow list is for addresses, and a
// token found is exposed the moment it is pushed. It is rotated at its
// provider, then taken out of the commit. A finding shows the first six
// characters and the length, never more.

import (
	"regexp"
	"strconv"
	"strings"
)

const findingToken FindingKind = "token"

// secretPattern is one provider's credential format.
type secretPattern struct {
	// What it is, as a refusal names it.
	name string
	// Literal parts one of which a match holds: a line with none is not
	// read with the expression.
	hints []string
	re    *regexp.Regexp
	// The part after the prefix is checked for being a placeholder
	// ("xxxx..."); off for a key's header, which is all prefix.
	body bool
}

// secretPatterns are the formats refused. Each has its provider's documented
// prefix; the lengths are the documented or observed ones.
var secretPatterns = []secretPattern{
	// Supabase personal access token (sbp_ and 40 hex) and secret API key.
	{"Supabase access token", []string{"sbp_"}, regexp.MustCompile(`sbp_[0-9a-f]{40}`), true},
	{"Supabase secret key", []string{"sb_secret_"}, regexp.MustCompile(`sb_secret_[A-Za-z0-9_-]{20,}`), true},
	// GitHub: classic, OAuth, user-to-server, server-to-server and refresh
	// tokens (36 characters after the prefix), and fine-grained ones.
	{"GitHub token", []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), true},
	{"GitHub fine-grained token", []string{"github_pat_"}, regexp.MustCompile(`github_pat_[A-Za-z0-9_]{60,}`), true},
	// Anthropic API and admin keys.
	{"Anthropic key", []string{"sk-ant-"}, regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), true},
	// OpenAI project, service account and admin keys, and the older user
	// keys (sk- and 48 letters and digits).
	{"OpenAI key", []string{"sk-proj-", "sk-svcacct-", "sk-admin-"}, regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}`), true},
	{"OpenAI key", []string{"sk-"}, regexp.MustCompile(`sk-[A-Za-z0-9]{48}`), true},
	// DeepSeek: sk- and 32 lowercase hex.
	{"DeepSeek key", []string{"sk-"}, regexp.MustCompile(`sk-[0-9a-f]{32}`), true},
	// AWS access key id, long-term (AKIA) and temporary (ASIA).
	{"AWS access key id", []string{"AKIA", "ASIA"}, regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), true},
	// Google API key: AIza and 35 more.
	{"Google API key", []string{"AIza"}, regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), true},
	// Stripe live secret and restricted keys; test keys are not refused.
	{"Stripe live key", []string{"sk_live_", "rk_live_"}, regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`), true},
	// Slack bot, user, app-level and legacy tokens: xox?- then a number.
	{"Slack token", []string{"xoxa-", "xoxb-", "xoxp-", "xoxr-"}, regexp.MustCompile(`xox[abpr]-[0-9]+-[0-9A-Za-z-]{10,}`), true},
	// Hugging Face user access token: hf_ and 34 letters.
	{"Hugging Face token", []string{"hf_"}, regexp.MustCompile(`hf_[A-Za-z]{34}`), true},
	// A private key in PEM inside a string: its header, \n and a line of
	// the key itself (a JSON service-account file). A key file's header on
	// its own line is secretPEM's, read with the line after it.
	{"private key", []string{"PRIVATE KEY-----"}, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:\\r)?\\n[A-Za-z0-9+/=]{60,}`), false},
}

var (
	secretPEMHeader = regexp.MustCompile(`^\s*-----BEGIN [A-Z ]*PRIVATE KEY-----\s*$`)
	// A line of a key's body: base64, 60 characters or more (PEM writes
	// 64, OpenSSH 70); an example's "MIIE..." is none.
	secretPEMBody = regexp.MustCompile(`^\s*[A-Za-z0-9+/=]{60,}\s*$`)
)

// secretPEM finds a key file in consecutive lines: a header alone on its
// line, then a line of the key's body. A header followed by anything else
// (an example, a placeholder) is no key.
type secretPEM struct {
	header, place string
}

// next reads one line at place; the finding, at the header's place, when
// this line completes a key.
func (w *secretPEM) next(place, line string) (string, personalFound, bool) {
	header, at := w.header, w.place
	w.header, w.place = "", ""
	if header != "" && secretPEMBody.MatchString(line) {
		return at, personalFound{findingToken, "private key: " + secretMask(header)}, true
	}
	if secretPEMHeader.MatchString(line) {
		w.header, w.place = strings.TrimSpace(line), place
	}
	return "", personalFound{}, false
}

// secretChar is a character a token can hold: a match next to one is part
// of a longer word, not a token.
func secretChar(c byte) bool {
	return isASCIIAlnum(rune(c)) || c == '_' || c == '-'
}

// secretPlaceholder reports whether the part of a match after its prefix is
// a stand-in ("xxxxxxxx", "0000...", "EXAMPLE"), as documentation writes one.
func secretPlaceholder(value string) bool {
	body := value
	if i := strings.LastIndexAny(value[:min(len(value), 12)], "_-"); i >= 0 {
		body = value[i+1:]
	}
	if strings.Contains(strings.ToLower(body), "example") {
		return true
	}
	distinct := map[byte]bool{}
	for i := 0; i < len(body); i++ {
		distinct[body[i]] = true
	}
	return len(distinct) < 6
}

// secretMask shows a token as its first six characters, an ellipsis and its
// length: enough to find it at the provider, not enough to use it.
func secretMask(value string) string {
	return value[:min(len(value), 6)] + "… of " + strconv.Itoa(len(value)) + " characters"
}

// secretsIn is every token and private key on one line, each once, masked.
func secretsIn(line string) []personalFound {
	var out []personalFound
	var seen []string
	for _, p := range secretPatterns {
		hinted := false
		for _, hint := range p.hints {
			if strings.Contains(line, hint) {
				hinted = true
				break
			}
		}
		if !hinted {
			continue
		}
		for _, at := range p.re.FindAllStringIndex(line, -1) {
			start, end := at[0], at[1]
			value := strings.TrimSpace(line[start:end])
			if p.body {
				if (start > 0 && secretChar(line[start-1])) || (end < len(line) && secretChar(line[end])) || secretPlaceholder(value) {
					continue
				}
			} else if i := strings.Index(value, "-----BEGIN"); i >= 0 {
				value = value[i:]
				if j := strings.Index(value, "KEY-----"); j >= 0 {
					value = value[:j+len("KEY-----")]
				}
			}
			if contains(seen, value) {
				continue
			}
			seen = append(seen, value)
			out = append(out, personalFound{findingToken, p.name + ": " + secretMask(value)})
		}
	}
	return out
}
