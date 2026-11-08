//! A reader for the YAML GitDCY kept its settings in before TOML: the block
//! style its own YAML writer produced, and the same written by hand (comments,
//! quoted keys and values, one-line `[a, b]` lists, `[]` and `{}`). Anything
//! else (anchors, aliases, tags, block scalars, flow mappings, several
//! documents, tabs, a quoted value over several lines, a repeated key) is
//! refused with its line, never guessed. It only reads: GitDCY writes TOML.
//!
//! Values come out as the YAML writer's reader gave them: a string setting
//! takes any scalar's text as written, `~`, `null` and nothing are "none" for
//! an optional setting, and `true`/`false`, numbers and names are only what
//! their settings take.

use serde::de::{self, DeserializeOwned, IntoDeserializer, Unexpected, Visitor};
use std::fmt;

#[derive(Debug, Clone, PartialEq)]
pub(crate) enum Node {
    Scalar {
        text: String,
        /// Written without quotes: only these can be none, true or a number.
        plain: bool,
        line: usize,
    },
    Seq(Vec<Node>),
    Map(Vec<(String, Node)>),
}

#[derive(Debug)]
pub(crate) struct YamlError(String);

impl fmt::Display for YamlError {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for YamlError {}

impl de::Error for YamlError {
    fn custom<T: fmt::Display>(message: T) -> Self {
        YamlError(message.to_string())
    }
}

fn refuse<T>(line: usize, what: impl fmt::Display) -> Result<T, YamlError> {
    Err(YamlError(format!("line {line}: {what}")))
}

/// The settings in `text`, typed; a file with nothing in it (or only
/// comments) holds no settings.
pub(crate) fn from_str<T: DeserializeOwned>(text: &str) -> Result<T, YamlError> {
    let node = parse(text)?.unwrap_or(Node::Map(Vec::new()));
    T::deserialize(De(&node))
}

/// The tree in `text`; None for a document with nothing in it.
pub(crate) fn parse(text: &str) -> Result<Option<Node>, YamlError> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let mut lines = Vec::new();
    for (index, raw) in text.lines().enumerate() {
        let number = index + 1;
        let content = raw.trim_start_matches([' ', '\t']);
        let text = content.trim_end_matches([' ', '\t']);
        if text.is_empty() || text.starts_with('#') {
            continue;
        }
        let lead = &raw[..raw.len() - content.len()];
        if lead.contains('\t') {
            return refuse(number, "a tab in the indentation");
        }
        if lead.is_empty() && (text == "---" || text == "...") {
            if text == "---" && lines.is_empty() {
                continue;
            }
            return refuse(number, "a second document");
        }
        if lead.is_empty() && (text.starts_with("--- ") || text.starts_with("... ")) {
            return refuse(number, "a document marker with something after it");
        }
        if lead.is_empty() && text.starts_with('%') {
            return refuse(number, "a YAML directive");
        }
        lines.push(Line {
            number,
            indent: lead.len(),
            text,
        });
    }
    let mut parser = Parser { lines, at: 0 };
    let Some(first) = parser.lines.first() else {
        return Ok(None);
    };
    let indent = first.indent;
    let node = parser.block(indent)?;
    if let Some(line) = parser.peek() {
        return refuse(line.number, "unexpected indentation");
    }
    Ok(Some(node))
}

#[derive(Clone, Copy)]
struct Line<'a> {
    number: usize,
    indent: usize,
    text: &'a str,
}

struct Parser<'a> {
    lines: Vec<Line<'a>>,
    at: usize,
}

fn is_item(text: &str) -> bool {
    text == "-" || text.starts_with("- ")
}

