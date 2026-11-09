package main

// The personal-data check on what a push sends.
//
// Every commit that would leave is read: its added lines, its message, and
// its author and committer. Two things are found:
//
//   - consumer mail addresses (gmail, hotmail, outlook, icloud, ...) that are
//     not one of the repository's own git identities or on the owner's allow
//     list;
//   - the owner's own words (names, phone numbers), from the rules file.
//
// The detectors here are generic; the words and the allow list are the
// owner's, read from their private rules file, never from a repository. A
// finding names the commit and the place and shows the value masked, so a
// refusal does not print the data it refuses.

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// personalConsumerMail are mail providers people use privately; a
// repository has no business gaining one of these unless it is the
// committer's own identity.
var personalConsumerMail = []string{
	"gmail.com", "googlemail.com", "hotmail.com", "hotmail.nl", "hotmail.be",
	"outlook.com", "outlook.be", "live.com", "live.nl", "live.be",
	"icloud.com", "me.com", "yahoo.com", "yahoo.nl", "ymail.com",
	"protonmail.com", "proton.me", "pm.me", "gmx.com", "gmx.de", "gmx.net",
	"ziggo.nl", "kpnmail.nl", "hetnet.nl", "planet.nl", "home.nl", "xs4all.nl",
	"upcmail.nl", "telfort.nl", "casema.nl", "quicknet.nl", "telenet.be",
}

// FindingKind is what a finding is.
type FindingKind string

const (
	findingConsumerAddress FindingKind = "consumer-address"
	findingPersonalWord    FindingKind = "personal-word"
)

func (k FindingKind) label() string {
	if k == findingConsumerAddress {
		return "a private mail address"
	}
	return "a word from the personal list"
}

// Finding is personal data in a commit.
type Finding struct {
	// Abbreviated commit hash.
	Commit string `json:"commit"`
	// `file:line`, `message`, `author` or `committer`.
	Place string      `json:"place"`
	Kind  FindingKind `json:"kind"`
	// The value, masked: enough to recognise it, not enough to spread it.
	Shown string `json:"shown"`
}

// personalConfig is the owner's own words and the addresses that belong in
// commits.
type personalConfig struct {
	words []string
	allow []string
}

// personalFound is one thing found on a line: its kind and how it is shown.
type personalFound struct {
	kind  FindingKind
	shown string
}

// personalMask turns a private address into its first letter, `…@` and its
// domain, a word into its first letter and its place in the list.
func personalMask(kind FindingKind, value string, wordIndex int) string {
	first := ""
	for _, r := range value {
		first = string(r)
		break
	}
	if kind == findingConsumerAddress {
		_, domain, _ := rsplitOnce(value, "@")
		return first + "…@" + domain
	}
	return first + "… (word " + strconv.Itoa(wordIndex+1) + " of the personal list)"
}

func personalLocalChar(c byte) bool {
	return isASCIIAlnum(rune(c)) || strings.IndexByte("._%+-", c) >= 0
}

func personalDomainChar(c byte) bool {
	return isASCIIAlnum(rune(c)) || c == '.' || c == '-'
}

// personalAddressAt is the mail address around the `@` at `at` in line,
// when it looks like one. The characters an address takes are ASCII, so
// bytes are characters here.
func personalAddressAt(line string, at int) (string, bool) {
	start := at
	for start > 0 && personalLocalChar(line[start-1]) {
		start--
	}
	end := at + 1
	for end < len(line) && personalDomainChar(line[end]) {
		end++
	}
	if start == at || end == at+1 {
		return "", false
	}
	address := strings.TrimRight(line[start:end], ".")
	return address, strings.Contains(address, "@")
}

// personalIn is everything personal in one line: consumer mail addresses
// not in allowed and the configured words, each once, masked.
func personalIn(line string, words []string, allowed map[string]bool) []personalFound {
	var seen []string
	var out []personalFound
	lower := rustLower(line)
	for at := 0; at < len(lower); at++ {
		if lower[at] != '@' {
			continue
		}
		address, ok := personalAddressAt(lower, at)
		if !ok {
			continue
		}
		_, domain, _ := rsplitOnce(address, "@")
		if contains(personalConsumerMail, domain) && !allowed[address] && !contains(seen, address) {
			seen = append(seen, address)
			out = append(out, personalFound{findingConsumerAddress, personalMask(findingConsumerAddress, address, 0)})
		}
	}
	for index, word := range words {
		word = rustLower(trim(word))
		if word != "" && strings.Contains(lower, word) && !contains(seen, word) {
			out = append(out, personalFound{findingPersonalWord, personalMask(findingPersonalWord, word, index)})
			seen = append(seen, word)
		}
	}
	return out
}

// personalOwnIdentities is the allow list plus the repository's own git
// identities (local and global).
func personalOwnIdentities(repo string, allow []string) map[string]bool {
	set := map[string]bool{}
	for _, a := range allow {
		set[rustLower(trim(a))] = true
	}
	if out, err := gitOutput(repo, "config", "--get-all", "user.email"); err == nil {
		for _, line := range lines(out) {
			set[rustLower(trim(line))] = true
		}
	}
	return set
}

