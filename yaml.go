package main

// A reader for the YAML GitDCY kept its settings in before TOML: the block
// style its own YAML writer produced, and the same written by hand
// (comments, quoted keys and values, one-line `[a, b]` lists, `[]` and
// `{}`). Anything else (anchors, aliases, tags, block scalars, flow
// mappings, several documents, tabs, a quoted value over several lines, a
// repeated key) is refused with its line, never guessed. It only reads:
// GitDCY writes TOML.
//
// Values come out as the YAML writer's reader gave them: a string setting
// takes any scalar's text as written, `~`, `null` and nothing are "none"
// for an optional setting, and `true`/`false`, numbers and names are only
// what their settings take. Typed values and their errors are those of the
// Rust build (serde's derive over yaml.rs), word for word.

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

type yamlKind int

const (
	yamlScalar yamlKind = iota
	yamlSeq
	yamlMap
)

// yamlNode is one value of the tree: a scalar (its text, whether it was
// written without quotes, its line), a sequence of items, or a mapping with
// its entries in the order written.
type yamlNode struct {
	kind yamlKind
	text string
	// Written without quotes: only these can be none, true or a number.
	plain   bool
	line    int
	items   []*yamlNode
	entries []yamlEntry
}

type yamlEntry struct {
	key   string
	value *yamlNode
}

func yamlRefuse(line int, what string) error { return errf("line %d: %s", line, what) }

// yamlParse is the tree in text; nil for a document with nothing in it (or
// only comments).
func yamlParse(text string) (*yamlNode, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	var found []yamlLine
	for index, raw := range lines(text) {
		number := index + 1
		content := strings.TrimLeft(raw, " \t")
		t := strings.TrimRight(content, " \t")
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lead := raw[:len(raw)-len(content)]
		if strings.Contains(lead, "\t") {
			return nil, yamlRefuse(number, "a tab in the indentation")
		}
		if lead == "" && (t == "---" || t == "...") {
			if t == "---" && len(found) == 0 {
				continue
			}
			return nil, yamlRefuse(number, "a second document")
		}
		if lead == "" && (strings.HasPrefix(t, "--- ") || strings.HasPrefix(t, "... ")) {
			return nil, yamlRefuse(number, "a document marker with something after it")
		}
		if lead == "" && strings.HasPrefix(t, "%") {
			return nil, yamlRefuse(number, "a YAML directive")
		}
		found = append(found, yamlLine{number: number, indent: len(lead), text: t})
	}
	if len(found) == 0 {
		return nil, nil
	}
	p := &yamlParser{lines: found}
	node, err := p.block(found[0].indent)
	if err != nil {
		return nil, err
	}
	if line, ok := p.peek(); ok {
		return nil, yamlRefuse(line.number, "unexpected indentation")
	}
	return node, nil
}

type yamlLine struct {
	number int
	indent int
	text   string
}

type yamlParser struct {
	lines []yamlLine
	at    int
}

func yamlIsItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *yamlParser) peek() (yamlLine, bool) {
	if p.at < len(p.lines) {
		return p.lines[p.at], true
	}
	return yamlLine{}, false
}

func (p *yamlParser) block(indent int) (*yamlNode, error) {
	if line, ok := p.peek(); ok && yamlIsItem(line.text) {
		return p.seq(indent)
	}
	return p.mapping(indent)
}

// nested is what follows `key:` or `-` with nothing after it on its line: a
// deeper block, a list at the key's own indentation (as the YAML writer put
// lists), or nothing.
func (p *yamlParser) nested(indent, number int, sameIndentList bool) (*yamlNode, error) {
	if next, ok := p.peek(); ok {
		if next.indent > indent {
			return p.block(next.indent)
		}
		if sameIndentList && next.indent == indent && yamlIsItem(next.text) {
			return p.seq(indent)
		}
	}
	return &yamlNode{kind: yamlScalar, plain: true, line: number}, nil
}