impl<'a> Parser<'a> {
    fn peek(&self) -> Option<Line<'a>> {
        self.lines.get(self.at).copied()
    }

    fn block(&mut self, indent: usize) -> Result<Node, YamlError> {
        match self.peek() {
            Some(line) if is_item(line.text) => self.seq(indent),
            _ => self.map(indent),
        }
    }

    /// What follows `key:` or `-` with nothing after it on its line: a
    /// deeper block, a list at the key's own indentation (as the YAML writer
    /// put lists), or nothing.
    fn nested(
        &mut self,
        indent: usize,
        number: usize,
        same_indent_list: bool,
    ) -> Result<Node, YamlError> {
        match self.peek() {
            Some(next) if next.indent > indent => self.block(next.indent),
            Some(next) if same_indent_list && next.indent == indent && is_item(next.text) => {
                self.seq(indent)
            }
            _ => Ok(Node::Scalar {
                text: String::new(),
                plain: true,
                line: number,
            }),
        }
    }

    fn seq(&mut self, indent: usize) -> Result<Node, YamlError> {
        let mut items = Vec::new();
        while let Some(line) = self.peek() {
            if line.indent < indent || (line.indent == indent && !is_item(line.text)) {
                break;
            }
            if line.indent > indent {
                return refuse(line.number, "unexpected indentation");
            }
            let rest = line.text[1..].trim_start_matches(' ');
            if rest.is_empty() {
                self.at += 1;
                items.push(self.nested(indent, line.number, false)?);
            } else if is_item(rest) {
                return refuse(line.number, "a list inside a list on one line");
            } else if split_key(rest, line.number).is_ok() {
                // A mapping that starts on the item's line, at its column.
                let column = indent + (line.text.len() - rest.len());
                self.lines[self.at] = Line {
                    indent: column,
                    text: rest,
                    ..line
                };
                items.push(self.map(column)?);
            } else {
                self.at += 1;
                items.push(inline(rest, line.number)?);
                self.no_deeper(indent)?;
            }
        }
        Ok(Node::Seq(items))
    }

    fn map(&mut self, indent: usize) -> Result<Node, YamlError> {
        let mut entries: Vec<(String, Node)> = Vec::new();
        while let Some(line) = self.peek() {
            if line.indent < indent {
                break;
            }
            if line.indent > indent || is_item(line.text) {
                return refuse(line.number, "unexpected indentation");
            }
            let (key, rest) = split_key(line.text, line.number)?;
            if entries.iter().any(|(known, _)| *known == key) {
                return refuse(line.number, format!("the key {key:?} a second time"));
            }
            self.at += 1;
            let value = if rest.is_empty() || rest.starts_with('#') {
                self.nested(indent, line.number, true)?
            } else {
                let value = inline(rest, line.number)?;
                self.no_deeper(indent)?;
                value
            };
            entries.push((key, value));
        }
        Ok(Node::Map(entries))
    }

    fn no_deeper(&self, indent: usize) -> Result<(), YamlError> {
        match self.peek() {
            Some(next) if next.indent > indent => refuse(next.number, "unexpected indentation"),
            _ => Ok(()),
        }
    }
}

/// `key: rest` -> the key and what follows the colon.
fn split_key(text: &str, line: usize) -> Result<(String, &str), YamlError> {
    let (key, after) = if text.starts_with(['"', '\'']) {
        let (key, used) = quoted(text, line)?;
        (key, &text[used..])
    } else {
        let colon = text
            .char_indices()
            .find(|&(i, c)| {
                c == ':'
                    && text[i + 1..]
                        .chars()
                        .next()
                        .is_none_or(|n| n == ' ' || n == '\t')
            })
            .map(|(i, _)| i);
        let Some(colon) = colon else {
            return refuse(line, "expected a key and a colon");
        };
        let key = text[..colon].trim_end_matches([' ', '\t']);
        if key.is_empty() || !plain_start(key) || key.contains(" #") {
            return refuse(line, format!("a key GitDCY does not read: {key:?}"));
        }
        (key.to_string(), &text[colon..])
    };
    let Some(rest) = after.strip_prefix(':') else {
        return refuse(line, "expected a colon after the key");
    };
    if !(rest.is_empty() || rest.starts_with([' ', '\t'])) {
        return refuse(line, "expected a space after the colon");
    }
    Ok((key, rest.trim_start_matches([' ', '\t'])))
}

