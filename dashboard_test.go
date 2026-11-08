package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestDashKeysAreReadAsTheOldReaderReadThem(t *testing.T) {
	cases := []struct {
		in   string
		want []dashKey
	}{
		{"qrjksS", []dashKey{dashQuit, dashRefresh, dashDown, dashUp, dashSync, dashSyncAll}},
		// Ctrl and a letter, Alt and a letter.
		{"\x11\x13\x0a\x0b", []dashKey{dashQuit, dashSync, dashDown, dashUp}},
		{"\x1bq\x1bS", []dashKey{dashQuit, dashSyncAll}},
		// Arrows in both cursor modes and with a modifier; other keys ignored.
		{"\x1b[A\x1bOB\x1b[1;5A\x1b[C\x1b[3~x\x1b", []dashKey{dashUp, dashDown, dashUp}},
		// Ctrl-C is ignored, as in the Rust build.
		{"x\x03", nil},
	}
	for _, c := range cases {
		if got := dashParseKeys([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("dashParseKeys(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDashColumnsAreWhereRatatuisSolverPutThem(t *testing.T) {
	// Taken from ratatui 0.30's layout for these widths.
	cases := map[int][4][2]int{
		20:  {{0, 5}, {6, 4}, {11, 4}, {16, 4}},
		29:  {{0, 9}, {10, 5}, {16, 6}, {23, 6}},
		91:  {{0, 29}, {30, 17}, {48, 24}, {73, 18}},
		399: {{0, 128}, {129, 72}, {202, 116}, {319, 80}},
		4:   {{0, 0}, {1, 1}, {3, 0}, {4, 0}},
	}
	for width, want := range cases {
		if got := dashColumns(width); got != want {
			t.Errorf("dashColumns(%d) = %v, want %v", width, got, want)
		}
	}
	for _, col := range dashColumns(0) {
		if col[1] != 0 {
			t.Errorf("dashColumns(0) has a column %d wide", col[1])
		}
	}
}

func TestDashViewListsRemotesInOrderAndTheIdentityNote(t *testing.T) {
	path := ".env"
	status := RepoStatus{
		Entry:   RepoEntry{ID: "github/one"},
		Path:    "/w/github/one",
		Remotes: map[string]string{"sync": "/r/s.git", "origin": "/r/o.git", "backup": "/r/b.git"},
		Safety: RepoSafetyReport{Visibility: VisibilityUnknown, PublicTargeted: true, Findings: []SafetyFinding{
			{Fatal: true, Path: &path, Reason: "private environment file is not public source"},
		}},
		Identity: RepoIdentityReport{State: IdentityMismatch, Message: "expected A <a@x>"},
	}
	v := dashViewOf([]RepoStatus{status}, 0, dashHelp)
	var got []string
	for _, line := range v.detail {
		var b strings.Builder
		for _, r := range line {
			b.WriteString(r.text)
		}
		got = append(got, b.String())
	}
	want := []string{
		"id: github/one", "path: /w/github/one", "branch: -", "tracking: -", "visibility: unknown",
		"state: clean, 1 safety block, identity-mismatch", "identity: identity-mismatch",
		"identity note: expected A <a@x>",
		"remotes:", "  backup: /r/b.git", "  origin: /r/o.git", "  sync: /r/s.git",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detail = %q, want %q", got, want)
	}
	wantFindings := []string{"[identity] expected A <a@x>", "[fatal] .env: private environment file is not public source"}
	if !reflect.DeepEqual(v.findings, wantFindings) {
		t.Errorf("findings = %q, want %q", v.findings, wantFindings)
	}
	if empty := dashViewOf(nil, 0, ""); empty.selected != -1 || len(empty.findings) != 0 || empty.detail[0][0].text != "No repositories discovered." {
		t.Errorf("an empty workspace's view = %+v", empty)
	}
}

func dashRow(s *dashScreen, y int) string {
	var b strings.Builder
	for x := 0; x < s.width; x++ {
		b.WriteString(s.cells[y*s.width+x].symbol)
	}
	return b.String()
}

func TestDashFrameHasThePanesAndDropsControlCharacters(t *testing.T) {
	v := dashView{
		rows: [][4]string{
			{"one", "main", "clean", "ok"},
			{"two\x1b[2J", "-", "dirty", "ok"},
		},
		selected: 1,
		detail:   [][]dashRun{{{"id: ", true}, {"two", false}}},
		findings: []string{"No safety or identity findings."},
		log:      "refreshed",
	}
	// The frame ratatui 0.30 drew for this view (compared cell by cell with
	// it at every size up to 130 by 40 when the Rust build replaced it), with
	// the footer three rows high since.
	s := dashDraw(v, 60, 24)
	want := map[int]string{
		0:  "┌GitDCY Dashboard─────────────────┐┌Selected Repo──────────┐",
		1:  "│repo        branch state  safety ││id: two                │",
		2:  "│one         main   clean  ok     ││                       │",
		3:  "│two[2J      -      dirty  ok     ││                       │",
		11: "└" + strings.Repeat("─", 33) + "┘└" + strings.Repeat("─", 23) + "┘",
		12: "┌Findings" + strings.Repeat("─", 50) + "┐",
		13: "│No safety or identity findings." + strings.Repeat(" ", 27) + "│",
		20: "└" + strings.Repeat("─", 58) + "┘",
		// The footer: its border and the log line.
		21: "┌" + strings.Repeat("─", 58) + "┐",
		22: "│refreshed" + strings.Repeat(" ", 49) + "│",
		23: "└" + strings.Repeat("─", 58) + "┘",
	}
	for y, line := range want {
		if got := dashRow(s, y); got != line {
			t.Errorf("row %d = %q, want %q", y, got, line)
		}
	}
	// Every size draws, the smallest included.
	for height := 0; height <= 40; height++ {
		for width := 0; width <= 80; width++ {
			dashDraw(v, width, height)
		}
	}
	at := func(x, y int) dashStyle { return s.cells[y*60+x].style }
	if st := at(1, 1); !st.yellow || !st.bold {
		t.Errorf("header cell style = %+v, want yellow and bold", st)
	}
	if !at(1, 3).reversed || at(1, 2).reversed {
		t.Errorf("only the selected row is reversed")
	}
	if !at(36, 1).bold || at(40, 1).bold {
		t.Errorf("only the field name is bold")
	}
	if bytes.Contains(s.ansi(false), []byte("\x1b[2J")) {
		t.Errorf("a repository name's escape code reached the frame")
	}
}
