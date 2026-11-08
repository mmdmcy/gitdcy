package main

// `gitdcy dashboard`: every repository, the selected one's details and its
// findings in the terminal. Drawn with plain ANSI escape codes on the
// alternate screen, the keyboard read in raw mode (termios on Unix, the
// console API on Windows; term_*.go), so no terminal library is compiled
// in. It draws what the Rust build drew, frame for frame: the same panes
// and sizes, colours and keys, and the footer of three rows (its border
// and the log line).

import (
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const dashHelp = "q quit | r refresh | j/k or arrows move | s sync selected | S sync all safe"

// dashOut is the terminal while the dashboard holds it. Frames and the
// restore share one lock, so a signal never restores the terminal in the
// middle of a frame.
type dashOut struct {
	mu       sync.Mutex
	tty      *termTTY
	restored bool
}

// write writes one frame to standard output.
func (o *dashOut) write(frame []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := os.Stdout.Write(frame)
	return err
}

// restore gives the terminal back: cooked mode, the main screen and the
// cursor. It runs once, whichever of quit, an error, a panic or a signal
// comes first.
func (o *dashOut) restore() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.restoreLocked()
}

func (o *dashOut) restoreLocked() {
	if o.restored {
		return
	}
	o.restored = true
	_ = o.tty.restore()
	_, _ = os.Stdout.Write([]byte("\x1b[?1049l\x1b[?25h"))
	o.tty.close()
}

func runDashboard() error {
	tty, err := termOpen()
	if err != nil {
		return wrap(err, "enable raw mode")
	}
	// The alternate screen, so the shell's own screen comes back on quit.
	if _, err := os.Stdout.Write([]byte("\x1b[?1049h")); err != nil {
		_ = tty.restore()
		tty.close()
		return wrap(err, "enter alternate screen")
	}
	out := &dashOut{tty: tty}
	// Raw mode turns Ctrl-C into an ignored key, as in the Rust build; a
	// signal sent from elsewhere gives the terminal back before the process
	// ends (the Rust build left it in raw mode).
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		out.restore() // also when the loop panics
		signal.Stop(signals)
	}()
	go func() {
		sig := <-signals
		out.mu.Lock()
		out.restoreLocked()
		code := 1
		if n, isNum := sig.(syscall.Signal); isNum {
			code = 128 + int(n)
		}
		os.Exit(code)
	}()

	if _, _, err := tty.size(); err != nil {
		return wrap(err, "create terminal")
	}
	return dashLoop(tty, out)
}

type dashKey int

const (
	dashQuit dashKey = iota
	dashRefresh
	dashDown
	dashUp
	dashSync
	dashSyncAll
)

func dashLoop(tty *termTTY, out *dashOut) error {
	manifest, err := loadOrDiscoverManifest()
	if err != nil {
		return err
	}
	statuses := statusAll(manifest)
	selected := 0
	log := dashHelp
	var shown *dashScreen
	var keys []dashKey

	for {
		if selected >= len(statuses) {
			selected = max(len(statuses)-1, 0)
		}
		width, height, err := tty.size()
		if err != nil {
			return ioError(err)
		}
		screen := dashDraw(dashViewOf(statuses, selected, log), width, height)
		if shown == nil || !shown.equal(screen) {
			resized := shown == nil || shown.width != width || shown.height != height
			if err := out.write(screen.ansi(resized)); err != nil {
				return ioError(err)
			}
			shown = screen
		}

		if len(keys) == 0 {
			got, err := tty.keys(250 * time.Millisecond)
			if err != nil {
				return ioError(err)
			}
			keys = append(keys, got...)
		}
		if len(keys) == 0 {
			continue
		}
		key := keys[0]
		keys = keys[1:]
		switch key {
		case dashQuit:
			return nil
		case dashRefresh:
			if manifest, err = loadOrDiscoverManifest(); err != nil {
				return err
			}
			statuses = statusAll(manifest)
			log = "refreshed"
		case dashDown:
			if selected+1 < len(statuses) {
				selected++
			}
		case dashUp:
			selected = max(selected-1, 0)
		case dashSync:
			if selected < len(statuses) {
				status := statuses[selected]
				if status.Safety.hasFatalFindings() {
					log = "sync blocked for " + status.Entry.ID + ": safety findings"
				} else {
					report := syncRepo(status.Entry)
					if report.Blocked != nil {
						log = "sync blocked for " + status.Entry.ID + ": " + *report.Blocked
					} else {
						log = "synced " + status.Entry.ID
					}
					statuses = statusAll(manifest)
				}
			}
		case dashSyncAll:
			synced, blocked := 0, 0
			for _, status := range statuses {
				if status.Safety.hasFatalFindings() {
					blocked++
					continue
				}
				if syncRepo(status.Entry).Blocked != nil {
					blocked++
				} else {
					synced++
				}
			}
			statuses = statusAll(manifest)
			log = "sync all safe: " + strconv.Itoa(synced) + " synced, " + strconv.Itoa(blocked) + " blocked/skipped"
		}
	}
}

