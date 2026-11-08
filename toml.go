package main

// TOML for GitDCY's own files (local.toml, workspace.toml, a folder's
// .gitdcy.local.toml, the rules file and its copy in a repository): a strict
// reader and a writer, written here so GitDCY needs no third-party code.
//
// The reader takes TOML 1.0 (and 1.1's newlines in inline tables and its
// \e and \xHH escapes): comments, bare, quoted and dotted keys, [tables]
// and [[arrays of tables]], every string form, integers (decimal, 0x, 0o,
// 0b), booleans, arrays and inline tables. Floats and dates are read only so
// an unknown key may hold one; a GitDCY setting never takes them. Anything
// else, a key given twice or a table defined twice is refused with its line
// and column, never guessed.
//
// Values are read into Go structs by their `json` field names, as serde read
// them into the Rust structs: a missing field is its zero value (nil for a
// pointer, which is Rust's None) unless `toml:",required"` (an error) or
// `toml:",default=true"`; an unknown key is ignored unless the type has a
// denyUnknownTOML method; a type with enumVariants takes only those words;
// a type with a decodeTOML method reads its own value.
//
// The writer writes what toml::to_string_pretty (toml 1.1) wrote for the
// same values, byte for byte: plain values of a table first, then its
// sub-tables and arrays of tables in field order, arrays of two or more
// values one per line, the shortest fitting string quoting.

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// The tree

type tomlKind int

const (
	tomlString tomlKind = iota
	tomlInteger
	tomlFloat
	tomlBool
	tomlDatetime
	tomlArray
	tomlTableKind
)

// tomlNode is a value and where it was written.
type tomlNode struct {
	kind  tomlKind
	str   string // string, or a datetime's text
	num   int64
	float float64
	bool  bool
	items []*tomlNode
	table *tomlTable
	line  int
	col   int
	span  int
	// An array of tables ([[x]]), which more [[x]] headers may extend.
	tableArray bool
	// An integer too large for 64 bits: kept so an unknown key may hold
	// one, refused when a setting takes it.
	overflow bool
	// Where the key of a `key = value` was written (an unknown key is
	// shown there); 0 when the node's own place is its key's.
	keyLine, keyCol, keySpan int
}

// tomlTable is a table: its keys in the order written.
type tomlTable struct {
	keys     []string
	vals     map[string]*tomlNode
	defined  bool // by a [header]
	implicit bool // made on the way to a deeper table
	dotted   bool // made by a dotted key
	inline   bool // an inline table: closed
	line     int
	col      int
	span     int // its [header]'s width, when a header defines it
}

func newTomlTable(line, col int) *tomlTable {
	return &tomlTable{vals: map[string]*tomlNode{}, line: line, col: col}
}

func (t *tomlTable) set(key string, n *tomlNode) {
	t.keys = append(t.keys, key)
	t.vals[key] = n
}

func (k tomlKind) String() string {
	switch k {
	case tomlString:
		return "string"
	case tomlInteger:
		return "integer"
	case tomlFloat:
		return "floating point"
	case tomlBool:
		return "boolean"
	case tomlDatetime:
		return "datetime"
	case tomlArray:
		return "sequence"
	}
	return "map"
}

// ---------------------------------------------------------------------------
// The reader

type tomlReader struct {
	text  string
	lines []string
	at    int
	line  int
	col   int
}

// tomlError is a TOML problem with its place, shown as the Rust build's
// TOML reader showed one: the line, a caret under the place, the message.
type tomlError struct {
	line, col, span int
	lineText        string
	msg             string
}

func (e *tomlError) Error() string {
	if e.line == 0 {
		return e.msg
	}
	n := strconv.Itoa(e.line)
	pad := strings.Repeat(" ", len(n))
	span := e.span
	if span < 1 {
		span = 1
	}
	caret := strings.Repeat(" ", e.col-1) + strings.Repeat("^", span)
	return fmt.Sprintf("TOML parse error at line %d, column %d\n%s |\n%s | %s\n%s | %s\n%s\n",
		e.line, e.col, pad, n, e.lineText, pad, caret, e.msg)
}

func (r *tomlReader) errAt(line, col, span int, format string, args ...any) error {
	text := ""
	if line >= 1 && line <= len(r.lines) {
		text = r.lines[line-1]
	}
	return &tomlError{line: line, col: col, span: span, lineText: text, msg: fmt.Sprintf(format, args...)}
}

func (r *tomlReader) errHere(format string, args ...any) error {
	return r.errAt(r.line, r.col, 1, format, args...)
}

func (r *tomlReader) done() bool { return r.at >= len(r.text) }
func (r *tomlReader) peek() byte {
	if r.done() {
		return 0
	}
	return r.text[r.at]
}
func (r *tomlReader) peekAt(n int) byte {
	if r.at+n >= len(r.text) {
		return 0
	}
	return r.text[r.at+n]
}

func (r *tomlReader) advance(n int) {
	for i := 0; i < n && r.at < len(r.text); i++ {
		if r.text[r.at] == '\n' {
			r.line++
			r.col = 1
		} else if r.text[r.at]&0xC0 != 0x80 {
			r.col++
		}
		r.at++
	}
}

func (r *tomlReader) skipSpace() {
	for r.peek() == ' ' || r.peek() == '\t' {
		r.advance(1)
	}
}