/// Whether unquoted text may start this way: not with a YAML indicator, but
/// `-`, `?` and `:` may lead when something other than a space follows
/// (`--all`, `-D`).
fn plain_start(text: &str) -> bool {
    let mut chars = text.chars();
    match chars.next() {
        None => false,
        Some('-' | '?' | ':') => chars.next().is_some_and(|c| c != ' ' && c != '\t'),
        Some(c) => !matches!(
            c,
            ',' | '['
                | ']'
                | '{'
                | '}'
                | '#'
                | '&'
                | '*'
                | '!'
                | '|'
                | '>'
                | '\''
                | '"'
                | '%'
                | '@'
                | '`'
        ),
    }
}

/// After a value: nothing, or a comment.
fn only_comment(rest: &str, line: usize) -> Result<(), YamlError> {
    let trimmed = rest.trim_start_matches([' ', '\t']);
    if trimmed.is_empty() || (trimmed.starts_with('#') && trimmed.len() < rest.len()) {
        Ok(())
    } else {
        refuse(line, format!("unexpected text after a value: {trimmed:?}"))
    }
}

/// A value on the line of its key or `-`.
fn inline(text: &str, line: usize) -> Result<Node, YamlError> {
    if text.starts_with(['"', '\'']) {
        let (value, used) = quoted(text, line)?;
        only_comment(&text[used..], line)?;
        return Ok(Node::Scalar {
            text: value,
            plain: false,
            line,
        });
    }
    if let Some(inner) = text.strip_prefix('[') {
        return flow_list(inner, line);
    }
    if let Some(after) = text.strip_prefix("{}") {
        only_comment(after, line)?;
        return Ok(Node::Map(Vec::new()));
    }
    match text.chars().next() {
        Some('&') => return refuse(line, "an anchor (&)"),
        Some('*') => return refuse(line, "an alias (*)"),
        Some('!') => return refuse(line, "a tag (!)"),
        Some('|' | '>') => return refuse(line, "a value over several lines (| or >)"),
        Some('{') => return refuse(line, "a mapping written on one line ({...})"),
        Some('-') if is_item(text) => return refuse(line, "a list on the line of its key"),
        _ if !plain_start(text) => {
            return refuse(line, format!("a value GitDCY does not read: {text:?}"));
        }
        _ => {}
    }
    Ok(Node::Scalar {
        text: plain(text, line)?.to_string(),
        plain: true,
        line,
    })
}

/// An unquoted value: up to a comment, not holding ": ".
fn plain(text: &str, line: usize) -> Result<&str, YamlError> {
    let end = text
        .char_indices()
        .find(|&(i, c)| c == '#' && i > 0 && text[..i].ends_with([' ', '\t']))
        .map_or(text.len(), |(i, _)| i);
    let value = text[..end].trim_end_matches([' ', '\t']);
    if value.contains(": ") || value.contains(":\t") || value.ends_with(':') {
        return refuse(line, format!("a colon inside a value; quote it: {value:?}"));
    }
    Ok(value)
}

/// `[a, 'b', "c"]` after its `[`.
fn flow_list(inner: &str, line: usize) -> Result<Node, YamlError> {
    let mut items = Vec::new();
    let mut rest = inner.trim_start_matches([' ', '\t']);
    if let Some(after) = rest.strip_prefix(']') {
        only_comment(after, line)?;
        return Ok(Node::Seq(items));
    }
    loop {
        let (item, used) = if rest.starts_with(['"', '\'']) {
            let (value, used) = quoted(rest, line)?;
            (
                Node::Scalar {
                    text: value,
                    plain: false,
                    line,
                },
                used,
            )
        } else {
            let end = rest.find([',', ']']).unwrap_or(rest.len());
            let text = rest[..end].trim_end_matches([' ', '\t']);
            if !plain_start(text) || text.contains(['[', '{', '}']) || text.contains(" #") {
                return refuse(line, format!("a list item GitDCY does not read: {text:?}"));
            }
            let text = plain(text, line)?;
            (
                Node::Scalar {
                    text: text.to_string(),
                    plain: true,
                    line,
                },
                end,
            )
        };
        items.push(item);
        rest = rest[used..].trim_start_matches([' ', '\t']);
        if let Some(after) = rest.strip_prefix(']') {
            only_comment(after, line)?;
            return Ok(Node::Seq(items));
        }
        match rest.strip_prefix(',') {
            Some(after) => rest = after.trim_start_matches([' ', '\t']),
            None => return refuse(line, "a list written on one line must end with ]"),
        }
        if rest.starts_with(']') {
            return refuse(line, "an empty item in a list");
        }
    }
}