// dashParseKeys is the keys in what the terminal sent, as the Rust build
// took them: a letter also with Ctrl or Alt held (Ctrl+S syncs), the arrows
// in either cursor mode and with modifiers; everything else (Ctrl-C too,
// which raw mode delivers as a byte) is ignored.
func dashParseKeys(b []byte) []dashKey {
	var keys []dashKey
	letter := func(c byte) {
		switch c {
		case 'q', 0x11:
			keys = append(keys, dashQuit)
		case 'r', 0x12:
			keys = append(keys, dashRefresh)
		case 'j', 0x0a:
			keys = append(keys, dashDown)
		case 'k', 0x0b:
			keys = append(keys, dashUp)
		case 's', 0x13:
			keys = append(keys, dashSync)
		case 'S':
			keys = append(keys, dashSyncAll)
		}
	}
	arrow := func(c byte) {
		switch c {
		case 'A':
			keys = append(keys, dashUp)
		case 'B':
			keys = append(keys, dashDown)
		}
	}
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			letter(b[i])
			i++
			continue
		}
		switch {
		case i+1 >= len(b):
			i++
		case b[i+1] == '[':
			// CSI: parameters, then one final byte; ESC [ A is Up.
			end := i + 2
			for end < len(b) && !(b[end] >= 0x40 && b[end] <= 0x7e) {
				end++
			}
			if end < len(b) {
				arrow(b[end])
			}
			i = end + 1
		case b[i+1] == 'O' && i+2 < len(b):
			// SS3, the application cursor mode: ESC O A is Up.
			arrow(b[i+2])
			i += 3
		default:
			// Alt and a key.
			letter(b[i+1])
			i += 2
		}
	}
	return keys
}

// dashRun is a run of text in a line, bold or not.
type dashRun struct {
	text string
	bold bool
}

// dashView is what one frame shows.
type dashView struct {
	rows     [][4]string
	selected int // -1 when there are no rows
	detail   [][]dashRun
	findings []string
	log      string
}

func dashViewOf(statuses []RepoStatus, selected int, log string) dashView {
	v := dashView{selected: -1, log: log}
	for _, s := range statuses {
		branch := "-"
		if s.Branch != nil {
			branch = *s.Branch
		}
		v.rows = append(v.rows, [4]string{s.Entry.ID, branch, s.shortState(), s.Safety.shortState()})
	}
	if len(statuses) > 0 {
		v.selected = selected
	}
	if selected < len(statuses) {
		v.detail = dashDetailLines(statuses[selected])
		v.findings = dashFindingItems(statuses[selected])
	} else {
		v.detail = [][]dashRun{{{"No repositories discovered.", false}}}
	}
	return v
}

// dashIdentityNoted reports whether the identity check has something to
// say: not matched and not switched off.
func dashIdentityNoted(s RepoStatus) bool {
	label := s.Identity.stateLabel()
	return label != "identity-ok" && label != "identity-unchecked"
}