// skipComment skips a comment, refusing control characters in it.
func (r *tomlReader) skipComment() error {
	if r.peek() != '#' {
		return nil
	}
	for !r.done() && r.peek() != '\n' {
		c := r.peek()
		if c == '\r' && r.peekAt(1) == '\n' {
			break
		}
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return r.errHere("a control character in a comment")
		}
		r.advance(1)
	}
	return nil
}

// endOfLine expects only spaces, a comment and the line's end.
func (r *tomlReader) endOfLine() error {
	r.skipSpace()
	if err := r.skipComment(); err != nil {
		return err
	}
	switch {
	case r.done():
		return nil
	case r.peek() == '\n':
		r.advance(1)
		return nil
	case r.peek() == '\r' && r.peekAt(1) == '\n':
		r.advance(2)
		return nil
	}
	return r.errHere("expected newline, `#`")
}

// skipBlank skips spaces, comments and line ends (inside arrays).
func (r *tomlReader) skipBlank() error {
	for {
		r.skipSpace()
		if err := r.skipComment(); err != nil {
			return err
		}
		switch {
		case r.peek() == '\n':
			r.advance(1)
		case r.peek() == '\r' && r.peekAt(1) == '\n':
			r.advance(2)
		default:
			return nil
		}
	}
}

// tomlParse reads text into its root table.
func tomlParse(text string) (*tomlTable, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	r := &tomlReader{text: text, lines: strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), line: 1, col: 1}
	if !utf8.ValidString(text) {
		return nil, r.errAt(1, 1, 1, "the file is not UTF-8")
	}
	root := newTomlTable(1, 1)
	current := root
	for {
		if err := r.skipBlank(); err != nil {
			return nil, err
		}
		if r.done() {
			return root, nil
		}
		if r.peek() == '[' {
			t, err := r.header(root)
			if err != nil {
				return nil, err
			}
			current = t
		} else {
			if err := r.keyValue(current); err != nil {
				return nil, err
			}
		}
		if err := r.endOfLine(); err != nil {
			return nil, err
		}
	}
}

type tomlKey struct {
	name      string
	line, col int
	span      int
}

// key reads a dotted key.
func (r *tomlReader) key() ([]tomlKey, error) {
	var parts []tomlKey
	for {
		r.skipSpace()
		line, col, start := r.line, r.col, r.at
		var name string
		switch c := r.peek(); {
		case c == '"':
			s, err := r.basicString()
			if err != nil {
				return nil, err
			}
			name = s
		case c == '\'':
			s, err := r.literalString()
			if err != nil {
				return nil, err
			}
			name = s
		case isBareKeyChar(c):
			for isBareKeyChar(r.peek()) {
				r.advance(1)
			}
			name = r.text[start:r.at]
		default:
			if r.done() || c == '\n' || c == '\r' {
				return nil, r.errHere("expected a key")
			}
			return nil, r.errHere("invalid key")
		}
		parts = append(parts, tomlKey{name: name, line: line, col: col, span: utf8.RuneCountInString(r.text[start:r.at])})
		r.skipSpace()
		if r.peek() != '.' {
			return parts, nil
		}
		r.advance(1)
	}
}

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// header reads [a.b] or [[a.b]] and returns the table it opens.
func (r *tomlReader) header(root *tomlTable) (*tomlTable, error) {
	line, col := r.line, r.col
	array := r.peekAt(1) == '['
	if array {
		r.advance(2)
	} else {
		r.advance(1)
	}
	keys, err := r.key()
	if err != nil {
		return nil, err
	}
	r.skipSpace()
	if array {
		if r.peek() != ']' || r.peekAt(1) != ']' {
			return nil, r.errHere("unclosed array table, expected `]]`")
		}
		r.advance(2)
	} else {
		if r.peek() != ']' {
			return nil, r.errHere("unclosed table, expected `]`")
		}
		r.advance(1)
	}
	span := r.col - col
	t := root
	for _, k := range keys[:len(keys)-1] {
		next, err := r.descend(t, k)
		if err != nil {
			return nil, err
		}
		t = next
	}
	last := keys[len(keys)-1]
	existing, ok := t.vals[last.name]
	if array {
		if !ok {
			table := newTomlTable(line, col)
			table.defined, table.span = true, span
			t.set(last.name, &tomlNode{kind: tomlArray, items: []*tomlNode{{kind: tomlTableKind, table: table, line: line, col: col}}, tableArray: true, line: last.line, col: last.col, span: last.span})
			return table, nil
		}
		if existing.kind != tomlArray || !existing.tableArray {
			return nil, r.errAt(last.line, last.col, last.span, "duplicate key `%s`", last.name)
		}
		table := newTomlTable(line, col)
		table.defined, table.span = true, span
		existing.items = append(existing.items, &tomlNode{kind: tomlTableKind, table: table, line: line, col: col})
		return table, nil
	}
	if !ok {
		table := newTomlTable(line, col)
		table.defined, table.span = true, span
		t.set(last.name, &tomlNode{kind: tomlTableKind, table: table, line: last.line, col: last.col, span: last.span})
		return table, nil
	}
	if existing.kind == tomlTableKind && existing.table.implicit && !existing.table.defined && !existing.table.dotted && !existing.table.inline {
		existing.table.defined = true
		existing.table.line, existing.table.col, existing.table.span = line, col, span
		return existing.table, nil
	}
	return nil, r.errAt(last.line, last.col, last.span, "duplicate key `%s`", last.name)
}