/// A quoted value at the start of `text`: its text and the bytes it used.
fn quoted(text: &str, line: usize) -> Result<(String, usize), YamlError> {
    let mut out = String::new();
    let mut chars = text.char_indices();
    let (_, quote) = chars.next().expect("starts with a quote");
    while let Some((i, c)) = chars.next() {
        if quote == '\'' {
            if c == '\'' {
                if text[i + 1..].starts_with('\'') {
                    chars.next();
                    out.push('\'');
                    continue;
                }
                return Ok((out, i + 1));
            }
            out.push(c);
            continue;
        }
        match c {
            '"' => return Ok((out, i + 1)),
            '\\' => {
                let Some((_, escape)) = chars.next() else {
                    break;
                };
                let simple = match escape {
                    '0' => Some('\0'),
                    'a' => Some('\u{7}'),
                    'b' => Some('\u{8}'),
                    't' | '\t' => Some('\t'),
                    'n' => Some('\n'),
                    'v' => Some('\u{b}'),
                    'f' => Some('\u{c}'),
                    'r' => Some('\r'),
                    'e' => Some('\u{1b}'),
                    ' ' => Some(' '),
                    '"' => Some('"'),
                    '/' => Some('/'),
                    '\\' => Some('\\'),
                    'N' => Some('\u{85}'),
                    '_' => Some('\u{a0}'),
                    'L' => Some('\u{2028}'),
                    'P' => Some('\u{2029}'),
                    _ => None,
                };
                if let Some(c) = simple {
                    out.push(c);
                    continue;
                }
                let digits = match escape {
                    'x' => 2,
                    'u' => 4,
                    'U' => 8,
                    _ => return refuse(line, format!("the escape \\{escape}")),
                };
                let hex: String = (0..digits)
                    .filter_map(|_| chars.next().map(|(_, c)| c))
                    .collect();
                let code = (hex.len() == digits)
                    .then(|| u32::from_str_radix(&hex, 16).ok())
                    .flatten()
                    .and_then(char::from_u32);
                match code {
                    Some(c) => out.push(c),
                    None => return refuse(line, format!("the escape \\{escape}{hex}")),
                }
            }
            c => out.push(c),
        }
    }
    refuse(line, "a quoted value that does not end on its line")
}

fn is_null(text: &str) -> bool {
    matches!(text, "" | "~" | "null" | "Null" | "NULL")
}

fn parse_bool(text: &str) -> Option<bool> {
    match text {
        "true" | "True" | "TRUE" => Some(true),
        "false" | "False" | "FALSE" => Some(false),
        _ => None,
    }
}

/// An integer as YAML writes one: decimal with a sign, 0x hex or 0o octal.
fn parse_int(text: &str) -> Option<i128> {
    let (negative, digits) = match text.strip_prefix('-') {
        Some(rest) => (true, rest),
        None => (false, text.strip_prefix('+').unwrap_or(text)),
    };
    let (radix, digits) = if let Some(rest) = digits.strip_prefix("0x") {
        (16, rest)
    } else if let Some(rest) = digits.strip_prefix("0o") {
        (8, rest)
    } else {
        (10, digits)
    };
    if digits.is_empty() || digits.starts_with(['+', '-']) {
        return None;
    }
    let value = i128::from_str_radix(digits, radix).ok()?;
    Some(if negative { -value } else { value })
}

struct De<'a>(&'a Node);