func dashDetailLines(s RepoStatus) [][]dashRun {
	field := func(name, value string) []dashRun {
		return []dashRun{{name + ": ", true}, {value, false}}
	}
	orDash := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	lines := [][]dashRun{
		field("id", s.Entry.ID),
		field("path", s.Path),
		field("branch", orDash(s.Branch)),
		field("tracking", orDash(s.TrackingBranch)),
		field("visibility", s.Safety.Visibility.label()),
		field("state", s.shortState()),
		field("identity", s.Identity.shortState()),
	}
	plain := func(text string) []dashRun { return []dashRun{{text, false}} }
	if dashIdentityNoted(s) {
		lines = append(lines, plain("identity note: "+s.Identity.Message))
	}
	if len(s.Remotes) == 0 {
		lines = append(lines, plain("remotes: -"))
	} else {
		lines = append(lines, plain("remotes:"))
		names := make([]string, 0, len(s.Remotes))
		for name := range s.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			lines = append(lines, plain("  "+name+": "+s.Remotes[name]))
		}
	}
	return lines
}

func dashFindingItems(s RepoStatus) []string {
	var items []string
	if dashIdentityNoted(s) {
		items = append(items, "[identity] "+s.Identity.Message)
	}
	for _, f := range s.Safety.Findings {
		path := "-"
		if f.Path != nil {
			path = *f.Path
		}
		items = append(items, "["+f.severityLabel()+"] "+path+": "+f.Reason)
	}
	if len(items) == 0 {
		items = append(items, "No safety or identity findings.")
	}
	return items
}

// dashFirstLine is Rust's text.lines().next().unwrap_or("").
func dashFirstLine(text string) string {
	if l := lines(text); len(l) > 0 {
		return l[0]
	}
	return ""
}

type dashStyle struct {
	yellow, bold, reversed bool
}

var dashBold = dashStyle{bold: true}

type dashCell struct {
	// What the cell shows; empty when a wide character to its left covers it.
	symbol string
	style  dashStyle
}

type dashArea struct {
	x, y, width, height int
}

// inner is the area inside a border all around.
func (a dashArea) inner() dashArea {
	return dashArea{
		x:      min(a.x+1, a.x+a.width),
		y:      min(a.y+1, a.y+a.height),
		width:  max(a.width-2, 0),
		height: max(a.height-2, 0),
	}
}

type dashScreen struct {
	width, height int
	cells         []dashCell
}

func newDashScreen(width, height int) *dashScreen {
	cells := make([]dashCell, width*height)
	for i := range cells {
		cells[i].symbol = " "
	}
	return &dashScreen{width: width, height: height, cells: cells}
}

func (s *dashScreen) equal(o *dashScreen) bool {
	if s.width != o.width || s.height != o.height || len(s.cells) != len(o.cells) {
		return false
	}
	for i := range s.cells {
		if s.cells[i] != o.cells[i] {
			return false
		}
	}
	return true
}

func (s *dashScreen) cell(x, y int) *dashCell {
	if x >= 0 && y >= 0 && x < s.width && y < s.height {
		return &s.cells[y*s.width+x]
	}
	return nil
}

func (s *dashScreen) put(x, y int, symbol string) {
	if c := s.cell(x, y); c != nil {
		c.symbol = symbol
	}
}

func (s *dashScreen) style(a dashArea, st dashStyle) {
	for y := a.y; y < a.y+a.height; y++ {
		for x := a.x; x < a.x+a.width; x++ {
			if c := s.cell(x, y); c != nil {
				c.style.yellow = c.style.yellow || st.yellow
				c.style.bold = c.style.bold || st.bold
				c.style.reversed = c.style.reversed || st.reversed
			}
		}
	}
}