func (p *yamlParser) seq(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlSeq}
	for {
		line, ok := p.peek()
		if !ok || line.indent < indent || (line.indent == indent && !yamlIsItem(line.text)) {
			break
		}
		if line.indent > indent {
			return nil, yamlRefuse(line.number, "unexpected indentation")
		}
		rest := strings.TrimLeft(line.text[1:], " ")
		var item *yamlNode
		var err error
		switch {
		case rest == "":
			p.at++
			item, err = p.nested(indent, line.number, false)
		case yamlIsItem(rest):
			return nil, yamlRefuse(line.number, "a list inside a list on one line")
		case yamlKeyStarts(rest, line.number):
			// A mapping that starts on the item's line, at its column.
			column := indent + len(line.text) - len(rest)
			p.lines[p.at] = yamlLine{number: line.number, indent: column, text: rest}
			item, err = p.mapping(column)
		default:
			p.at++
			if item, err = yamlInline(rest, line.number); err == nil {
				err = p.noDeeper(indent)
			}
		}
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, item)
	}
	return node, nil
}

func (p *yamlParser) mapping(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlMap}
	for {
		line, ok := p.peek()
		if !ok || line.indent < indent {
			break
		}
		if line.indent > indent || yamlIsItem(line.text) {
			return nil, yamlRefuse(line.number, "unexpected indentation")
		}
		key, rest, err := yamlSplitKey(line.text, line.number)
		if err != nil {
			return nil, err
		}
		for _, known := range node.entries {
			if known.key == key {
				return nil, yamlRefuse(line.number, "the key "+rustDebug(key)+" a second time")
			}
		}
		p.at++
		var value *yamlNode
		if rest == "" || strings.HasPrefix(rest, "#") {
			value, err = p.nested(indent, line.number, true)
		} else if value, err = yamlInline(rest, line.number); err == nil {
			err = p.noDeeper(indent)
		}
		if err != nil {
			return nil, err
		}
		node.entries = append(node.entries, yamlEntry{key: key, value: value})
	}
	return node, nil
}

func (p *yamlParser) noDeeper(indent int) error {
	if next, ok := p.peek(); ok && next.indent > indent {
		return yamlRefuse(next.number, "unexpected indentation")
	}
	return nil
}

func yamlStartsQuoted(text string) bool {
	return strings.HasPrefix(text, "\"") || strings.HasPrefix(text, "'")
}

// yamlSplitKey splits `key: rest` into the key and what follows the colon.
func yamlSplitKey(text string, line int) (string, string, error) {
	var key, after string
	if yamlStartsQuoted(text) {
		k, used, err := yamlQuoted(text, line)
		if err != nil {
			return "", "", err
		}
		key, after = k, text[used:]
	} else {
		colon := -1
		for i := 0; i < len(text); i++ {
			if text[i] == ':' && (i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\t') {
				colon = i
				break
			}
		}
		if colon < 0 {
			return "", "", yamlRefuse(line, "expected a key and a colon")
		}
		key = strings.TrimRight(text[:colon], " \t")
		if key == "" || !yamlPlainStart(key) || strings.Contains(key, " #") {
			return "", "", yamlRefuse(line, "a key GitDCY does not read: "+rustDebug(key))
		}
		after = text[colon:]
	}
	rest, ok := strings.CutPrefix(after, ":")
	if !ok {
		return "", "", yamlRefuse(line, "expected a colon after the key")
	}
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", "", yamlRefuse(line, "expected a space after the colon")
	}
	return key, strings.TrimLeft(rest, " \t"), nil
}

func yamlKeyStarts(text string, line int) bool {
	_, _, err := yamlSplitKey(text, line)
	return err == nil
}

// yamlPlainStart is whether unquoted text may start this way: not with a
// YAML indicator, but `-`, `?` and `:` may lead when something other than a
// space follows (`--all`, `-D`).
func yamlPlainStart(text string) bool {
	if text == "" {
		return false
	}
	switch text[0] {
	case '-', '?', ':':
		return len(text) > 1 && text[1] != ' ' && text[1] != '\t'
	}
	return !strings.ContainsRune(",[]{}#&*!|>'\"%@`", rune(text[0]))
}