// descend goes into (or makes) the table at key k on the way to a header.
func (r *tomlReader) descend(t *tomlTable, k tomlKey) (*tomlTable, error) {
	n, ok := t.vals[k.name]
	if !ok {
		table := newTomlTable(k.line, k.col)
		table.implicit = true
		t.set(k.name, &tomlNode{kind: tomlTableKind, table: table, line: k.line, col: k.col, span: k.span})
		return table, nil
	}
	switch {
	case n.kind == tomlTableKind && !n.table.inline:
		return n.table, nil
	case n.kind == tomlArray && n.tableArray:
		return n.items[len(n.items)-1].table, nil
	}
	return nil, r.errAt(k.line, k.col, k.span, "duplicate key `%s`", k.name)
}

// keyValue reads `key = value` into t.
func (r *tomlReader) keyValue(t *tomlTable) error {
	keys, err := r.key()
	if err != nil {
		return err
	}
	r.skipSpace()
	if r.peek() != '=' {
		return r.errHere("expected `.`, `=`")
	}
	r.advance(1)
	r.skipSpace()
	value, err := r.value()
	if err != nil {
		return err
	}
	for _, k := range keys[:len(keys)-1] {
		n, ok := t.vals[k.name]
		if !ok {
			table := newTomlTable(k.line, k.col)
			table.dotted = true
			t.set(k.name, &tomlNode{kind: tomlTableKind, table: table, line: k.line, col: k.col, span: k.span})
			t = table
			continue
		}
		if n.kind != tomlTableKind || n.table.inline || (n.table.defined && !n.table.dotted) {
			return r.errAt(k.line, k.col, k.span, "duplicate key `%s`", k.name)
		}
		t = n.table
	}
	last := keys[len(keys)-1]
	if _, ok := t.vals[last.name]; ok {
		return r.errAt(last.line, last.col, last.span, "duplicate key `%s`", last.name)
	}
	value.keyLine, value.keyCol, value.keySpan = last.line, last.col, last.span
	t.set(last.name, value)
	return nil
}