impl De<'_> {
    fn unexpected(&self) -> Unexpected<'_> {
        match self.0 {
            Node::Scalar { text, .. } => Unexpected::Str(text),
            Node::Seq(_) => Unexpected::Seq,
            Node::Map(_) => Unexpected::Map,
        }
    }

    fn invalid(&self, expected: &dyn de::Expected) -> YamlError {
        let error = de::Error::invalid_type(self.unexpected(), expected);
        match self.0 {
            Node::Scalar { line, .. } => YamlError(format!("line {line}: {error}")),
            _ => error,
        }
    }

    fn plain(&self) -> Option<&str> {
        match self.0 {
            Node::Scalar {
                text, plain: true, ..
            } => Some(text),
            _ => None,
        }
    }

    fn int<'de, V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.plain().and_then(parse_int) {
            Some(value) if value >= 0 => match u64::try_from(value) {
                Ok(value) => visitor.visit_u64(value),
                Err(_) => Err(self.invalid(&visitor)),
            },
            Some(value) => match i64::try_from(value) {
                Ok(value) => visitor.visit_i64(value),
                Err(_) => Err(self.invalid(&visitor)),
            },
            None => Err(self.invalid(&visitor)),
        }
    }
}

macro_rules! ints {
    ($($method:ident)*) => {$(
        fn $method<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
            self.int(visitor)
        }
    )*};
}

impl<'de> de::Deserializer<'de> for De<'_> {
    type Error = YamlError;

    fn deserialize_any<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.0 {
            Node::Scalar {
                text, plain: true, ..
            } => {
                if is_null(text) {
                    visitor.visit_unit()
                } else if let Some(value) = parse_bool(text) {
                    visitor.visit_bool(value)
                } else if parse_int(text).is_some() {
                    self.int(visitor)
                } else {
                    visitor.visit_str(text)
                }
            }
            Node::Scalar { text, .. } => visitor.visit_str(text),
            Node::Seq(items) => visitor.visit_seq(Items(items.iter())),
            Node::Map(entries) => visitor.visit_map(Entries {
                entries: entries.iter(),
                value: None,
            }),
        }
    }

    fn deserialize_bool<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.plain().and_then(parse_bool) {
            Some(value) => visitor.visit_bool(value),
            None => Err(self.invalid(&visitor)),
        }
    }

    ints!(deserialize_i8 deserialize_i16 deserialize_i32 deserialize_i64 deserialize_i128
          deserialize_u8 deserialize_u16 deserialize_u32 deserialize_u64 deserialize_u128);

    fn deserialize_f32<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        self.deserialize_f64(visitor)
    }

    fn deserialize_f64<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.plain().and_then(|text| text.parse::<f64>().ok()) {
            Some(value) => visitor.visit_f64(value),
            None => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_char<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        self.deserialize_str(visitor)
    }

    fn deserialize_str<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.0 {
            Node::Scalar { text, .. } => visitor.visit_str(text),
            _ => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_string<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        self.deserialize_str(visitor)
    }

    fn deserialize_bytes<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        Err(self.invalid(&visitor))
    }

    fn deserialize_byte_buf<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        Err(self.invalid(&visitor))
    }

    fn deserialize_option<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.plain() {
            Some(text) if is_null(text) => visitor.visit_none(),
            _ => visitor.visit_some(self),
        }
    }

    fn deserialize_unit<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.plain() {
            Some(text) if is_null(text) => visitor.visit_unit(),
            _ => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_unit_struct<V: Visitor<'de>>(
        self,
        _name: &'static str,
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        self.deserialize_unit(visitor)
    }

    fn deserialize_newtype_struct<V: Visitor<'de>>(
        self,
        _name: &'static str,
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        visitor.visit_newtype_struct(self)
    }

    fn deserialize_seq<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.0 {
            Node::Seq(items) => visitor.visit_seq(Items(items.iter())),
            _ => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_tuple<V: Visitor<'de>>(
        self,
        _len: usize,
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        self.deserialize_seq(visitor)
    }

    fn deserialize_tuple_struct<V: Visitor<'de>>(
        self,
        _name: &'static str,
        _len: usize,
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        self.deserialize_seq(visitor)
    }

    fn deserialize_map<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        match self.0 {
            Node::Map(entries) => visitor.visit_map(Entries {
                entries: entries.iter(),
                value: None,
            }),
            _ => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_struct<V: Visitor<'de>>(
        self,
        _name: &'static str,
        _fields: &'static [&'static str],
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        self.deserialize_map(visitor)
    }

    fn deserialize_enum<V: Visitor<'de>>(
        self,
        _name: &'static str,
        _variants: &'static [&'static str],
        visitor: V,
    ) -> Result<V::Value, YamlError> {
        match self.0 {
            Node::Scalar { text, .. } => visitor.visit_enum(
                IntoDeserializer::<YamlError>::into_deserializer(text.as_str()),
            ),
            _ => Err(self.invalid(&visitor)),
        }
    }

    fn deserialize_identifier<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        self.deserialize_str(visitor)
    }

    fn deserialize_ignored_any<V: Visitor<'de>>(self, visitor: V) -> Result<V::Value, YamlError> {
        visitor.visit_unit()
    }
}