// yamlOnlyComment checks what follows a value: nothing, or a comment.
func yamlOnlyComment(rest string, line int) error {
	trimmed := strings.TrimLeft(rest, " \t")
	if trimmed == "" || (strings.HasPrefix(trimmed, "#") && len(trimmed) < len(rest)) {
		return nil
	}
	return yamlRefuse(line, "unexpected text after a value: "+rustDebug(trimmed))
}

// yamlInline is a value on the line of its key or `-`.
func yamlInline(text string, line int) (*yamlNode, error) {
	if yamlStartsQuoted(text) {
		value, used, err := yamlQuoted(text, line)
		if err != nil {
			return nil, err
		}
		if err := yamlOnlyComment(text[used:], line); err != nil {
			return nil, err
		}
		return &yamlNode{kind: yamlScalar, text: value, line: line}, nil
	}
	if inner, ok := strings.CutPrefix(text, "["); ok {
		return yamlFlowList(inner, line)
	}
	if after, ok := strings.CutPrefix(text, "{}"); ok {
		if err := yamlOnlyComment(after, line); err != nil {
			return nil, err
		}
		return &yamlNode{kind: yamlMap}, nil
	}
	var first byte
	if text != "" {
		first = text[0]
	}
	switch {
	case first == '&':
		return nil, yamlRefuse(line, "an anchor (&)")
	case first == '*':
		return nil, yamlRefuse(line, "an alias (*)")
	case first == '!':
		return nil, yamlRefuse(line, "a tag (!)")
	case first == '|' || first == '>':
		return nil, yamlRefuse(line, "a value over several lines (| or >)")
	case first == '{':
		return nil, yamlRefuse(line, "a mapping written on one line ({...})")
	case first == '-' && yamlIsItem(text):
		return nil, yamlRefuse(line, "a list on the line of its key")
	case !yamlPlainStart(text):
		return nil, yamlRefuse(line, "a value GitDCY does not read: "+rustDebug(text))
	}
	value, err := yamlPlain(text, line)
	if err != nil {
		return nil, err
	}
	return &yamlNode{kind: yamlScalar, text: value, plain: true, line: line}, nil
}

// yamlPlain is an unquoted value: up to a comment, not holding ": ".
func yamlPlain(text string, line int) (string, error) {
	end := len(text)
	for i := 1; i < len(text); i++ {
		if text[i] == '#' && (text[i-1] == ' ' || text[i-1] == '\t') {
			end = i
			break
		}
	}
	value := strings.TrimRight(text[:end], " \t")
	if strings.Contains(value, ": ") || strings.Contains(value, ":\t") || strings.HasSuffix(value, ":") {
		return "", yamlRefuse(line, "a colon inside a value; quote it: "+rustDebug(value))
	}
	return value, nil
}

// yamlFlowList is `[a, 'b', "c"]` after its `[`.
func yamlFlowList(inner string, line int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlSeq}
	rest := strings.TrimLeft(inner, " \t")
	if after, ok := strings.CutPrefix(rest, "]"); ok {
		if err := yamlOnlyComment(after, line); err != nil {
			return nil, err
		}
		return node, nil
	}
	for {
		var item *yamlNode
		var used int
		if yamlStartsQuoted(rest) {
			value, n, err := yamlQuoted(rest, line)
			if err != nil {
				return nil, err
			}
			item, used = &yamlNode{kind: yamlScalar, text: value, line: line}, n
		} else {
			end := strings.IndexAny(rest, ",]")
			if end < 0 {
				end = len(rest)
			}
			t := strings.TrimRight(rest[:end], " \t")
			if !yamlPlainStart(t) || strings.ContainsAny(t, "[{}") || strings.Contains(t, " #") {
				return nil, yamlRefuse(line, "a list item GitDCY does not read: "+rustDebug(t))
			}
			value, err := yamlPlain(t, line)
			if err != nil {
				return nil, err
			}
			item, used = &yamlNode{kind: yamlScalar, text: value, plain: true, line: line}, end
		}
		node.items = append(node.items, item)
		rest = strings.TrimLeft(rest[used:], " \t")
		if after, ok := strings.CutPrefix(rest, "]"); ok {
			if err := yamlOnlyComment(after, line); err != nil {
				return nil, err
			}
			return node, nil
		}
		after, ok := strings.CutPrefix(rest, ",")
		if !ok {
			return nil, yamlRefuse(line, "a list written on one line must end with ]")
		}
		rest = strings.TrimLeft(after, " \t")
		if strings.HasPrefix(rest, "]") {
			return nil, yamlRefuse(line, "an empty item in a list")
		}
	}
}