// text writes text from (x, y), at most width columns, and returns the
// columns used. Control characters are dropped: a repository name or a
// message can never move the cursor or change the terminal. Bytes that are
// not UTF-8 show as U+FFFD, as Rust's lossy conversion showed them.
func (s *dashScreen) text(x, y, width int, text string, st dashStyle) int {
	used, last := 0, -1
	for _, c := range lossy([]byte(text)) {
		if unicode.IsControl(c) {
			continue
		}
		w := dashCharWidth(c)
		if w == 0 {
			if last >= 0 {
				if cell := s.cell(last, y); cell != nil {
					cell.symbol += string(c)
				}
			}
			continue
		}
		if used+w > width {
			break
		}
		at := x + used
		s.put(at, y, string(c))
		s.style(dashArea{at, y, 1, 1}, st)
		if w == 2 {
			s.put(at+1, y, "")
		}
		last = at
		used += w
	}
	return used
}

// line writes one line of runs, left-aligned, cut at the area's width.
func (s *dashScreen) line(a dashArea, y int, runs []dashRun) {
	x := a.x
	for _, r := range runs {
		left := a.width - (x - a.x)
		if left <= 0 {
			break
		}
		st := dashStyle{}
		if r.bold {
			st = dashBold
		}
		x += s.text(x, y, left, r.text, st)
	}
}

// block draws a border all around with title on its top edge and returns
// the inside.
func (s *dashScreen) block(a dashArea, title string) dashArea {
	if a.width == 0 || a.height == 0 {
		return a.inner()
	}
	left, top := a.x, a.y
	right, bottom := a.x+a.width-1, a.y+a.height-1
	for y := top; y <= bottom; y++ {
		s.put(left, y, "│")
	}
	for x := left; x <= right; x++ {
		s.put(x, top, "─")
	}
	for y := top; y <= bottom; y++ {
		s.put(right, y, "│")
	}
	for x := left; x <= right; x++ {
		s.put(x, bottom, "─")
	}
	s.put(right, bottom, "┘")
	s.put(right, top, "┐")
	s.put(left, bottom, "└")
	s.put(left, top, "┌")
	s.text(left+1, top, max(a.width-2, 0), title, dashStyle{})
	return a.inner()
}

// ansi is the frame as escape codes: every row from its first column, a
// style change only where the style changes. clear wipes the screen first,
// after a resize.
func (s *dashScreen) ansi(clear bool) []byte {
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	if clear {
		b.WriteString("\x1b[2J")
	}
	for y := 0; y < s.height; y++ {
		b.WriteString("\x1b[" + strconv.Itoa(y+1) + ";1H")
		style := dashStyle{}
		b.WriteString("\x1b[0m")
		for x := 0; x < s.width; x++ {
			c := s.cells[y*s.width+x]
			if c.style != style {
				style = c.style
				b.WriteString("\x1b[0")
				if style.bold {
					b.WriteString(";1")
				}
				if style.reversed {
					b.WriteString(";7")
				}
				if style.yellow {
					b.WriteString(";33")
				}
				b.WriteByte('m')
			}
			b.WriteString(c.symbol)
		}
	}
	b.WriteString("\x1b[0m")
	return []byte(b.String())
}

// dashCharWidth is the columns a character takes in a terminal: 0 for
// combining marks and other zero-width characters, 2 for East Asian wide
// characters and emoji, else 1.
func dashCharWidth(c rune) int {
	switch {
	case c >= 0x0300 && c <= 0x036f,
		c >= 0x0483 && c <= 0x0489,
		c >= 0x0591 && c <= 0x05bd,
		c >= 0x0610 && c <= 0x061a,
		c >= 0x064b && c <= 0x065f,
		c >= 0x200b && c <= 0x200f,
		c >= 0x20d0 && c <= 0x20ff,
		c >= 0xfe00 && c <= 0xfe0f,
		c >= 0xfe20 && c <= 0xfe2f,
		c >= 0xe0100 && c <= 0xe01ef:
		return 0
	case c >= 0x1100 && c <= 0x115f,
		c >= 0x2e80 && c <= 0x303e,
		c >= 0x3041 && c <= 0x33ff,
		c >= 0x3400 && c <= 0x4dbf,
		c >= 0x4e00 && c <= 0x9fff,
		c >= 0xa000 && c <= 0xa4cf,
		c >= 0xac00 && c <= 0xd7a3,
		c >= 0xf900 && c <= 0xfaff,
		c >= 0xfe30 && c <= 0xfe4f,
		c >= 0xff00 && c <= 0xff60,
		c >= 0xffe0 && c <= 0xffe6,
		c >= 0x1f300 && c <= 0x1f64f,
		c >= 0x1f900 && c <= 0x1f9ff,
		c >= 0x20000 && c <= 0x2fffd,
		c >= 0x30000 && c <= 0x3fffd:
		return 2
	}
	return 1
}

