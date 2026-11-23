package main

// The personal-data check on what a push sends.
//
// Every commit that would leave is read: its added lines, its message, and
// its author and committer. These are found:
//
//   - consumer mail addresses (gmail, hotmail, outlook, icloud, ...) that are
//     not one of the repository's own git identities or on the owner's allow
//     list;
//   - the owner's own words (names, phone numbers), from the rules file, in
//     the added lines, the message, the author and committer, and in the
//     bytes of every binary file a commit adds or changes (binary.go);
//   - the rules file's message words (an AI tool's credit line), in the
//     message alone: a file may quote another project's commit;
//   - tokens and private keys of the formats in secrets.go, never allowed;
//   - an author or committer the rules file's identities do not list, when
//     it lists any (the push check only: a scan of what already reached a
//     remote reads other people's commits too).
//
// The detectors here are generic; the words and the allow list are the
// owner's, read from their private rules file, never from a repository. A
// finding names the commit and the place and shows the value masked, so a
// refusal does not print the data it refuses.

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	findingIdentity        FindingKind = "identity"
	findingMessageWord     FindingKind = "message-word"
	findingBinaryWord      FindingKind = "binary-personal-word"
)

func (k FindingKind) label() string {
	switch k {
	case findingConsumerAddress:
		return "a private mail address"
	case findingIdentity:
		return "an identity the rules do not allow"
	case findingToken:
		return "a token or private key"
	case findingMessageWord:
		return "a word the rules keep out of commit messages"
	case findingBinaryWord:
		return "a word from the personal list (in a binary file)"
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
	// Words that never go in a commit message (read in the message alone).
	messageWords []string
	// Who may author and commit; none: no identity check.
	identities []string
	// The rules file in use, as a path in the repository being read
	// ("/"-separated); "" when it is outside it. Its lines are not read for
	// the personal words, which it lists itself: the commit that adds a
	// word would be refused for that word.
	rulesFile string
}

// personalRulesFile is the rules file at rulesPath as a path inside the
// repository at root, both resolved (links followed, absolute); in another
// checkout of the rules file's own repository (a worktree of it, or a clone
// with the same origin), the same path there; "" else. Copies of the rules
// repository list the same words: they were flagged for them while the
// checkout the rules are read from was not.
func personalRulesFile(rulesPath, root string) string {
	if rulesPath == "" || root == "" {
		return ""
	}
	rulesPath, root = personalResolve(rulesPath), personalResolve(root)
	if rel, ok := personalInside(root, rulesPath); ok {
		return rel
	}
	home := personalRulesHome(rulesPath)
	if home.rel == "" {
		return ""
	}
	if common, err := gitOutput(root, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil &&
		home.commonDir != "" && personalResolve(rulesAbsolute(root, common)) == home.commonDir {
		return home.rel
	}
	if origin, err := gitOutput(root, "remote", "get-url", "origin"); err == nil &&
		home.origin != "" && trim(origin) == home.origin {
		return home.rel
	}
	return ""
}

// personalResolve is path absolute, links followed.
func personalResolve(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return path
}

// personalInside is path as a "/"-separated path inside root; false when
// it is not inside it.
func personalInside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// personalRulesRepo is the repository holding the rules file: the file's
// place in it, its git folder shared by its worktrees, and its origin.
type personalRulesRepo struct{ rel, commonDir, origin string }

// personalRulesRepos keeps each rules file's repository, read once per run
// (a scan asks for every checkout).
var personalRulesRepos sync.Map

// personalRulesHome is the repository of the rules file at rulesPath
// (resolved); rel is "" when the file is in none.
func personalRulesHome(rulesPath string) personalRulesRepo {
	if kept, ok := personalRulesRepos.Load(rulesPath); ok {
		return kept.(personalRulesRepo)
	}
	var home personalRulesRepo
	dir := filepath.Dir(rulesPath)
	if top, err := gitOutput(dir, "rev-parse", "--show-toplevel"); err == nil && trim(top) != "" {
		if rel, ok := personalInside(personalResolve(trim(top)), rulesPath); ok {
			home.rel = rel
			if common, err := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
				home.commonDir = personalResolve(rulesAbsolute(dir, common))
			}
			if origin, err := gitOutput(dir, "remote", "get-url", "origin"); err == nil {
				home.origin = trim(origin)
			}
		}
	}
	personalRulesRepos.Store(rulesPath, home)
	return home
}

// identityParts splits "Name <address>" (or an address alone, with or
// without <>) into its name and its address.
func identityParts(text string) (name, address string) {
	text = trim(text)
	if open := strings.LastIndex(text, "<"); open >= 0 && strings.HasSuffix(text, ">") {
		return trim(text[:open]), trim(text[open+1 : len(text)-1])
	}
	return "", text
}

// identitiesValid refuses an entry with no address: it would allow nobody.
func identitiesValid(where string, list []string) error {
	for _, entry := range list {
		if _, address := identityParts(entry); address == "" {
			return errf("%s: \"%s\" has no address; write \"Name <address>\", or the address alone for any name", where, entry)
		}
	}
	return nil
}

// identityAllowed reports whether the identity git shows ("Name <address>")
// is on the list: its address (any case) and, where the entry names one,
// its name exactly.
func identityAllowed(identity string, list []string) bool {
	name, address := identityParts(identity)
	for _, entry := range list {
		n, a := identityParts(entry)
		if strings.EqualFold(a, address) && (n == "" || n == name) {
			return true
		}
	}
	return false
}

// identityShown is the identity as a refusal names it: as git has it, its
// address masked when it is a private one.
func identityShown(identity string) string {
	name, address := identityParts(identity)
	for _, f := range personalIn(address, nil, map[string]bool{}) {
		if f.kind == findingConsumerAddress {
			return name + " <" + f.shown + ">"
		}
	}
	return trim(identity)
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
// not in allowed, the configured words and tokens, each once, masked.
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
	// Tokens are never on the allow list: one found is always refused.
	return append(out, secretsIn(line)...)
}

// messageWordsIn is each message word the message holds, in any case, as
// the rules file writes it: a message word is policy, not personal data,
// so it is shown.
func messageWordsIn(message string, words []string) []string {
	lower := rustLower(message)
	var out []string
	for _, word := range words {
		word = trim(word)
		if word != "" && strings.Contains(lower, rustLower(word)) && !contains(out, word) {
			out = append(out, word)
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
		"-c", "core.quotePath=false", "log", "-p", "-U0", "--no-color", "--no-renames", "--full-index",
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
	// Binary files whose new content is read for the personal words, once
	// every commit is parsed.
	var binaries []binaryBlob
	for _, record := range records {
		parts := strings.SplitN(record, "\x1f", 6)
		if len(parts) < 6 {
			commits = append(commits, personalCommit{})
			continue
		}
		c := personalCommit{id: parts[0], short: parts[1]}
		diff := parts[5]
		// The file the diff is at; "" for the author, committer and message.
		path := ""
		note := func(place, text string) {
			words := config.words
			if path != "" && path == config.rulesFile {
				words = nil
			}
			for _, f := range personalIn(text, words, allowed) {
				c.findings = append(c.findings, Finding{Commit: c.short, Place: place, Kind: f.kind, Shown: f.shown})
			}
		}
		note("author", parts[2])
		note("committer", parts[3])
		if len(config.identities) > 0 {
			for i, place := range []string{"author", "committer"} {
				if !identityAllowed(parts[2+i], config.identities) {
					c.findings = append(c.findings, Finding{Commit: c.short, Place: place, Kind: findingIdentity, Shown: identityShown(parts[2+i])})
				}
			}
		}
		note("message", parts[4])
		for _, word := range messageWordsIn(parts[4], config.messageWords) {
			c.findings = append(c.findings, Finding{Commit: c.short, Place: "message", Kind: findingMessageWord, Shown: word})
		}
		// A key file's header and the line after it.
		var pem secretPEM
		key := func(place, line string) {
			if at, f, ok := pem.next(place, line); ok {
				c.findings = append(c.findings, Finding{Commit: c.short, Place: at, Kind: f.kind, Shown: f.shown})
			}
		}
		for _, line := range lines(parts[4]) {
			key("message", line)
		}
		pem = secretPEM{}
		lineNo := uint64(0)
		// The file the diff header names and its new blob, for a binary one.
		header, blob := "", ""
		for _, line := range lines(diff) {
			if names, ok := strings.CutPrefix(line, "diff --git "); ok {
				header, blob = binaryDiffPath(names), ""
			} else if index, ok := strings.CutPrefix(line, "index "); ok && header != "" {
				if fields := strings.Fields(index); len(fields) > 0 {
					_, blob, _ = strings.Cut(fields[0], "..")
				}
			} else if strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
				// A deleted file's new blob is all zeros: nothing is added.
				if blob != "" && !rulesIsZero(blob) && header != config.rulesFile && len(config.words) > 0 {
					binaries = append(binaries, binaryBlob{commit: len(commits), path: header, id: blob})
				}
				header, blob = "", ""
			} else if file, ok := strings.CutPrefix(line, "+++ b/"); ok {
				path = file
				pem = secretPEM{}
			} else if hunk, ok := strings.CutPrefix(line, "@@ "); ok {
				pem = secretPEM{}
				lineNo = 0
				if fields := strings.Fields(hunk); len(fields) > 1 {
					number, _, _ := strings.Cut(strings.TrimLeft(fields[1], "+"), ",")
					if n, err := strconv.ParseUint(number, 10, 64); err == nil {
						lineNo = n
					}
				}
			} else if added, ok := strings.CutPrefix(line, "+"); ok && !strings.HasPrefix(line, "+++") {
				place := path + ":" + strconv.FormatUint(lineNo, 10)
				note(place, added)
				key(place, added)
				lineNo++
			}
		}
		commits = append(commits, c)
	}
	if len(binaries) > 0 {
		found, err := binaryWords(repo, binaries, config.words)
		if err != nil {
			return nil, err
		}
		for i, b := range binaries {
			c := &commits[b.commit]
			for _, index := range found[i] {
				shown := personalMask(findingPersonalWord, rustLower(trim(config.words[index])), index)
				c.findings = append(c.findings, Finding{Commit: c.short, Place: b.path, Kind: findingBinaryWord, Shown: shown})
			}
		}
	}
	return commits, nil
}

// personalScanCommits reads the commits in rng (git log syntax) for
// personal data; it returns how many commits were read and what was found.
func personalScanCommits(repo string, rng []string, config personalConfig) (int, []Finding, error) {
	// An identity allowed to author is no personal data in a commit either.
	allow := append([]string{}, config.allow...)
	for _, entry := range config.identities {
		_, address := identityParts(entry)
		allow = append(allow, address)
	}
	commits, err := personalLog(repo, rng, "", config, personalOwnIdentities(repo, allow))
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