// yamlEscapes are the one-character escapes of a double-quoted value.
var yamlEscapes = map[rune]rune{
	'0': 0, 'a': 7, 'b': 8, 't': '\t', '\t': '\t', 'n': '\n', 'v': 0xb, 'f': 0xc,
	'r': '\r', 'e': 0x1b, ' ': ' ', '"': '"', '/': '/', '\\': '\\', 'N': 0x85,
	'_': 0xa0, 'L': 0x2028, 'P': 0x2029,
}

// yamlQuoted is the quoted value at the start of text: its text and the
// bytes it used.
func yamlQuoted(text string, line int) (string, int, error) {
	var out strings.Builder
	quote := text[0]
	i := 1
	for i < len(text) {
		c, size := utf8.DecodeRuneInString(text[i:])
		if quote == '\'' {
			if c == '\'' {
				if strings.HasPrefix(text[i+1:], "'") {
					out.WriteByte('\'')
					i += 2
					continue
				}
				return out.String(), i + 1, nil
			}
			out.WriteString(text[i : i+size])
			i += size
			continue
		}
		switch c {
		case '"':
			return out.String(), i + 1, nil
		case '\\':
			i += size
			if i >= len(text) {
				return "", 0, yamlRefuse(line, "a quoted value that does not end on its line")
			}
			escape, esize := utf8.DecodeRuneInString(text[i:])
			escapeText := text[i : i+esize]
			i += esize
			if r, ok := yamlEscapes[escape]; ok {
				out.WriteRune(r)
				continue
			}
			var digits int
			switch escape {
			case 'x':
				digits = 2
			case 'u':
				digits = 4
			case 'U':
				digits = 8
			default:
				return "", 0, yamlRefuse(line, "the escape \\"+escapeText)
			}
			start := i
			for n := 0; n < digits && i < len(text); n++ {
				_, s := utf8.DecodeRuneInString(text[i:])
				i += s
			}
			hex := text[start:i]
			code, ok := yamlHexChar(hex, digits)
			if !ok {
				return "", 0, yamlRefuse(line, "the escape \\"+escapeText+hex)
			}
			out.WriteRune(code)
		default:
			out.WriteString(text[i : i+size])
			i += size
		}
	}
	return "", 0, yamlRefuse(line, "a quoted value that does not end on its line")
}

// yamlHexChar is the character a \x, \u or \U escape's digits name: as
// many bytes as digits, read as Rust's u32::from_str_radix(hex, 16) read
// them (a leading + allowed), and a character (no surrogate, at most
// U+10FFFF).
func yamlHexChar(hex string, digits int) (rune, bool) {
	if len(hex) != digits {
		return 0, false
	}
	hex = strings.TrimPrefix(hex, "+")
	if hex == "" {
		return 0, false
	}
	var code uint32
	for i := 0; i < len(hex); i++ {
		d, ok := yamlDigit(hex[i], 16)
		if !ok {
			return 0, false
		}
		code = code<<4 | uint32(d)
	}
	if code >= 0xd800 && code <= 0xdfff || code > utf8.MaxRune {
		return 0, false
	}
	return rune(code), true
}