// personalDefaultRange is what to read when no refs are given: the commits
// not yet on the upstream, or, without one, the commits on no remote.
func personalDefaultRange(repo string) []string {
	for _, upstream := range []string{"@{push}", "@{upstream}"} {
		if _, err := gitOutput(repo, "rev-parse", "--verify", "-q", upstream); err == nil {
			return []string{upstream + "..HEAD"}
		}
	}
	return []string{"HEAD", "--not", "--remotes"}
}

// personalCommit is one commit read for personal data.
type personalCommit struct {
	// The full commit id, and the abbreviated one findings name.
	id, short string
	findings  []Finding
}

// personalHeader reports whether a record of personalLog's output starts as
// a commit does: its full id, then the field separator. A diff line or a
// message can hold the record separator too; what follows it is no commit.
func personalHeader(record string) bool {
	id, _, ok := strings.Cut(record, "\x1f")
	return ok && (len(id) == 40 || len(id) == 64) && rulesAllHex(id)
}

// personalLog reads the commits `git log` selects with revs for personal
// data, in git log's order; stdin is what git reads from standard input
// (commit ids, with --stdin), "" for nothing.
func personalLog(repo string, revs []string, stdin string, config personalConfig, allowed map[string]bool) ([]personalCommit, error) {
	args := []string{
		"-c", "core.quotePath=false", "log", "-p", "-U0", "--no-color", "--no-renames",
		"--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/",
		"--format=%x1e%H%x1f%h%x1f%an <%ae>%x1f%cn <%ce>%x1f%B%x1f",
	}
	cmd := gitCommand(repo, append(args, revs...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := runGit(cmd)
	if err != nil {
		return nil, err
	}
	var records []string
	for i, record := range strings.Split(lossy(out.Stdout), "\x1e")[1:] {
		if i > 0 && !personalHeader(record) {
			records[len(records)-1] += "\x1e" + record
			continue
		}
		records = append(records, record)
	}
	commits := make([]personalCommit, 0, len(records))
	for _, record := range records {
		parts := strings.SplitN(record, "\x1f", 6)
		if len(parts) < 6 {
			commits = append(commits, personalCommit{})
			continue
		}
		c := personalCommit{id: parts[0], short: parts[1]}
		diff := parts[5]
		note := func(place, text string) {
			for _, f := range personalIn(text, config.words, allowed) {
				c.findings = append(c.findings, Finding{Commit: c.short, Place: place, Kind: f.kind, Shown: f.shown})
			}
		}
		note("author", parts[2])
		note("committer", parts[3])
		note("message", parts[4])
		path := ""
		lineNo := uint64(0)
		for _, line := range lines(diff) {
			if file, ok := strings.CutPrefix(line, "+++ b/"); ok {
				path = file
			} else if hunk, ok := strings.CutPrefix(line, "@@ "); ok {
				lineNo = 0
				if fields := strings.Fields(hunk); len(fields) > 1 {
					number, _, _ := strings.Cut(strings.TrimLeft(fields[1], "+"), ",")
					if n, err := strconv.ParseUint(number, 10, 64); err == nil {
						lineNo = n
					}
				}
			} else if added, ok := strings.CutPrefix(line, "+"); ok && !strings.HasPrefix(line, "+++") {
				note(path+":"+strconv.FormatUint(lineNo, 10), added)
				lineNo++
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// personalScanCommits reads the commits in rng (git log syntax) for
// personal data; it returns how many commits were read and what was found.
func personalScanCommits(repo string, rng []string, config personalConfig) (int, []Finding, error) {
	commits, err := personalLog(repo, rng, "", config, personalOwnIdentities(repo, config.allow))
	if err != nil {
		return 0, nil, err
	}
	var findings []Finding
	for _, c := range commits {
		findings = append(findings, c.findings...)
	}
	return len(commits), findings, nil
}

// rustLower is Rust's str::to_lowercase: each character's full lowercase
// mapping (İ becomes i and a combining dot), and Σ as ς at the end of a
// word.
func rustLower(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case 'İ':
			b.WriteString("i̇")
		case 'Σ':
			if rustCasedBefore(s[:i]) && !rustCasedAfter(s[i+len("Σ"):]) {
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// rustCaseIgnorable is Unicode's Case_Ignorable property.
func rustCaseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '^', '`', 0x00A8, 0x00AD, 0x00AF, 0x00B4, 0x00B7, 0x00B8, 0x0387, 0x055F, 0x05F4,
		0x2018, 0x2019, 0x2024, 0x2027, 0xFE13, 0xFE52, 0xFE55, 0xFF07, 0xFF0E, 0xFF1A, 0xFF3E, 0xFF40:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// rustCased is Unicode's Cased property.
func rustCased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

func rustCasedBefore(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
		if !rustCaseIgnorable(r) {
			return rustCased(r)
		}
	}
	return false
}

func rustCasedAfter(s string) bool {
	for _, r := range s {
		if !rustCaseIgnorable(r) {
			return rustCased(r)
		}
	}
	return false
}