// dashColumns is the table's four columns (x, width) inside width cells:
// 32, 18, 30 and 20 per cent with one cell between them, as ratatui's
// solver placed them. The three spacing cells come off the columns a
// hundredth of a cell at a time, each time off the column whose shrinking
// evens out its neighbours most (the solver's weak equal-size rule; the
// leftmost on a tie); then each edge is rounded.
func dashColumns(width int) [4][2]int {
	size := [4]int{32 * width, 18 * width, 30 * width, 20 * width}
	gain := func(i int) int {
		g := 0
		for _, j := range []int{i - 1, i + 1} {
			if j < 0 || j >= 4 {
				continue
			}
			if size[i] > size[j] {
				g++
			} else {
				g--
			}
		}
		return g
	}
	for range 300 {
		best := -1
		for i := 0; i < 4; i++ {
			if size[i] > 0 && (best < 0 || gain(i) > gain(best)) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		size[best]--
	}
	round := func(hundredths int) int { return (hundredths + 50) / 100 }
	var cols [4][2]int
	start := 0
	for i, sz := range size {
		x, end := round(start), round(start+sz)
		start += sz + 100
		cols[i] = [2]int{x, max(end-x, 0)}
	}
	return cols
}

// dashDraw is the frame for a terminal width by height.
func dashDraw(v dashView, width, height int) *dashScreen {
	s := newDashScreen(width, height)

	// Rows: the panes take at least 12, the findings 9 and the footer 3 (its
	// border and the log line); short of room the findings and the footer
	// share what is left after 12.
	top := min(height, max(12, height-12))
	rest := height - top
	footer := min(rest/2, 3)
	findings := rest - footer
	// Columns: 58 and 42 per cent, rounded half up.
	left := (width*58 + 50) / 100

	table := s.block(dashArea{0, 0, left, top}, "GitDCY Dashboard")
	if table.width > 0 && table.height > 0 {
		cols := dashColumns(table.width)
		cells := func(y int, row [4]string, st dashStyle) {
			for i, col := range cols {
				s.text(table.x+col[0], y, col[1], dashFirstLine(row[i]), st)
			}
		}
		yellowBold := dashStyle{yellow: true, bold: true}
		header := table
		header.height = 1
		s.style(header, yellowBold)
		cells(table.y, [4]string{"repo", "branch", "state", "safety"}, yellowBold)

		visible := table.height - 1
		first := 0
		if v.selected >= 0 && v.selected >= visible {
			first = v.selected + 1 - visible
		}
		for n := 0; n < visible && first+n < len(v.rows); n++ {
			y := table.y + 1 + n
			cells(y, v.rows[first+n], dashStyle{})
			if v.selected == first+n {
				row := table
				row.y, row.height = y, 1
				s.style(row, dashStyle{reversed: true})
			}
		}
	}

	detail := s.block(dashArea{left, 0, width - left, top}, "Selected Repo")
	for n := 0; n < len(v.detail) && n < detail.height; n++ {
		s.line(detail, detail.y+n, v.detail[n])
	}

	list := s.block(dashArea{0, top, width, findings}, "Findings")
	y := 0
	for _, item := range v.findings {
		itemLines := []string{""}
		if item != "" {
			itemLines = lines(item)
		}
		if y+len(itemLines) > list.height {
			break
		}
		for _, l := range itemLines {
			s.line(list, list.y+y, []dashRun{{l, false}})
			y++
		}
	}

	log := s.block(dashArea{0, top + findings, width, footer}, "")
	if log.height > 0 {
		s.line(log, log.y, []dashRun{{dashFirstLine(v.log), false}})
	}
	return s
}
