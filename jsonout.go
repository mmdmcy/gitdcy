package main

// JSON as serde_json's to_string_pretty wrote it: two-space indent, struct
// fields in declaration order, map keys sorted, the same string escapes
// (\" \\ \b \f \n \r \t, \u00XX for other control characters, nothing else
// escaped). A nil slice is [] (a Rust Vec is never null); a nil pointer or
// interface is null. Field names come from the `json` tag; ",omitempty"
// leaves out a nil pointer or interface, an empty string, slice or map
// (serde's skip_serializing_if), never a false or a 0. A type with a
// jsonValue method is written as the value it returns.

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// jsonValuer is a type that is written as another value.
type jsonValuer interface{ jsonValue() any }

// jsonPretty is v as pretty JSON, without a final newline.
func jsonPretty(v any) string {
	var b strings.Builder
	writeJSON(&b, reflect.ValueOf(v), 0)
	return b.String()
}

func writeJSON(b *strings.Builder, v reflect.Value, depth int) {
	if !v.IsValid() {
		b.WriteString("null")
		return
	}
	if v.Kind() != reflect.Pointer && v.Kind() != reflect.Interface && v.CanInterface() {
		if jv, ok := v.Interface().(jsonValuer); ok {
			writeJSON(b, reflect.ValueOf(jv.jsonValue()), depth)
			return
		}
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			b.WriteString("null")
			return
		}
		writeJSON(b, v.Elem(), depth)
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		b.WriteString(jsonFloat(v.Float()))
	case reflect.String:
		writeJSONString(b, v.String())
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteString(",\n")
			}
			indent(b, depth+1)
			writeJSON(b, v.Index(i), depth+1)
		}
		b.WriteString("\n")
		indent(b, depth)
		b.WriteString("]")
	case reflect.Map:
		if v.Len() == 0 {
			b.WriteString("{}")
			return
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		b.WriteString("{\n")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",\n")
			}
			indent(b, depth+1)
			writeJSONString(b, k.String())
			b.WriteString(": ")
			writeJSON(b, v.MapIndex(k), depth+1)
		}
		b.WriteString("\n")
		indent(b, depth)
		b.WriteString("}")
	case reflect.Struct:
		type field struct {
			name  string
			value reflect.Value
		}
		var fields []field
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() && f.Tag.Get("json") == "" {
				continue
			}
			name, omitEmpty := jsonTag(f)
			if name == "-" {
				continue
			}
			fv := v.Field(i)
			if omitEmpty && emptyForJSON(fv) {
				continue
			}
			fields = append(fields, field{name, fv})
		}
		if len(fields) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, f := range fields {
			if i > 0 {
				b.WriteString(",\n")
			}
			indent(b, depth+1)
			writeJSONString(b, f.name)
			b.WriteString(": ")
			writeJSON(b, f.value, depth+1)
		}
		b.WriteString("\n")
		indent(b, depth)
		b.WriteString("}")
	default:
		panic(fmt.Sprintf("jsonPretty: cannot write a %s", v.Kind()))
	}
}

// jsonTag is a field's JSON name and whether ",omitempty" is set.
func jsonTag(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, opts == "omitempty" || strings.Contains(","+opts+",", ",omitempty,")
}

func emptyForJSON(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() == 0
	}
	if v.CanInterface() {
		if z, ok := v.Interface().(interface{ isEmpty() bool }); ok {
			return z.isEmpty()
		}
	}
	return false
}

func indent(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
}

// writeJSONString writes s with serde_json's escapes.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var esc string
		switch {
		case c == '"':
			esc = `\"`
		case c == '\\':
			esc = `\\`
		case c == '\b':
			esc = `\b`
		case c == '\f':
			esc = `\f`
		case c == '\n':
			esc = `\n`
		case c == '\r':
			esc = `\r`
		case c == '\t':
			esc = `\t`
		case c < 0x20:
			esc = fmt.Sprintf(`\u%04x`, c)
		default:
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(esc)
		start = i + 1
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// jsonFloat is a float as serde_json wrote it (always with a fraction or an
// exponent).
func jsonFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}