struct Items<'a>(std::slice::Iter<'a, Node>);

impl<'de> de::SeqAccess<'de> for Items<'_> {
    type Error = YamlError;

    fn next_element_seed<T: de::DeserializeSeed<'de>>(
        &mut self,
        seed: T,
    ) -> Result<Option<T::Value>, YamlError> {
        self.0
            .next()
            .map(|node| seed.deserialize(De(node)))
            .transpose()
    }
}

struct Entries<'a> {
    entries: std::slice::Iter<'a, (String, Node)>,
    value: Option<&'a Node>,
}

impl<'de> de::MapAccess<'de> for Entries<'_> {
    type Error = YamlError;

    fn next_key_seed<K: de::DeserializeSeed<'de>>(
        &mut self,
        seed: K,
    ) -> Result<Option<K::Value>, YamlError> {
        match self.entries.next() {
            Some((key, value)) => {
                self.value = Some(value);
                seed.deserialize(IntoDeserializer::<YamlError>::into_deserializer(
                    key.as_str(),
                ))
                .map(Some)
            }
            None => Ok(None),
        }
    }

    fn next_value_seed<V: de::DeserializeSeed<'de>>(
        &mut self,
        seed: V,
    ) -> Result<V::Value, YamlError> {
        let value = self.value.take().expect("a value follows its key");
        seed.deserialize(De(value))
    }
}