// yamlDigit is c's value as a digit in radix (ASCII digits and letters, in
// either case).
func yamlDigit(c byte, radix uint64) (uint64, bool) {
	var d uint64
	switch {
	case c >= '0' && c <= '9':
		d = uint64(c - '0')
	case c >= 'a' && c <= 'z':
		d = uint64(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		d = uint64(c-'A') + 10
	default:
		return 0, false
	}
	return d, d < radix
}

func yamlIsNull(text string) bool {
	switch text {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

func yamlBool(text string) (bool, bool) {
	switch text {
	case "true", "True", "TRUE":
		return true, true
	case "false", "False", "FALSE":
		return false, true
	}
	return false, false
}

// yamlInt is an integer as YAML writes one (decimal with a sign, 0x hex or
// 0o octal): its sign and size, ok only when it is one and its size fits a
// u64. The Rust build read it as an i128; a valid number past a u64 failed
// there with the same error as no number.
func yamlInt(text string) (negative bool, size uint64, ok bool) {
	digits := text
	if rest, cut := strings.CutPrefix(text, "-"); cut {
		negative, digits = true, rest
	} else {
		digits = strings.TrimPrefix(text, "+")
	}
	radix := uint64(10)
	if rest, cut := strings.CutPrefix(digits, "0x"); cut {
		radix, digits = 16, rest
	} else if rest, cut := strings.CutPrefix(digits, "0o"); cut {
		radix, digits = 8, rest
	}
	if digits == "" || digits[0] == '+' || digits[0] == '-' {
		return false, 0, false
	}
	for i := 0; i < len(digits); i++ {
		d, valid := yamlDigit(digits[i], radix)
		if !valid || size > (^uint64(0)-d)/radix {
			return false, 0, false
		}
		size = size*radix + d
	}
	return negative, size, true
}

// yamlLocalConfig reads YAML settings text as the Rust build typed it.
func yamlLocalConfig(text string) (LocalConfig, error) {
	var c LocalConfig
	if err := yamlDecode(text, &c); err != nil {
		return LocalConfig{}, err
	}
	return c, nil
}

// yamlManifest reads a YAML workspace file as the Rust build typed it.
func yamlManifest(text string) (WorkspaceManifest, error) {
	var m WorkspaceManifest
	if err := yamlDecode(text, &m); err != nil {
		return WorkspaceManifest{}, err
	}
	return m, nil
}

// yamlDecode reads the settings in text into v, a pointer to a struct; a
// file with nothing in it holds no settings. Types are read as serde's
// derive read the Rust structs: a pointer is an Option (none for a plain
// ~, null or nothing), a field tagged toml:",required" must be there, one
// tagged toml:",default=true" is true when missing, a key that is no field
// is ignored, a string type with enumVariants takes only those words.
func yamlDecode(text string, v any) error {
	node, err := yamlParse(text)
	if err != nil {
		return err
	}
	if node == nil {
		node = &yamlNode{kind: yamlMap}
	}
	return yamlValue(node, reflect.ValueOf(v).Elem(), false)
}

// yamlPathFields are the settings the Rust structs kept as a PathBuf, which
// serde calls a "path string" in its errors (a String is "a string").
var yamlPathFields = map[string]bool{
	"LocalConfig.workspace_root":       true,
	"LocalConfig.scan_roots":           true,
	"LocalConfig.rules_file":           true,
	"GitIdentityProfile.path_prefixes": true,
	"CheckProfile.path_prefixes":       true,
	"WorkspaceManifest.workspace_root": true,
	"RepoEntry.path":                   true,
}

// yamlInvalid is serde's invalid_type error: what n is and what was
// expected; a scalar's names its line.
func yamlInvalid(n *yamlNode, expected string) error {
	found := "map"
	switch n.kind {
	case yamlScalar:
		return yamlRefuse(n.line, "invalid type: string "+rustDebug(n.text)+", expected "+expected)
	case yamlSeq:
		found = "sequence"
	}
	return errf("invalid type: %s, expected %s", found, expected)
}

func yamlValue(n *yamlNode, v reflect.Value, path bool) error {
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		if n.kind == yamlScalar && n.plain && yamlIsNull(n.text) {
			v.SetZero()
			return nil
		}
		elem := reflect.New(t.Elem())
		if err := yamlValue(n, elem.Elem(), path); err != nil {
			return err
		}
		v.Set(elem)
		return nil
	}
	if e, ok := v.Interface().(tomlEnum); ok && t.Kind() == reflect.String {
		if n.kind != yamlScalar {
			return yamlInvalid(n, "enum "+t.Name())
		}
		if !contains(e.enumVariants(), n.text) {
			return errf("unknown variant `%s`, expected %s", n.text, serdeOneOf(e.enumVariants()))
		}
		v.SetString(n.text)
		return nil
	}
	switch t.Kind() {
	case reflect.String:
		if n.kind != yamlScalar {
			if path {
				return yamlInvalid(n, "path string")
			}
			return yamlInvalid(n, "a string")
		}
		v.SetString(n.text)
	case reflect.Bool:
		b, ok := yamlBool(n.text)
		if n.kind != yamlScalar || !n.plain || !ok {
			return yamlInvalid(n, "a boolean")
		}
		v.SetBool(b)
	case reflect.Uint64:
		negative, size, ok := yamlInt(n.text)
		switch {
		case n.kind != yamlScalar || !n.plain || !ok:
		case !negative || size == 0:
			v.SetUint(size)
			return nil
		case size <= 1<<63:
			return errf("invalid value: integer `-%d`, expected u64", size)
		}
		return yamlInvalid(n, "u64")
	case reflect.Slice:
		if n.kind != yamlSeq {
			return yamlInvalid(n, "a sequence")
		}
		list := reflect.MakeSlice(t, len(n.items), len(n.items))
		for i, item := range n.items {
			if err := yamlValue(item, list.Index(i), path); err != nil {
				return err
			}
		}
		v.Set(list)
	case reflect.Map:
		if n.kind != yamlMap {
			return yamlInvalid(n, "a map")
		}
		m := reflect.MakeMapWithSize(t, len(n.entries))
		for _, e := range n.entries {
			elem := reflect.New(t.Elem()).Elem()
			if err := yamlValue(e.value, elem, false); err != nil {
				return err
			}
			m.SetMapIndex(reflect.ValueOf(e.key).Convert(t.Key()), elem)
		}
		v.Set(m)
	case reflect.Struct:
		return yamlStruct(n, v)
	default:
		panic(fmt.Sprintf("yaml.go cannot read a %s", t))
	}
	return nil
}

func yamlStruct(n *yamlNode, v reflect.Value) error {
	t := v.Type()
	if n.kind != yamlMap {
		return yamlInvalid(n, "struct "+t.Name())
	}
	fields := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _ := jsonTag(f)
		if name == "-" || !f.IsExported() {
			continue
		}
		fields[name] = i
		if strings.Contains(f.Tag.Get("toml"), "default=true") {
			v.Field(i).SetBool(true)
		}
	}
	seen := map[string]bool{}
	for _, e := range n.entries {
		i, ok := fields[e.key]
		if !ok {
			continue
		}
		seen[e.key] = true
		if err := yamlValue(e.value, v.Field(i), yamlPathFields[t.Name()+"."+e.key]); err != nil {
			return err
		}
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _ := jsonTag(f)
		if strings.Contains(f.Tag.Get("toml"), "required") && !seen[name] {
			return errf("missing field `%s`", name)
		}
	}
	return nil
}

// yamlFirstDroppedKey is the first key in node whose value is not none that
// kept (the typed settings as JSON values) lacks, as a dotted path: a
// setting a conversion would drop.
func yamlFirstDroppedKey(node *yamlNode, kept any) (string, bool) {
	if node == nil {
		return "", false
	}
	switch node.kind {
	case yamlMap:
		object, ok := kept.(map[string]any)
		if !ok {
			return "", false
		}
		for _, e := range node.entries {
			if e.value.kind == yamlScalar && e.value.plain && yamlIsNull(e.value.text) {
				continue
			}
			inner, ok := object[e.key]
			if !ok {
				return e.key, true
			}
			if deeper, ok := yamlFirstDroppedKey(e.value, inner); ok {
				return e.key + "." + deeper, true
			}
		}
	case yamlSeq:
		list, ok := kept.([]any)
		if !ok {
			return "", false
		}
		for i, item := range node.items {
			if i >= len(list) {
				break
			}
			if deeper, ok := yamlFirstDroppedKey(item, list[i]); ok {
				return fmt.Sprintf("%d.%s", i, deeper), true
			}
		}
	}
	return "", false
}