// value reads one value.
func (r *tomlReader) value() (*tomlNode, error) {
	line, col, start := r.line, r.col, r.at
	node := func(n *tomlNode) (*tomlNode, error) {
		n.line, n.col = line, col
		n.span = utf8.RuneCountInString(r.text[start:r.at])
		if i := strings.IndexByte(r.text[start:r.at], '\n'); i >= 0 {
			n.span = utf8.RuneCountInString(r.text[start : start+i])
		}
		return n, nil
	}
	switch c := r.peek(); {
	case c == '"':
		var s string
		var err error
		if strings.HasPrefix(r.text[r.at:], `"""`) {
			s, err = r.multiBasicString()
		} else {
			s, err = r.basicString()
		}
		if err != nil {
			return nil, err
		}
		return node(&tomlNode{kind: tomlString, str: s})
	case c == '\'':
		var s string
		var err error
		if strings.HasPrefix(r.text[r.at:], "'''") {
			s, err = r.multiLiteralString()
		} else {
			s, err = r.literalString()
		}
		if err != nil {
			return nil, err
		}
		return node(&tomlNode{kind: tomlString, str: s})
	case c == '[':
		items, err := r.array()
		if err != nil {
			return nil, err
		}
		return node(&tomlNode{kind: tomlArray, items: items})
	case c == '{':
		t, err := r.inlineTable(line, col)
		if err != nil {
			return nil, err
		}
		return node(&tomlNode{kind: tomlTableKind, table: t})
	}
	// A bare value: true, false, a number or a date, up to the next
	// separator.
	end := r.at
	for end < len(r.text) {
		b := r.text[end]
		if b == ',' || b == ']' || b == '}' || b == '\n' || b == '\r' || b == '#' || b == '\t' {
			break
		}
		if b == ' ' {
			// A date and a time may be split by one space.
			if end+1 < len(r.text) && isDigit(r.text[end+1]) && looksLikeDate(r.text[r.at:end]) {
				end++
				continue
			}
			break
		}
		end++
	}
	word := r.text[r.at:end]
	if word == "" {
		if r.done() || r.peek() == '\n' || r.peek() == '\r' {
			return nil, r.errHere("expected a value")
		}
		return nil, r.errHere("invalid value")
	}
	r.advance(end - r.at)
	switch word {
	case "true":
		return node(&tomlNode{kind: tomlBool, bool: true})
	case "false":
		return node(&tomlNode{kind: tomlBool, bool: false})
	}
	if n, ok := parseTomlInteger(word); ok {
		return node(&tomlNode{kind: tomlInteger, num: n})
	}
	if tomlIntegerShape(word) {
		return node(&tomlNode{kind: tomlInteger, str: word, overflow: true})
	}
	if f, ok := parseTomlFloat(word); ok {
		return node(&tomlNode{kind: tomlFloat, float: f})
	}
	if looksLikeDate(word) || looksLikeTime(word) {
		return node(&tomlNode{kind: tomlDatetime, str: word})
	}
	return nil, r.errAt(line, col, utf8.RuneCountInString(word), "invalid value `%s`: a string is written in quotes", word)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func looksLikeDate(s string) bool {
	return len(s) >= 10 && isDigit(s[0]) && isDigit(s[1]) && isDigit(s[2]) && isDigit(s[3]) && s[4] == '-' && isDigit(s[5]) && isDigit(s[6]) && s[7] == '-' && isDigit(s[8]) && isDigit(s[9])
}

func looksLikeTime(s string) bool {
	return len(s) >= 8 && isDigit(s[0]) && isDigit(s[1]) && s[2] == ':' && isDigit(s[3]) && isDigit(s[4]) && s[5] == ':'
}

// parseTomlInteger reads a TOML integer.
func parseTomlInteger(word string) (int64, bool) {
	base := 10
	digits := word
	sign := ""
	switch {
	case strings.HasPrefix(word, "0x"):
		base, digits = 16, word[2:]
	case strings.HasPrefix(word, "0o"):
		base, digits = 8, word[2:]
	case strings.HasPrefix(word, "0b"):
		base, digits = 2, word[2:]
	default:
		if word[0] == '+' || word[0] == '-' {
			sign, digits = word[:1], word[1:]
		}
		if len(digits) > 1 && digits[0] == '0' {
			return 0, false
		}
	}
	if digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") {
		return 0, false
	}
	clean := strings.ReplaceAll(digits, "_", "")
	for _, c := range clean {
		ok := false
		switch base {
		case 10:
			ok = c >= '0' && c <= '9'
		case 16:
			ok = c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		case 8:
			ok = c >= '0' && c <= '7'
		case 2:
			ok = c == '0' || c == '1'
		}
		if !ok {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(sign+clean, base, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// tomlIntegerShape reports a decimal integer too large for 64 bits.
func tomlIntegerShape(word string) bool {
	digits := strings.TrimLeft(word, "+-")
	if len(word)-len(digits) > 1 || digits == "" || digits[0] == '0' || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") {
		return false
	}
	for _, c := range digits {
		if !(c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// parseTomlFloat reads a TOML float.
func parseTomlFloat(word string) (float64, bool) {
	switch strings.TrimLeft(word, "+-") {
	case "inf":
		if strings.HasPrefix(word, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	if !strings.ContainsAny(word, ".eE") {
		return 0, false
	}
	for i, c := range word {
		if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '_' || ((c == '+' || c == '-') && (i == 0 || word[i-1] == 'e' || word[i-1] == 'E'))) {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(word, "_", ""), 64)
	return f, err == nil
}

// basicString reads "...".
func (r *tomlReader) basicString() (string, error) {
	line, col := r.line, r.col
	r.advance(1)
	var b strings.Builder
	for {
		if r.done() || r.peek() == '\n' || r.peek() == '\r' {
			return "", r.errAt(line, col, 1, "invalid basic string, expected `\"`")
		}
		c := r.peek()
		switch {
		case c == '"':
			r.advance(1)
			return b.String(), nil
		case c == '\\':
			if err := r.escape(&b); err != nil {
				return "", err
			}
		case (c < 0x20 && c != '\t') || c == 0x7f:
			return "", r.errHere("a control character in a string")
		default:
			_, size := utf8.DecodeRuneInString(r.text[r.at:])
			b.WriteString(r.text[r.at : r.at+size])
			r.advance(size)
		}
	}
}

// escape reads one escape after a backslash.
func (r *tomlReader) escape(b *strings.Builder) error {
	line, col := r.line, r.col
	r.advance(1)
	c := r.peek()
	simple := map[byte]string{'b': "\b", 't': "\t", 'n': "\n", 'f': "\f", 'r': "\r", '"': "\"", '\\': "\\", 'e': "\x1b"}
	if s, ok := simple[c]; ok {
		b.WriteString(s)
		r.advance(1)
		return nil
	}
	width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
	if width == 0 {
		return r.errAt(line, col, 2, "invalid escape sequence, expected `b`, `e`, `f`, `n`, `r`, `t`, `u`, `U`, `x`, `\\`, `\"`")
	}
	r.advance(1)
	if r.at+width > len(r.text) {
		return r.errAt(line, col, 2, "invalid unicode escape")
	}
	n, err := strconv.ParseUint(r.text[r.at:r.at+width], 16, 32)
	if err != nil || n > utf8.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
		return r.errAt(line, col, 2+width, "invalid unicode escape")
	}
	b.WriteRune(rune(n))
	r.advance(width)
	return nil
}

// multiBasicString reads """...""".
func (r *tomlReader) multiBasicString() (string, error) {
	line, col := r.line, r.col
	r.advance(3)
	if r.peek() == '\n' {
		r.advance(1)
	} else if r.peek() == '\r' && r.peekAt(1) == '\n' {
		r.advance(2)
	}
	var b strings.Builder
	for {
		if r.done() {
			return "", r.errAt(line, col, 3, "invalid multiline basic string, expected `\"\"\"`")
		}
		if strings.HasPrefix(r.text[r.at:], `"""`) {
			// Up to two more quotes belong to the string.
			extra := 0
			for extra < 2 && r.peekAt(3+extra) == '"' {
				extra++
			}
			b.WriteString(strings.Repeat(`"`, extra))
			r.advance(3 + extra)
			return b.String(), nil
		}
		c := r.peek()
		switch {
		case c == '\\':
			// A backslash at a line's end joins it with the next text.
			rest := r.text[r.at+1:]
			trimmed := strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(trimmed, "\n") || strings.HasPrefix(trimmed, "\r\n") {
				r.advance(1)
				for r.peek() == ' ' || r.peek() == '\t' || r.peek() == '\n' || r.peek() == '\r' {
					r.advance(1)
				}
				continue
			}
			if err := r.escape(&b); err != nil {
				return "", err
			}
		case c == '\r' && r.peekAt(1) == '\n':
			b.WriteString("\n")
			r.advance(2)
		case c == '\n':
			b.WriteString("\n")
			r.advance(1)
		case (c < 0x20 && c != '\t') || c == 0x7f:
			return "", r.errHere("a control character in a string")
		default:
			_, size := utf8.DecodeRuneInString(r.text[r.at:])
			b.WriteString(r.text[r.at : r.at+size])
			r.advance(size)
		}
	}
}

// literalString reads '...'.
func (r *tomlReader) literalString() (string, error) {
	line, col := r.line, r.col
	r.advance(1)
	start := r.at
	for {
		if r.done() || r.peek() == '\n' || r.peek() == '\r' {
			return "", r.errAt(line, col, 1, "invalid literal string, expected `'`")
		}
		c := r.peek()
		if c == '\'' {
			s := r.text[start:r.at]
			r.advance(1)
			return s, nil
		}
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return "", r.errHere("a control character in a string")
		}
		r.advance(1)
	}
}

// multiLiteralString reads ”'...”'.
func (r *tomlReader) multiLiteralString() (string, error) {
	line, col := r.line, r.col
	r.advance(3)
	if r.peek() == '\n' {
		r.advance(1)
	} else if r.peek() == '\r' && r.peekAt(1) == '\n' {
		r.advance(2)
	}
	var b strings.Builder
	for {
		if r.done() {
			return "", r.errAt(line, col, 3, "invalid multiline literal string, expected `'''`")
		}
		if strings.HasPrefix(r.text[r.at:], "'''") {
			extra := 0
			for extra < 2 && r.peekAt(3+extra) == '\'' {
				extra++
			}
			b.WriteString(strings.Repeat("'", extra))
			r.advance(3 + extra)
			return b.String(), nil
		}
		c := r.peek()
		switch {
		case c == '\r' && r.peekAt(1) == '\n':
			b.WriteString("\n")
			r.advance(2)
		case c == '\n':
			b.WriteString("\n")
			r.advance(1)
		case (c < 0x20 && c != '\t') || c == 0x7f:
			return "", r.errHere("a control character in a string")
		default:
			_, size := utf8.DecodeRuneInString(r.text[r.at:])
			b.WriteString(r.text[r.at : r.at+size])
			r.advance(size)
		}
	}
}

// array reads [a, b, ...].
func (r *tomlReader) array() ([]*tomlNode, error) {
	line, col := r.line, r.col
	r.advance(1)
	items := []*tomlNode{}
	for {
		if err := r.skipBlank(); err != nil {
			return nil, err
		}
		if r.peek() == ']' {
			r.advance(1)
			return items, nil
		}
		if r.done() {
			return nil, r.errAt(line, col, 1, "unclosed array, expected `]`")
		}
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		if err := r.skipBlank(); err != nil {
			return nil, err
		}
		switch r.peek() {
		case ',':
			r.advance(1)
		case ']':
			r.advance(1)
			return items, nil
		default:
			return nil, r.errHere("expected `,`, `]`")
		}
	}
}

// inlineTable reads { a = 1, b.c = 2 }.
func (r *tomlReader) inlineTable(line, col int) (*tomlTable, error) {
	r.advance(1)
	t := newTomlTable(line, col)
	for {
		if err := r.skipBlank(); err != nil {
			return nil, err
		}
		if r.peek() == '}' {
			r.advance(1)
			break
		}
		if r.done() {
			return nil, r.errAt(line, col, 1, "unclosed inline table, expected `}`")
		}
		if err := r.keyValue(t); err != nil {
			return nil, err
		}
		if err := r.skipBlank(); err != nil {
			return nil, err
		}
		switch r.peek() {
		case ',':
			r.advance(1)
		case '}':
			r.advance(1)
			sealInline(t)
			return t, nil
		default:
			return nil, r.errHere("expected `,`, `}`")
		}
	}
	sealInline(t)
	return t, nil
}

func sealInline(t *tomlTable) {
	t.inline = true
	for _, n := range t.vals {
		if n.kind == tomlTableKind {
			sealInline(n.table)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading into Go values

type tomlEnum interface{ enumVariants() []string }
type tomlDenyUnknown interface{ denyUnknownTOML() }

// tomlValueDecoder is a type that reads its own value from a TOML value:
// a string, int64, bool, []any or map[string]any.
type tomlValueDecoder interface{ decodeTOML(v any) error }

// tomlUnmarshal reads TOML text into v, a pointer to a struct.
func tomlUnmarshal(text string, v any) error {
	root, err := tomlParse(text)
	if err != nil {
		return err
	}
	r := &tomlReader{lines: strings.Split(strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n"), "\n")}
	rv := reflect.ValueOf(v).Elem()
	return r.decodeTable(&tomlNode{kind: tomlTableKind, table: root, line: 1, col: 1}, rv, true)
}

func (r *tomlReader) typeError(n *tomlNode, expected string) error {
	return r.errAt(n.line, n.col, n.span, "invalid type: %s, expected %s", describeNode(n), expected)
}

func describeNode(n *tomlNode) string {
	switch n.kind {
	case tomlString:
		return "string " + rustDebug(n.str)
	case tomlInteger:
		if n.overflow {
			return "integer `" + n.str + "`"
		}
		return "integer `" + strconv.FormatInt(n.num, 10) + "`"
	case tomlFloat:
		return "floating point `" + strconv.FormatFloat(n.float, 'g', -1, 64) + "`"
	case tomlBool:
		return "boolean `" + strconv.FormatBool(n.bool) + "`"
	case tomlDatetime:
		return "datetime `" + n.str + "`"
	case tomlArray:
		return "sequence"
	}
	return "map"
}

// serdeOneOf is serde's list of the words a name may be: "`a`", "`a` or
// `b`", else "one of `a`, `b`, `c`".
func serdeOneOf(names []string) string {
	switch len(names) {
	case 1:
		return "`" + names[0] + "`"
	case 2:
		return "`" + names[0] + "` or `" + names[1] + "`"
	}
	return "one of `" + strings.Join(names, "`, `") + "`"
}

// expectedText is how serde named what a Go type takes.
func expectedText(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return expectedText(t.Elem())
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Uint64, reflect.Uint, reflect.Uint32:
		return "u64"
	case reflect.Int64, reflect.Int:
		return "i64"
	case reflect.Slice:
		return "a sequence"
	case reflect.Map:
		return "a map"
	case reflect.Struct:
		return "struct " + t.Name()
	}
	return t.String()
}

// plainValue is a node as a plain Go value (for decodeTOML).
func plainValue(n *tomlNode) any {
	switch n.kind {
	case tomlString, tomlDatetime:
		return n.str
	case tomlInteger:
		if n.overflow {
			return n.str
		}
		return n.num
	case tomlFloat:
		return n.float
	case tomlBool:
		return n.bool
	case tomlArray:
		list := make([]any, len(n.items))
		for i, item := range n.items {
			list[i] = plainValue(item)
		}
		return list
	}
	m := map[string]any{}
	for _, k := range n.table.keys {
		m[k] = plainValue(n.table.vals[k])
	}
	return m
}

func (r *tomlReader) decode(n *tomlNode, v reflect.Value) error {
	if v.CanAddr() {
		if d, ok := v.Addr().Interface().(tomlValueDecoder); ok {
			if err := d.decodeTOML(plainValue(n)); err != nil {
				return r.errAt(n.line, n.col, n.span, "%s", err.Error())
			}
			return nil
		}
	}
	if v.Kind() == reflect.Pointer {
		elem := reflect.New(v.Type().Elem())
		if err := r.decode(n, elem.Elem()); err != nil {
			return err
		}
		v.Set(elem)
		return nil
	}
	if e, ok := v.Interface().(tomlEnum); ok && v.Kind() == reflect.String {
		if n.kind != tomlString {
			return r.typeError(n, "enum "+v.Type().Name())
		}
		for _, variant := range e.enumVariants() {
			if variant == n.str {
				v.SetString(n.str)
				return nil
			}
		}
		return r.errAt(n.line, n.col, n.span, "unknown variant `%s`, expected %s", n.str, serdeOneOf(e.enumVariants()))
	}
	switch v.Kind() {
	case reflect.String:
		if n.kind != tomlString {
			return r.typeError(n, expectedText(v.Type()))
		}
		v.SetString(n.str)
	case reflect.Bool:
		if n.kind != tomlBool {
			return r.typeError(n, "a boolean")
		}
		v.SetBool(n.bool)
	case reflect.Uint64, reflect.Uint, reflect.Uint32:
		if n.kind != tomlInteger {
			return r.typeError(n, expectedText(v.Type()))
		}
		if n.overflow {
			return r.errAt(n.line, n.col, n.span, "number too large to fit in target type")
		}
		if n.num < 0 {
			return r.errAt(n.line, n.col, n.span, "invalid value: integer `%d`, expected %s", n.num, expectedText(v.Type()))
		}
		if v.OverflowUint(uint64(n.num)) {
			return r.errAt(n.line, n.col, n.span, "invalid value: integer `%d`, expected %s", n.num, expectedText(v.Type()))
		}
		v.SetUint(uint64(n.num))
	case reflect.Int64, reflect.Int:
		if n.kind != tomlInteger {
			return r.typeError(n, expectedText(v.Type()))
		}
		if n.overflow {
			return r.errAt(n.line, n.col, n.span, "number too large to fit in target type")
		}
		v.SetInt(n.num)
	case reflect.Slice:
		if n.kind != tomlArray {
			return r.typeError(n, "a sequence")
		}
		list := reflect.MakeSlice(v.Type(), len(n.items), len(n.items))
		for i, item := range n.items {
			if err := r.decode(item, list.Index(i)); err != nil {
				return err
			}
		}
		v.Set(list)
	case reflect.Map:
		if n.kind != tomlTableKind {
			return r.typeError(n, "a map")
		}
		m := reflect.MakeMapWithSize(v.Type(), len(n.table.keys))
		for _, k := range n.table.keys {
			elem := reflect.New(v.Type().Elem()).Elem()
			if s, ok := elem.Addr().Interface().(interface{ setDefaults() }); ok {
				s.setDefaults()
			}
			if err := r.decode(n.table.vals[k], elem); err != nil {
				return err
			}
			m.SetMapIndex(reflect.ValueOf(k).Convert(v.Type().Key()), elem)
		}
		v.Set(m)
	case reflect.Struct:
		return r.decodeTable(n, v, false)
	default:
		return r.typeError(n, v.Type().String())
	}
	return nil
}

// decodeTable reads a table into a struct.
func (r *tomlReader) decodeTable(n *tomlNode, v reflect.Value, root bool) error {
	if n.kind != tomlTableKind {
		return r.typeError(n, expectedText(v.Type()))
	}
	t := v.Type()
	fields := map[string]int{}
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _ := jsonTag(f)
		if name == "-" || !f.IsExported() {
			continue
		}
		fields[name] = i
		names = append(names, name)
		// Defaults first, so a missing field keeps them.
		opts := f.Tag.Get("toml")
		if strings.Contains(opts, "default=true") {
			v.Field(i).SetBool(true)
		}
	}
	_, deny := v.Addr().Interface().(tomlDenyUnknown)
	// The toml crate gave serde a table's keys sorted (a BTreeMap), so the
	// first problem reported is the one under the first key in that order.
	for _, k := range sortedKeys(n.table.vals) {
		i, ok := fields[k]
		if !ok {
			if deny {
				kn := n.table.vals[k]
				line, col, span := kn.line, kn.col, kn.span
				if kn.keyLine > 0 {
					line, col, span = kn.keyLine, kn.keyCol, kn.keySpan
				}
				if len(names) == 0 {
					return r.errAt(line, col, span, "unknown field `%s`, there are no fields", k)
				}
				return r.errAt(line, col, span, "unknown field `%s`, expected %s", k, serdeOneOf(names))
			}
			continue
		}
		if err := r.decode(n.table.vals[k], v.Field(i)); err != nil {
			return err
		}
	}
	for _, name := range names {
		f := t.Field(fields[name])
		if strings.Contains(f.Tag.Get("toml"), "required") {
			if _, ok := n.table.vals[name]; !ok {
				// Shown under the table's [header], or the whole inline table.
				line, col, span := n.table.line, n.table.col, n.table.span
				if span == 0 && n.table.inline {
					span = n.span
				}
				if root {
					line, col, span = 1, 1, 1
				}
				return r.errAt(line, col, span, "missing field `%s`", name)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The writer

// tomlValueEncoder is a type that writes itself as another value.
type tomlValueEncoder interface{ encodeTOML() any }

type tomlOutTable struct {
	key         []string
	body        strings.Builder
	hasChildren bool
	array       bool
}

type tomlOut struct {
	tables []*tomlOutTable
}

// tomlMarshal writes v (a struct) as toml::to_string_pretty did.
func tomlMarshal(v any) (string, error) {
	out := &tomlOut{}
	root := out.newTable(nil)
	if err := out.writeTable(root, reflect.ValueOf(v)); err != nil {
		return "", err
	}
	var b strings.Builder
	first := true
	for _, t := range out.tables {
		required := t.array || t.body.Len() > 0 || !t.hasChildren
		if t.key == nil {
			required = t.body.Len() > 0
		}
		if !required {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		if t.key != nil {
			keys := make([]string, len(t.key))
			for i, k := range t.key {
				keys[i] = tomlKeyText(k)
			}
			if t.array {
				b.WriteString("[[" + strings.Join(keys, ".") + "]]\n")
			} else {
				b.WriteString("[" + strings.Join(keys, ".") + "]\n")
			}
		}
		b.WriteString(t.body.String())
	}
	return b.String(), nil
}

func (o *tomlOut) newTable(key []string) *tomlOutTable {
	t := &tomlOutTable{key: key}
	o.tables = append(o.tables, t)
	return t
}

func childKey(parent *tomlOutTable, key string) []string {
	return append(append([]string{}, parent.key...), key)
}

type tomlStrategy int

const (
	strategyValue tomlStrategy = iota
	strategyTable
	strategyArrayOfTables
	strategySkip
)

// strategyOf is how a value is written inside a table, as toml's
// SerializationStrategy decided.
func strategyOf(v reflect.Value) tomlStrategy {
	if !v.IsValid() {
		return strategySkip
	}
	if e, ok := v.Interface().(tomlValueEncoder); ok {
		return strategyOf(reflect.ValueOf(e.encodeTOML()))
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return strategySkip
		}
		return strategyOf(v.Elem())
	case reflect.Map, reflect.Struct:
		return strategyTable
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return strategyValue
		}
		for i := 0; i < v.Len(); i++ {
			if strategyOf(v.Index(i)) != strategyTable {
				return strategyValue
			}
		}
		return strategyArrayOfTables
	}
	return strategyValue
}

// writeTable writes a struct or map's entries into t.
func (o *tomlOut) writeTable(t *tomlOutTable, v reflect.Value) error {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	type entry struct {
		key   string
		value reflect.Value
	}
	var entries []entry
	switch v.Kind() {
	case reflect.Struct:
		ty := v.Type()
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name, omitEmpty := jsonTag(f)
			if name == "-" || !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			if omitEmpty && emptyForJSON(fv) {
				continue
			}
			entries = append(entries, entry{name, fv})
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			entries = append(entries, entry{k.String(), v.MapIndex(k)})
		}
	default:
		return errf("TOML cannot hold a %s here", v.Kind())
	}
	for _, e := range entries {
		switch strategyOf(e.value) {
		case strategySkip:
		case strategyValue:
			t.body.WriteString(tomlKeyText(e.key) + " = ")
			if err := writeTomlValue(&t.body, e.value); err != nil {
				return err
			}
			t.body.WriteString("\n")
		case strategyTable:
			t.hasChildren = true
			child := o.newTable(childKey(t, e.key))
			if err := o.writeTable(child, e.value); err != nil {
				return err
			}
		case strategyArrayOfTables:
			t.hasChildren = true
			list := e.value
			for list.Kind() == reflect.Pointer || list.Kind() == reflect.Interface {
				list = list.Elem()
			}
			for i := 0; i < list.Len(); i++ {
				child := o.newTable(childKey(t, e.key))
				child.array = true
				t.hasChildren = true
				if err := o.writeTable(child, list.Index(i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeTomlValue writes a plain value: a string, number, boolean or array
// (two or more items one per line).
func writeTomlValue(b *strings.Builder, v reflect.Value) error {
	if e, ok := v.Interface().(tomlValueEncoder); ok {
		return writeTomlValue(b, reflect.ValueOf(e.encodeTOML()))
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return errf("unsupported None value")
		}
		return writeTomlValue(b, v.Elem())
	case reflect.String:
		b.WriteString(tomlStringText(v.String()))
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Slice, reflect.Array:
		multi := v.Len() >= 2
		b.WriteString("[")
		for i := 0; i < v.Len(); i++ {
			if multi {
				b.WriteString("\n    ")
			} else if i > 0 {
				b.WriteString(", ")
			}
			if err := writeTomlValue(b, v.Index(i)); err != nil {
				return err
			}
			if multi {
				b.WriteString(",")
			}
		}
		if multi {
			b.WriteString("\n")
		}
		b.WriteString("]")
	case reflect.Map, reflect.Struct:
		return writeInlineTable(b, v)
	default:
		return errf("TOML cannot hold a %s", v.Kind())
	}
	return nil
}

// writeInlineTable writes a table inside an array of plain values.
func writeInlineTable(b *strings.Builder, v reflect.Value) error {
	b.WriteString("{ ")
	first := true
	put := func(key string, fv reflect.Value) error {
		if strategyOf(fv) == strategySkip {
			return nil
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(tomlKeyText(key) + " = ")
		return writeTomlValue(b, fv)
	}
	if v.Kind() == reflect.Map {
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			if err := put(k.String(), v.MapIndex(k)); err != nil {
				return err
			}
		}
	} else {
		ty := v.Type()
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name, omitEmpty := jsonTag(f)
			if name == "-" || !f.IsExported() || (omitEmpty && emptyForJSON(v.Field(i))) {
				continue
			}
			if err := put(name, v.Field(i)); err != nil {
				return err
			}
		}
	}
	b.WriteString(" }")
	return nil
}

// tomlKeyText is a key as toml_writer wrote it: bare when it can be, else
// the shortest quoting.
func tomlKeyText(s string) string {
	bare := s != ""
	single, double, controls, backslash := false, false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isBareKeyChar(c) {
			bare = false
		}
		switch {
		case c == '\'':
			single = true
		case c == '"':
			double = true
		case c == '\\':
			backslash = true
		case c == '\t':
		case c <= 0x1f || c == 0x7f:
			controls = true
		}
	}
	switch {
	case bare:
		return s
	case !controls && !backslash && !double:
		return tomlBasic(s, false)
	case !controls && !single:
		return "'" + s + "'"
	}
	return tomlBasic(s, false)
}

// tomlStringText is a string value as toml_writer's default wrote it: a
// basic string if it needs no escapes, else a literal one, else multi-line
// forms, else an escaped basic string.
func tomlStringText(s string) string {
	maxSingle, maxDouble := 0, 0
	runSingle, runDouble := 0, 0
	controls, backslash, newline := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			runSingle++
			if runSingle > maxSingle {
				maxSingle = runSingle
			}
		} else {
			runSingle = 0
		}
		if c == '"' {
			runDouble++
			if runDouble > maxDouble {
				maxDouble = runDouble
			}
		} else {
			runDouble = 0
		}
		switch {
		case c == '\\':
			backslash = true
		case c == '\t':
		case c == '\n':
			newline = true
		case c <= 0x1f || c == 0x7f:
			controls = true
		}
	}
	switch {
	case !controls && !backslash && maxDouble == 0 && !newline:
		return tomlBasic(s, false)
	case !controls && maxSingle == 0 && !newline:
		return "'" + s + "'"
	case !controls && !backslash && maxDouble <= 2:
		return tomlBasicML(s, newline)
	case !controls && maxSingle <= 2:
		if newline {
			return "'''\n" + s + "'''"
		}
		return "'''" + s + "'''"
	case newline:
		return tomlBasicML(s, true)
	}
	return tomlBasic(s, false)
}

func tomlBasicML(s string, newline bool) string {
	text := tomlEscape(s, true)
	if newline {
		return "\"\"\"\n" + text + "\"\"\""
	}
	return `"""` + text + `"""`
}

func tomlBasic(s string, _ bool) string { return `"` + tomlEscape(s, false) + `"` }

// tomlEscape escapes s for a basic string as toml_writer did.
func tomlEscape(s string, ml bool) string {
	maxDouble := 0
	if ml {
		maxDouble = 2
	}
	var b strings.Builder
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			run++
			if run > maxDouble {
				b.WriteString(`\"`)
				run = 0
				continue
			}
		} else {
			run = 0
		}
		switch {
		case c == 0x8:
			b.WriteString(`\b`)
		case c == 0x9:
			b.WriteString(`\t`)
		case c == 0xa && !ml:
			b.WriteString(`\n`)
		case c == 0xc:
			b.WriteString(`\f`)
		case c == 0xd:
			b.WriteString(`\r`)
		case c == '\\':
			b.WriteString(`\\`)
		case (c <= 0x1f && c != 0xa) || c == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