/// The first key in `node` whose value is not none that `kept` lacks (the
/// same tree as JSON, as a value serializes): a setting a conversion would
/// drop.
pub(crate) fn first_dropped_key(node: &Node, kept: &serde_json::Value) -> Option<String> {
    match (node, kept) {
        (Node::Map(entries), serde_json::Value::Object(kept)) => {
            entries.iter().find_map(|(key, value)| {
                if matches!(value, Node::Scalar { text, plain: true, .. } if is_null(text)) {
                    return None;
                }
                match kept.get(key) {
                    None => Some(key.clone()),
                    Some(inner) => {
                        first_dropped_key(value, inner).map(|deeper| format!("{key}.{deeper}"))
                    }
                }
            })
        }
        (Node::Seq(items), serde_json::Value::Array(kept)) => items
            .iter()
            .zip(kept)
            .enumerate()
            .find_map(|(i, (item, kept))| {
                first_dropped_key(item, kept).map(|deeper| format!("{i}.{deeper}"))
            }),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scalar(text: &str, plain: bool) -> Node {
        Node::Scalar {
            text: text.to_string(),
            plain,
            line: 0,
        }
    }

    /// The tree with lines zeroed, to compare shapes.
    fn shape(node: Node) -> Node {
        match node {
            Node::Scalar { text, plain, .. } => scalar(&text, plain),
            Node::Seq(items) => Node::Seq(items.into_iter().map(shape).collect()),
            Node::Map(entries) => {
                Node::Map(entries.into_iter().map(|(k, v)| (k, shape(v))).collect())
            }
        }
    }

    fn tree(text: &str) -> Node {
        shape(parse(text).unwrap().unwrap())
    }

    #[test]
    fn reads_the_block_style_and_what_people_write_by_hand() {
        let text = "\
# a comment
workspace_root: /home/u/Code   # trailing comment
'*': [a, 'b c', \"d\\te\"]
empty_list: []
empty_map: {}
nothing:
tilde: ~
quoted: 'it''s'
url: https://example.com/a#b
repos:
- id: github/app
  enabled: true
  remotes:
  - origin
- id: other
indented:
  - one
  -
    deep: value
";
        assert_eq!(
            tree(text),
            Node::Map(vec![
                ("workspace_root".into(), scalar("/home/u/Code", true)),
                (
                    "*".into(),
                    Node::Seq(vec![
                        scalar("a", true),
                        scalar("b c", false),
                        scalar("d\te", false)
                    ])
                ),
                ("empty_list".into(), Node::Seq(vec![])),
                ("empty_map".into(), Node::Map(vec![])),
                ("nothing".into(), scalar("", true)),
                ("tilde".into(), scalar("~", true)),
                ("quoted".into(), scalar("it's", false)),
                ("url".into(), scalar("https://example.com/a#b", true)),
                (
                    "repos".into(),
                    Node::Seq(vec![
                        Node::Map(vec![
                            ("id".into(), scalar("github/app", true)),
                            ("enabled".into(), scalar("true", true)),
                            ("remotes".into(), Node::Seq(vec![scalar("origin", true)])),
                        ]),
                        Node::Map(vec![("id".into(), scalar("other", true))]),
                    ])
                ),
                (
                    "indented".into(),
                    Node::Seq(vec![
                        scalar("one", true),
                        Node::Map(vec![("deep".into(), scalar("value", true))]),
                    ])
                ),
            ])
        );
        assert_eq!(parse("# only comments\n\n").unwrap(), None);
        assert_eq!(
            tree("---\na: 1\n"),
            Node::Map(vec![("a".into(), scalar("1", true))])
        );
        assert_eq!(
            tree("a: \"\\u00e9\\x41\\\\ \\\"\"\n"),
            Node::Map(vec![("a".into(), scalar("éA\\ \"", false))])
        );
    }

    #[test]
    fn refuses_what_it_does_not_read() {
        for (text, why) in [
            ("a: &x 1\nb: *x\n", "anchor"),
            ("a: *x\n", "alias"),
            ("a: !!str 1\n", "tag"),
            ("a: |\n  text\n", "several lines"),
            ("a: >\n  text\n", "several lines"),
            ("a: {b: 1}\n", "one line"),
            ("a: 1\n---\nb: 2\n", "second document"),
            ("a:\n\tb: 1\n", "tab"),
            ("a: 'open\n", "does not end"),
            ("a: 1\na: 2\n", "second time"),
            ("a: 1\n  b: 2\n", "indentation"),
            ("a: b: c\n", "colon"),
            ("just text\n", "key"),
            ("a: [b, [c]]\n", "list item"),
            ("a: \"\\q\"\n", "escape"),
            ("%YAML 1.2\na: 1\n", "directive"),
            ("- - a\n", "list inside a list"),
            ("a: - b\n", "list on the line"),
        ] {
            let error = parse(text).unwrap_err().to_string();
            assert!(error.contains(why), "{text:?}: {error}");
            assert!(error.starts_with("line "), "{text:?}: {error}");
        }
    }

    #[derive(Debug, PartialEq, serde::Deserialize)]
    struct Settings {
        name: String,
        count: Option<u64>,
        on: bool,
        missing: Option<String>,
        list: Vec<String>,
        #[serde(default)]
        kind: Option<Kind>,
    }

    #[derive(Debug, PartialEq, serde::Deserialize)]
    #[serde(rename_all = "lowercase")]
    enum Kind {
        Github,
    }

    #[test]
    fn typed_values_are_read_as_the_yaml_writer_s_reader_read_them() {
        let settings: Settings = from_str(
            "name: 123\ncount: 0x10\non: True\nmissing: ~\nlist: [~, 'x']\nkind: github\n",
        )
        .unwrap();
        assert_eq!(
            settings,
            Settings {
                name: "123".into(),
                count: Some(16),
                on: true,
                missing: None,
                list: vec!["~".into(), "x".into()],
                kind: Some(Kind::Github),
            }
        );
        // A quoted null is text; yes is not a bool.
        let quoted: Settings =
            from_str("name: x\ncount:\non: false\nmissing: 'null'\nlist: []\n").unwrap();
        assert_eq!(quoted.missing.as_deref(), Some("null"));
        assert!(from_str::<Settings>("name: x\non: yes\nlist: []\n").is_err());
        assert!(from_str::<Settings>("name: x\non: true\nlist: []\ncount: '5'\n").is_err());
    }

    #[test]
    fn reads_back_what_serde_norway_wrote() {
        use crate::{
            CheckCommand, CheckProfile, GitIdentityProfile, LocalConfig, Provider, RepoEntry,
            VisibilityOverride, WorkspaceManifest,
        };
        use std::collections::BTreeMap;
        use std::path::PathBuf;
        fn json<T: serde::Serialize>(value: &T) -> serde_json::Value {
            serde_json::to_value(value).unwrap()
        }
        let local = LocalConfig {
            workspace_root: Some(PathBuf::from("~/Code")),
            scan_roots: Some(vec![
                PathBuf::from("/home/u/Documents/github"),
                PathBuf::from(" lead"),
            ]),
            sync_remote_template: Some("ssh://git@example.internal/o/{repo}.git".into()),
            origin_remote_templates: Some(BTreeMap::from([
                ("github".into(), "true".into()),
                ("gitlab".into(), "a: b # c".into()),
            ])),
            local_sync_files: Some(BTreeMap::from([(
                "github/app".into(),
                vec![".env".into(), "-D".into()],
            )])),
            trusted_wip_devices: Some(BTreeMap::from([(
                "*".into(),
                vec!["laptop".into(), "123".into()],
            )])),
            visibility_overrides: Some(BTreeMap::from([(
                "github/private".into(),
                VisibilityOverride::Private,
            )])),
            private_remote_patterns: Some(vec![]),
            public_export_remotes: Some(vec!["~".into(), "'q'".into(), "é\t\u{7}".into()]),
            ignore_profiles: Some(BTreeMap::new()),
            require_identity: Some(true),
            identity_profiles: Some(BTreeMap::from([(
                "main".into(),
                GitIdentityProfile {
                    name: "A \"B\"".into(),
                    email: "a@b.c".into(),
                    path_prefixes: vec![PathBuf::from("~/x")],
                    remote_patterns: vec![],
                    providers: vec![Provider::Forgejo],
                },
            )])),
            require_checks: None,
            check_profiles: Some(BTreeMap::from([(
                "rust".into(),
                CheckProfile {
                    path_prefixes: vec![],
                    remote_patterns: vec!["github.com/o/a".into()],
                    providers: vec![],
                    checks: vec![CheckCommand {
                        name: "clippy".into(),
                        program: "cargo".into(),
                        args: vec!["--".into(), "-D".into(), "warnings".into()],
                        timeout_seconds: Some(900),
                    }],
                    run_before_push: false,
                    require_clean_worktree: true,
                },
            )])),
            rules_file: Some(PathBuf::from("/home/u/fleet/repos.toml")),
            config_error: None,
        };
        let manifest = WorkspaceManifest {
            workspace_root: PathBuf::from("/home/u/Code"),
            repos: vec![
                RepoEntry {
                    id: "github/app".into(),
                    path: PathBuf::from("/home/u/Code/github/app"),
                    provider: Provider::Github,
                    enabled: true,
                    primary_remote: Some("origin".into()),
                    wip_sync: false,
                    review_required: true,
                },
                RepoEntry {
                    id: "other/x: y".into(),
                    path: PathBuf::from("/home/u/Code/x"),
                    provider: Provider::Other,
                    enabled: false,
                    primary_remote: None,
                    wip_sync: true,
                    review_required: false,
                },
            ],
        };
        let read: LocalConfig = from_str(include_str!("../tests/yaml/local.yaml")).unwrap();
        assert_eq!(json(&read), json(&local));
        let read: WorkspaceManifest =
            from_str(include_str!("../tests/yaml/workspace.yaml")).unwrap();
        assert_eq!(json(&read), json(&manifest));
        let read: LocalConfig = from_str(include_str!("../tests/yaml/empty.yaml")).unwrap();
        assert_eq!(json(&read), json(&LocalConfig::default()));
    }
}
