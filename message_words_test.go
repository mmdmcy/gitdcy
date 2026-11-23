//go:build unix

package main

// Message words (read in the commit message alone) and the personal words
// in binary files. Real repositories in a sandbox with its own HOME.

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

const messageTrailer = "Co-Authored-By: Example Bot <bot@example.com>"

func TestRulesCmdAMessageWordIsRefusedInTheMessageAndPassesInAFile(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("message_words = [\"co-authored-by: example bot\"]\n")

	// In a file (an archived page quoting another project): no finding.
	s.commit(work, "archive.html", "<pre>"+messageTrailer+"</pre>\n")
	if out := s.gitdcy(work, "rules", "check"); out.code != 0 {
		t.Fatalf("the words in a file pass: %s", out.text())
	}

	s.ok(work, "commit", "-q", "--allow-empty", "-m", "fix\n\n"+strings.ToUpper(messageTrailer))
	short := s.ok(work, "rev-parse", "--short", "HEAD")
	out := s.gitdcy(work, "rules", "check")
	said := out.text()
	if out.code != 1 || !strings.Contains(said, "commit "+short+" message: a word the rules keep out of commit messages (co-authored-by: example bot)") {
		t.Fatal(said)
	}
	if !strings.Contains(said, "git commit --amend") || !strings.Contains(said, "git rebase -i "+short+"^") || !strings.Contains(said, "reword") ||
		!strings.Contains(said, "message_words") {
		t.Errorf("the refusal gives the fix: %s", said)
	}
	v := rulesJSON(t, s.gitdcy(work, "rules", "check", "--format", "json"))
	if n := rulesLen(rulesAt(v, "data", "findings")); n != 1 || rulesAt(v, "data", "findings", 0, "kind") != "message-word" {
		t.Fatalf("one finding, of its own kind: %v", v)
	}

	// The fix it gives works.
	s.ok(work, "commit", "-q", "--amend", "--allow-empty", "-m", "fix")
	if out := s.gitdcy(work, "rules", "check"); out.code != 0 {
		t.Fatalf("after the reword the push passes: %s", out.text())
	}

	// An empty word is refused: it would match every message.
	s.rules("message_words = [\" \"]\n")
	if out := s.gitdcy(work, "rules", "check"); out.code != 1 || !strings.Contains(out.text(), "message_words") {
		t.Fatalf("an empty word is an error: %s", out.text())
	}
}

func TestRulesCmdAScanReportsAMessageWordThatReachedARemote(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("message_words = [\"co-authored-by: example bot\"]\n")
	s.ok(work, "commit", "-q", "--allow-empty", "-m", "fix\n\n"+messageTrailer)
	s.ok(work, "push", "-q", "origin", "main")
	out := s.gitdcy(s.root, "rules", "scan", "--format", "json", work)
	v := rulesJSON(t, out)
	app := rulesAt(v, "data", "checkouts", 0)
	if out.code != 0 || rulesLen(rulesAt(app, "pushed_findings")) != 1 || rulesAt(app, "pushed_findings", 0, "kind") != "message-word" ||
		rulesAt(v, "data", "message_words") != float64(1) {
		t.Fatal(out.text())
	}
	said := s.gitdcy(s.root, "rules", "scan", work).text()
	if !strings.Contains(said, "message: a word the rules keep out of commit messages (co-authored-by: example bot)") {
		t.Error(said)
	}
}

// messageUTF16 is text as UTF-16LE bytes.
func messageUTF16(text string) []byte {
	var b bytes.Buffer
	for _, u := range utf16.Encode([]rune(text)) {
		_ = binary.Write(&b, binary.LittleEndian, u)
	}
	return b.Bytes()
}

func TestRulesCmdAPersonalWordInABinaryFileIsRefusedPlainOrUTF16(t *testing.T) {
	s := newRulesSandbox(t)
	work, _ := s.repo("app")
	s.rules("personal = [\"Janedoe\"]\n")
	head := []byte("MZ\x00\x01\x02\x00")

	// Without the word: passes.
	s.commit(work, "app.dll", string(append(head, []byte(`C:\Users\someone\build`)...)))
	if out := s.gitdcy(work, "rules", "check"); out.code != 0 {
		t.Fatalf("a binary file without the word passes: %s", out.text())
	}

	for name, content := range map[string][]byte{
		"plain": append(append([]byte{}, head...), []byte(`C:\Users\JaneDoe\build\x.pdb`)...),
		"utf16": append(append([]byte{}, head...), messageUTF16(`C:\Users\JANEDOE\AppData`)...),
	} {
		s.commit(work, "app.dll", string(content))
		short := s.ok(work, "rev-parse", "--short", "HEAD")
		out := s.gitdcy(work, "rules", "check")
		said := out.text()
		if out.code != 1 || !strings.Contains(said, "commit "+short+" app.dll: a word from the personal list (in a binary file) (j… (word 1 of the personal list))") ||
			!strings.Contains(said, "personal data") {
			t.Fatalf("%s: %s", name, said)
		}
		if strings.Contains(strings.ToLower(said), "janedoe") {
			t.Errorf("%s: the word is shown: %s", name, said)
		}
		s.ok(work, "reset", "-q", "--hard", "HEAD~1")
	}

	// A deleted binary file adds nothing.
	s.ok(work, "push", "-q", "origin", "main")
	s.ok(work, "branch", "-q", "--set-upstream-to=origin/main")
	s.ok(work, "rm", "-q", "app.dll")
	s.ok(work, "commit", "-qm", "gone")
	if out := s.gitdcy(work, "rules", "check"); out.code != 0 {
		t.Fatalf("a deletion passes: %s", out.text())
	}
}

func TestBinaryAWordAcrossTwoChunksIsFound(t *testing.T) {
	word := "Janedoe"
	data := bytes.Repeat([]byte{0}, binaryChunk-3)
	data = append(data, messageUTF16(word)...)
	found, err := binaryFind(bytes.NewReader(data), binaryPatterns([]string{"other", word}))
	if err != nil || len(found) != 1 || found[0] != 1 {
		t.Fatalf("found %v, %v", found, err)
	}
	if path := binaryDiffPath("a/dir with b/x.dll b/dir with b/x.dll"); path != "dir with b/x.dll" {
		t.Errorf("path %q", path)
	}
}
