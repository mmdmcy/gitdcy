package main

// Text helpers that behave as Rust's str methods did, so output stays the
// same byte for byte.

import (
	"strconv"
	"strings"
	"unicode"
)

// trim is Rust's str::trim (Unicode White_Space on both ends).
func trim(s string) string { return strings.TrimSpace(s) }

// lines is Rust's str::lines: split at "\n", a trailing "\r" dropped from
// each line, and no empty last line for text that ends with "\n".
func lines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

// asciiLower is Rust's to_ascii_lowercase: only A-Z change.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// isASCIIAlnum is Rust's char::is_ascii_alphanumeric.
func isASCIIAlnum(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// rustDebug is a string as Rust's {:?} showed it: in double quotes, with
// \t \r \n \\ \" \0 and \u{..} for characters that do not print.
func rustDebug(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch r {
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case 0:
			b.WriteString(`\0`)
		default:
			if r == unicode.ReplacementChar && strings.HasPrefix(s[i:], "\xef\xbf\xbd") || unicode.IsPrint(r) && !(i == 0 && unicode.Is(unicode.Mn, r)) {
				b.WriteRune(r)
			} else {
				b.WriteString(`\u{` + strconv.FormatInt(int64(r), 16) + `}`)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// splitOnce is Rust's str::split_once.
func splitOnce(s, sep string) (string, string, bool) {
	i := strings.Index(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// rsplitOnce is Rust's str::rsplit_once.
func rsplitOnce(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// plural is "" for one, else "s".
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
