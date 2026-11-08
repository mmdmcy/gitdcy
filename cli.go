package main

// The command line: parsing, help and errors written as clap 4.6 (the Rust
// build's parser, without colour or wrapping) wrote them, for the features
// gitdcy uses: subcommands (required or not, an automatic `help`), long and
// short options, flags, positionals with a number of values, defaults,
// possible values, global options shared with subcommands, conflicts, path
// and number values. Help goes to standard output with exit 0; an error, or
// help because nothing was given, to standard error with exit 2.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// cliArg is one argument of a command.
type cliArg struct {
	id         string
	long       string
	short      rune
	valueName  string
	help       string
	takesValue bool
	appendMode bool
	positional bool
	required   bool
	minVals    int
	maxVals    int // -1: no limit
	defaults   []string
	possible   []string
	global     bool
	conflicts  []string
	kind       string // "", "path" (not empty) or "usize"
	dispOrd    int
	index      int
	isHelp     bool
}

// flagArg is `--long`, true when given.
func flagArg(id, long string) *cliArg {
	return &cliArg{id: id, long: long, minVals: 0, maxVals: 0}
}

// optArg is `--long VALUE`.
func optArg(id, long, valueName string) *cliArg {
	return &cliArg{id: id, long: long, valueName: valueName, takesValue: true, minVals: 1, maxVals: 1}
}

// posArg is a positional VALUE.
func posArg(id, valueName string) *cliArg {
	return &cliArg{id: id, valueName: valueName, takesValue: true, positional: true, minVals: 1, maxVals: 1}
}

func (a *cliArg) Help(text string) *cliArg { a.help = text; return a }
func (a *cliArg) Required() *cliArg        { a.required = true; return a }
func (a *cliArg) Short(c rune) *cliArg     { a.short = c; return a }
func (a *cliArg) Long(name string) *cliArg { a.long = name; return a }
func (a *cliArg) Global() *cliArg          { a.global = true; return a }
func (a *cliArg) Path() *cliArg            { a.kind = "path"; return a }
func (a *cliArg) Usize() *cliArg           { a.kind = "usize"; return a }

// Append keeps every occurrence's value.
func (a *cliArg) Append() *cliArg { a.appendMode = true; return a }

// Values sets how many values a positional takes (max -1: no limit); more
// than one implies Append.
func (a *cliArg) Values(min, max int) *cliArg {
	a.minVals, a.maxVals, a.appendMode = min, max, true
	return a
}

func (a *cliArg) Default(v string) *cliArg { a.defaults = []string{v}; return a }
func (a *cliArg) Possible(values ...string) *cliArg {
	a.possible = values
	return a
}
func (a *cliArg) ConflictsWith(ids ...string) *cliArg {
	a.conflicts = append(a.conflicts, ids...)
	return a
}

// multiple reports whether the argument takes more than one value.
func (a *cliArg) multiple() bool {
	return a.appendMode || a.maxVals > 1 || a.maxVals < 0
}

// render is the argument as clap showed it: `--long <VALUE>`, `--long`,
// `<NAME>`, `[NAME]...`; required decides brackets for a positional.
func (a *cliArg) render(required bool) string {
	var b strings.Builder
	if a.long != "" {
		b.WriteString("--" + a.long)
	} else if a.short != 0 {
		b.WriteString("-" + string(a.short))
	}
	if a.takesValue && !a.positional {
		if a.minVals == 0 {
			b.WriteString(" [")
		} else {
			b.WriteString(" ")
		}
	}
	if a.takesValue || a.positional {
		name := a.valueName
		if name == "" {
			name = a.id
		}
		if a.positional && (a.minVals == 0 || !required) {
			b.WriteString("[" + name + "]")
		} else {
			b.WriteString("<" + name + ">")
		}
		extra := a.maxVals < 0 || 1 < a.maxVals
		if a.positional && a.appendMode {
			extra = true
		}
		if extra {
			b.WriteString("...")
		}
		if a.takesValue && !a.positional && a.minVals == 0 {
			b.WriteString("]")
		}
	}
	return b.String()
}

// String is the argument as it appears in errors and help.
func (a *cliArg) String() string { return a.render(a.required) }

// cliCmd is a command and its subcommands.
type cliCmd struct {
	name        string
	about       string
	afterHelp   string
	args        []*cliArg
	subs        []*cliCmd
	subRequired bool
	elseHelp    bool
	dispOrd     int
	counter     int
	parent      *cliCmd
	isHelpSub   bool
}

func newCmd(name string) *cliCmd { return &cliCmd{name: name} }

func (c *cliCmd) About(text string) *cliCmd     { c.about = text; return c }
func (c *cliCmd) AfterHelp(text string) *cliCmd { c.afterHelp = text; return c }

// SubRequired: a subcommand must be given (clap's subcommand_required).
func (c *cliCmd) SubRequired() *cliCmd { c.subRequired = true; return c }

// ElseHelp: nothing given shows the help (clap's arg_required_else_help).
func (c *cliCmd) ElseHelp() *cliCmd { c.elseHelp = true; return c }

// Arg adds arguments in order.
func (c *cliCmd) Arg(args ...*cliArg) *cliCmd {
	for _, a := range args {
		if a.positional {
			n := 0
			for _, o := range c.args {
				if o.positional {
					n++
				}
			}
			a.index = n
		} else {
			a.dispOrd = c.counter
			c.counter++
		}
		c.args = append(c.args, a)
	}
	return c
}

// Sub adds subcommands in order.
func (c *cliCmd) Sub(subs ...*cliCmd) *cliCmd {
	for _, s := range subs {
		s.dispOrd = c.counter
		c.counter++
		s.parent = c
		c.subs = append(c.subs, s)
	}
	return c
}

// built is the command as parsing and help see it: its own arguments, the
// global ones of the commands above it, `-h, --help`, and a `help`
// subcommand when it has subcommands.
func (c *cliCmd) built() *cliCmd {
	b := *c
	b.args = append([]*cliArg{}, c.args...)
	for p := c.parent; p != nil && !c.isHelpSub; p = p.parent {
		for _, a := range p.args {
			if a.global && b.find(a.id) == nil {
				b.args = append(b.args, a)
			}
		}
	}
	if !c.isHelpSub {
		b.args = append(b.args, &cliArg{id: "help", long: "help", short: 'h', help: "Print help", isHelp: true, dispOrd: 999})
	}
	if len(c.subs) > 0 && !c.isHelpSub {
		help := &cliCmd{name: "help", about: "Print this message or the help of the given subcommand(s)", dispOrd: 999, parent: c, isHelpSub: true}
		help.args = []*cliArg{posArg("subcommand", "COMMAND").Values(0, -1).Help("Print help for the subcommand(s)")}
		b.subs = append(append([]*cliCmd{}, c.subs...), help)
	}
	return &b
}

func (c *cliCmd) find(id string) *cliArg {
	for _, a := range c.args {
		if a.id == id {
			return a
		}
	}
	return nil
}

func (c *cliCmd) findLong(long string) *cliArg {
	for _, a := range c.args {
		if a.long != "" && a.long == long {
			return a
		}
	}
	return nil
}

func (c *cliCmd) findShort(r rune) *cliArg {
	for _, a := range c.args {
		if a.short == r {
			return a
		}
	}
	return nil
}

func (c *cliCmd) findSub(name string) *cliCmd {
	for _, s := range c.subs {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (c *cliCmd) positionals() []*cliArg {
	var list []*cliArg
	for _, a := range c.args {
		if a.positional {
			list = append(list, a)
		}
	}
	return list
}

// binName is the command's name in usage lines: the program's file name,
// then each subcommand.
func (c *cliCmd) binName() string {
	if c.parent == nil {
		return filepath.Base(os.Args[0])
	}
	return c.parent.binName() + " " + c.name
}

// cliMatches is what the command line gave one command.
type cliMatches struct {
	cmd     *cliCmd
	vals    map[string][]string
	given   map[string]bool
	order   []string
	subName string
	sub     *cliMatches
}

func (m *cliMatches) flag(id string) bool { return len(m.vals[id]) > 0 && m.vals[id][0] == "true" }

// str is the argument's value, if it has one.
func (m *cliMatches) str(id string) (string, bool) {
	v := m.vals[id]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// strs is every value the argument got.
func (m *cliMatches) strs(id string) []string { return append([]string{}, m.vals[id]...) }

// optStr is the value as a pointer, nil when not given.
func (m *cliMatches) optStr(id string) *string {
	if v, ok := m.str(id); ok {
		return &v
	}
	return nil
}

// cliExit is help or a usage error: the text, where it goes and the exit code.
type cliExit struct {
	text   string
	stderr bool
	code   int
}

func (e *cliExit) Error() string { return e.text }

// parseCLI parses the command line after the program name; help and usage
// errors come back as *cliExit.
func parseCLI(root *cliCmd, args []string) (*cliMatches, error) {
	m, err := parseCmd(root, args)
	if err != nil {
		return nil, err
	}
	propagateGlobals(m)
	return m, nil
}

func newMatches(c *cliCmd) *cliMatches {
	return &cliMatches{cmd: c, vals: map[string][]string{}, given: map[string]bool{}}
}

func (m *cliMatches) mark(a *cliArg) {
	if !m.given[a.id] {
		m.order = append(m.order, a.id)
	}
	m.given[a.id] = true
}

// parseCmd parses args for command c (its own level only, then recurses).
func parseCmd(raw *cliCmd, args []string) (*cliMatches, error) {
	c := raw.built()
	m := newMatches(c)
	var pending *cliArg
	var pendingVals []string
	pendingStarted := false
	inPos := false
	posIndex := 0
	trailing := false
	valid := false
	positionals := c.positionals()

	resolve := func() error {
		if pending == nil {
			return nil
		}
		a, vals := pending, pendingVals
		pending, pendingVals, pendingStarted = nil, nil, false
		return reactValues(c, m, a, vals)
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !trailing {
			if pending == nil && !inPos {
				if s := c.findSub(arg); s != nil {
					if s.isHelpSub {
						return nil, helpSubcommand(raw, args[i+1:])
					}
					if err := resolve(); err != nil {
						return nil, err
					}
					finishDefaults(c, m)
					if err := validate(c, m, true); err != nil {
						return nil, err
					}
					sm, err := parseCmd(raw.findSub(arg), args[i+1:])
					if err != nil {
						return nil, err
					}
					m.subName, m.sub = arg, sm
					return m, nil
				}
			}
			if arg == "--" {
				trailing = true
				if pending != nil && !pendingStarted {
					// the option still wants a value: clap leaves it to the end
				}
				continue
			}
			if strings.HasPrefix(arg, "--") {
				name, value, hasValue := strings.Cut(arg[2:], "=")
				a := c.findLong(name)
				if a == nil {
					if err := resolve(); err != nil {
						_ = err
					}
					return nil, unknownLong(c, m, name, args[i+1:], trailing)
				}
				valid = true
				if err := resolve(); err != nil {
					return nil, err
				}
				inPos = false
				if a.isHelp {
					return nil, helpExit(raw, false)
				}
				if a.takesValue {
					if hasValue {
						if err := reactValues(c, m, a, []string{value}); err != nil {
							return nil, err
						}
					} else {
						pending, pendingVals, pendingStarted = a, nil, false
						if err := checkRepeat(c, m, a); err != nil {
							return nil, err
						}
					}
					continue
				}
				if hasValue {
					used := []string{}
					for _, id := range m.order {
						if f := c.find(id); f != nil && !f.required {
							used = append(used, id)
						}
					}
					used = append(used, a.id)
					return nil, usageError(c, fmt.Sprintf("unexpected value '%s' for '%s' found; no more were expected", value, a), smartUsage(c, used))
				}
				if err := reactValues(c, m, a, nil); err != nil {
					return nil, err
				}
				continue
			}
			if strings.HasPrefix(arg, "-") && len(arg) > 1 {
				rest := arg[1:]
				handled := false
				for len(rest) > 0 {
					r, size := utf8.DecodeRuneInString(rest)
					rest = rest[size:]
					a := c.findShort(r)
					if a == nil {
						_ = resolve()
						return nil, unknownArgError(c, "-"+string(r), "", len(positionals) > 0 && !trailing, helpUsage(c))
					}
					valid = true
					if err := resolve(); err != nil {
						return nil, err
					}
					inPos = false
					if a.isHelp {
						return nil, helpExit(raw, false)
					}
					if !a.takesValue {
						if err := reactValues(c, m, a, nil); err != nil {
							return nil, err
						}
						continue
					}
					value := strings.TrimPrefix(rest, "=")
					if rest != "" {
						if err := reactValues(c, m, a, []string{value}); err != nil {
							return nil, err
						}
					} else {
						pending, pendingVals, pendingStarted = a, nil, false
						if err := checkRepeat(c, m, a); err != nil {
							return nil, err
						}
					}
					handled = true
					break
				}
				_ = handled
				continue
			}
			if pending != nil && !pending.positional {
				pendingVals = append(pendingVals, arg)
				pendingStarted = true
				if len(pendingVals) >= pending.maxVals && pending.maxVals >= 0 {
					if err := resolve(); err != nil {
						return nil, err
					}
				}
				continue
			}
		}
		// A positional value.
		if posIndex >= len(positionals) {
			_ = resolve()
			return nil, matchArgError(c, m, arg, valid, trailing)
		}
		p := positionals[posIndex]
		if pending != p {
			if err := resolve(); err != nil {
				return nil, err
			}
			pending, pendingVals = p, nil
		}
		pendingVals = append(pendingVals, arg)
		valid = true
		if !p.multiple() {
			if err := resolve(); err != nil {
				return nil, err
			}
			posIndex++
			inPos = false
		} else {
			inPos = true
		}
	}
	if err := resolve(); err != nil {
		return nil, err
	}
	finishDefaults(c, m)
	if err := validate(c, m, false); err != nil {
		return nil, err
	}
	return m, nil
}

// checkRepeat refuses an option given twice that keeps one value.
func checkRepeat(c *cliCmd, m *cliMatches, a *cliArg) error {
	if m.given[a.id] && !a.appendMode {
		return usageError(c, fmt.Sprintf("the argument '%s' cannot be used multiple times", a), helpUsage(c))
	}
	return nil
}

// reactValues stores an argument's values, checking them as clap did.
func reactValues(c *cliCmd, m *cliMatches, a *cliArg, vals []string) error {
	if !a.takesValue {
		if m.given[a.id] {
			return usageError(c, fmt.Sprintf("the argument '%s' cannot be used multiple times", a), helpUsage(c))
		}
		m.vals[a.id] = []string{"true"}
		m.mark(a)
		return nil
	}
	if !a.positional && !a.appendMode && m.given[a.id] && len(m.vals[a.id]) > 0 {
		return usageError(c, fmt.Sprintf("the argument '%s' cannot be used multiple times", a), helpUsage(c))
	}
	if a.minVals > 0 && len(vals) == 0 {
		return emptyValueError(c, a)
	}
	if a.maxVals >= 0 && len(vals) > a.maxVals {
		return usageError(c, fmt.Sprintf("unexpected value '%s' for '%s' found; no more were expected", vals[len(vals)-1], a), helpUsage(c))
	}
	for _, v := range vals {
		if err := checkValue(c, a, v); err != nil {
			return err
		}
	}
	if a.appendMode {
		m.vals[a.id] = append(m.vals[a.id], vals...)
	} else {
		m.vals[a.id] = vals
	}
	m.mark(a)
	return nil
}

// checkValue applies the argument's value parser.
func checkValue(c *cliCmd, a *cliArg, v string) error {
	if len(a.possible) > 0 {
		for _, p := range a.possible {
			if p == v {
				return nil
			}
		}
		if v == "" {
			return emptyValueError(c, a)
		}
		text := fmt.Sprintf("invalid value '%s' for '%s'", v, a)
		text += valuesList("possible values", a.possible)
		if s := didYouMean(v, a.possible); len(s) > 0 {
			text += fmt.Sprintf("\n\n  tip: a similar value exists: '%s'", s[len(s)-1])
		}
		return usageError(c, text, "")
	}
	switch a.kind {
	case "path":
		if v == "" {
			return emptyValueError(c, a)
		}
	case "usize":
		if v == "" {
			return emptyValueError(c, a)
		}
		if _, err := parseUsize(v); err != "" {
			return usageError(c, fmt.Sprintf("invalid value '%s' for '%s': %s", v, a, err), "")
		}
	}
	return nil
}

// parseUsize parses a number as Rust's usize::from_str did; the error is
// Rust's text.
func parseUsize(v string) (uint64, string) {
	digits := strings.TrimPrefix(v, "+")
	if digits == "" {
		if v == "" {
			return 0, "cannot parse integer from empty string"
		}
		return 0, "invalid digit found in string"
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, "invalid digit found in string"
		}
	}
	n, err := strconv.ParseUint(digits, 10, strconv.IntSize)
	if err != nil {
		return 0, "number too large to fit in target type"
	}
	return n, ""
}

func emptyValueError(c *cliCmd, a *cliArg) error {
	text := fmt.Sprintf("a value is required for '%s' but none was supplied", a)
	text += valuesList("possible values", a.possible)
	return usageError(c, text, "")
}

func valuesList(name string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = escapeValue(v)
	}
	return fmt.Sprintf("\n  [%s: %s]", name, strings.Join(quoted, ", "))
}

// escapeValue quotes a value with spaces, as clap's Escape did.
func escapeValue(v string) string {
	if strings.ContainsAny(v, " ") {
		return rustDebug(v)
	}
	return v
}

// finishDefaults gives arguments that were not given their defaults.
func finishDefaults(c *cliCmd, m *cliMatches) {
	for _, a := range c.args {
		if _, ok := m.vals[a.id]; !ok && len(a.defaults) > 0 {
			m.vals[a.id] = append([]string{}, a.defaults...)
		}
	}
}

// validate checks what clap's validator checked, in its order.
func validate(c *cliCmd, m *cliMatches, hasSub bool) error {
	if !hasSub && c.elseHelp && len(m.order) == 0 {
		return &cliExit{text: renderHelp(c), stderr: true, code: 2}
	}
	if !hasSub && c.subRequired {
		names := []string{}
		for _, s := range c.subs {
			names = append(names, s.name)
		}
		text := fmt.Sprintf("'%s' requires a subcommand but one was not provided", c.binName())
		text += valuesList("subcommands", names)
		return usageError(c, text, helpUsage(c))
	}
	// Conflicts, in the order the arguments were given.
	for _, id := range m.order {
		a := c.find(id)
		if a == nil {
			continue
		}
		var conflicting []string
		for _, other := range m.order {
			if other == id {
				continue
			}
			o := c.find(other)
			if o == nil {
				continue
			}
			if contains(a.conflicts, other) || contains(o.conflicts, id) {
				conflicting = append(conflicting, other)
			}
		}
		if len(conflicting) > 0 {
			names := make([]string, len(conflicting))
			for i, cid := range conflicting {
				names[i] = c.find(cid).String()
			}
			var used []string
			for _, u := range m.order {
				if !contains(conflicting, u) {
					used = append(used, u)
				}
			}
			text := fmt.Sprintf("the argument '%s' cannot be used with", a)
			if len(names) == 1 {
				text += fmt.Sprintf(" '%s'", names[0])
			} else {
				text += ":"
				for _, n := range names {
					text += "\n  " + n
				}
			}
			return usageError(c, text, smartUsage(c, used))
		}
	}
	if hasSub {
		return nil
	}
	// Required arguments.
	var missing []string
	for _, a := range c.args {
		if a.required && !m.given[a.id] && !conflictsWithGiven(c, m, a) {
			missing = append(missing, a.id)
		}
	}
	if len(missing) > 0 {
		var listed []string
		for _, id := range missing {
			if a := c.find(id); !a.positional {
				listed = append(listed, a.render(true))
			}
		}
		for _, p := range c.positionals() {
			if contains(missing, p.id) {
				listed = append(listed, p.render(true))
			}
		}
		text := "the following required arguments were not provided:"
		for _, l := range listed {
			text += "\n  " + l
		}
		used := append(append([]string{}, m.order...), missing...)
		return usageError(c, text, smartUsage(c, used))
	}
	return nil
}

func conflictsWithGiven(c *cliCmd, m *cliMatches, a *cliArg) bool {
	for _, id := range m.order {
		o := c.find(id)
		if o != nil && (contains(a.conflicts, id) || contains(o.conflicts, a.id)) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// propagateGlobals makes a global argument's value, given at any level,
// visible at every level (the deepest one given wins).
func propagateGlobals(top *cliMatches) {
	chain := []*cliMatches{}
	for m := top; m != nil; m = m.sub {
		chain = append(chain, m)
	}
	final := map[string][]string{}
	for _, m := range chain {
		for _, a := range m.cmd.args {
			if !a.global {
				continue
			}
			if m.given[a.id] {
				final[a.id] = m.vals[a.id]
			} else if _, ok := final[a.id]; !ok {
				if v, ok := m.vals[a.id]; ok {
					final[a.id] = v
				}
			}
		}
	}
	for _, m := range chain {
		for id, v := range final {
			if m.cmd.find(id) != nil {
				m.vals[id] = v
			}
		}
	}
}

// usageError is a usage error as clap printed it.
func usageError(c *cliCmd, message, usage string) error {
	text := "error: " + message
	if usage != "" {
		text += "\n\n" + usage
	}
	if c.isHelpSub {
		text += "\n"
	} else {
		text += "\n\nFor more information, try '--help'.\n"
	}
	return &cliExit{text: text, stderr: true, code: 2}
}

// unknownArgError is "unexpected argument 'X' found" with its tips.
func unknownArgError(c *cliCmd, arg, similar string, trailingTip bool, usage string) error {
	text := fmt.Sprintf("unexpected argument '%s' found", arg)
	if similar != "" {
		text += fmt.Sprintf("\n\n  tip: a similar argument exists: '%s'", similar)
	}
	if trailingTip {
		if similar == "" {
			text += "\n"
		}
		text += fmt.Sprintf("\n  tip: to pass '%s' as a value, use '-- %s'", arg, arg)
	}
	return usageError(c, text, usage)
}

// unknownLong is the error for an unknown `--name`, with a similar long
// option when there is one.
func unknownLong(c *cliCmd, m *cliMatches, name string, remaining []string, trailing bool) error {
	var longs []string
	for _, a := range c.args {
		if a.long != "" {
			longs = append(longs, a.long)
		}
	}
	similar := ""
	used := append([]string{}, m.order...)
	if s := didYouMean(name, longs); len(s) > 0 {
		best := s[len(s)-1]
		similar = "--" + best
		if a := c.findLong(best); a != nil && !contains(used, a.id) {
			used = append(used, a.id)
		}
	}
	if similar == "" {
		best, bestScore := "", -1
		for _, sub := range c.subs {
			var subLongs []string
			for _, a := range sub.args {
				if a.long != "" {
					subLongs = append(subLongs, a.long)
				}
			}
			if !sub.isHelpSub {
				subLongs = append(subLongs, "help")
			}
			s := didYouMean(name, subLongs)
			if len(s) == 0 {
				continue
			}
			score := -1
			for i, r := range remaining {
				if r == sub.name {
					score = i
					break
				}
			}
			if score >= 0 && (bestScore < 0 || score < bestScore) {
				best, bestScore = sub.name+" --"+s[len(s)-1], score
			}
		}
		if best != "" {
			text := fmt.Sprintf("unexpected argument '--%s' found\n\n  tip: '%s' exists", name, best)
			return usageError(c, text, usageFor(c, m.order))
		}
	}
	trailingTip := similar == "" && !trailing && len(c.positionals()) > 0
	return unknownArgError(c, "--"+name, similar, trailingTip, usageFor(c, used))
}

// matchArgError is the error for a value that is no positional here.
func matchArgError(c *cliCmd, m *cliMatches, arg string, valid, trailing bool) error {
	if trailing && c.findSub(arg) != nil {
		return usageError(c, fmt.Sprintf("unexpected argument '%s' found\n\n  tip: subcommand '%s' exists; to use it, remove the '--' before it", arg, arg), helpUsage(c))
	}
	hasPos := len(c.positionals()) > 0
	trailingTip := !trailing && hasPos && strings.HasPrefix(arg, "-") && len(arg) > 1
	if len(c.subs) > 0 {
		names := []string{}
		for _, s := range c.subs {
			names = append(names, s.name)
		}
		if candidates := didYouMean(arg, names); len(candidates) > 0 {
			text := fmt.Sprintf("unrecognized subcommand '%s'", arg)
			if len(candidates) == 1 {
				text += fmt.Sprintf("\n\n  tip: a similar subcommand exists: '%s'", candidates[0])
			} else {
				text += "\n\n  tip: some similar subcommands exist: '" + strings.Join(candidates, "', '") + "'"
			}
			if trailingTip {
				text += fmt.Sprintf("\n  tip: to pass '%s' as a value, use '%s -- %s'", arg, c.binName(), arg)
			}
			return usageError(c, text, helpUsage(c))
		}
		if !hasPos {
			return usageError(c, fmt.Sprintf("unrecognized subcommand '%s'", arg), helpUsage(c))
		}
	}
	return unknownArgError(c, arg, "", trailingTip, helpUsage(c))
}

// helpSubcommand is `help [COMMAND]...`: the help of the command named.
func helpSubcommand(c *cliCmd, names []string) error {
	for _, name := range names {
		b := c.built()
		s := b.findSub(name)
		if s == nil {
			return usageError(b, fmt.Sprintf("unrecognized subcommand '%s'", name), helpUsage(b))
		}
		c = s
	}
	return helpExit(c, false)
}

// helpExit is the help of c on standard output, exit 0.
func helpExit(c *cliCmd, _ bool) error {
	return &cliExit{text: renderHelp(c.built()), code: 0}
}

// ---------------------------------------------------------------------------
// Usage lines

// helpUsage is "Usage: NAME [OPTIONS] ARGS [COMMAND]" for c (built).
func helpUsage(c *cliCmd) string {
	return "Usage: " + usageLine(c, nil)
}

// smartUsage is the usage line naming what was used (no [OPTIONS]).
func smartUsage(c *cliCmd, used []string) string {
	return "Usage: " + usageLine(c, used)
}

// usageFor is the help usage when nothing was used, else the smart usage.
func usageFor(c *cliCmd, used []string) string {
	if len(used) == 0 {
		return helpUsage(c)
	}
	return smartUsage(c, used)
}

func usageLine(c *cliCmd, used []string) string {
	var b strings.Builder
	b.WriteString(c.binName() + " ")
	if len(used) == 0 && needsOptionsTag(c) {
		b.WriteString("[OPTIONS] ")
	}
	// Required options and the ones used, then every positional.
	var opts []string
	add := func(s string) {
		if !contains(opts, s) {
			opts = append(opts, s)
		}
	}
	required := []string{}
	for _, a := range c.args {
		if a.required {
			required = append(required, a.id)
		}
	}
	positions := map[int]string{}
	for _, id := range append(required, used...) {
		a := c.find(id)
		if a == nil {
			continue
		}
		if a.positional {
			positions[a.index] = a.render(true)
		} else {
			add(a.render(true))
		}
	}
	for _, p := range c.positionals() {
		if _, ok := positions[p.index]; !ok {
			positions[p.index] = p.render(false)
		}
	}
	for _, o := range opts {
		b.WriteString(o + " ")
	}
	for i := 0; i < len(c.positionals()); i++ {
		if s, ok := positions[i]; ok {
			b.WriteString(s + " ")
		}
	}
	if len(used) == 0 {
		if len(c.subs) > 0 {
			if c.subRequired {
				b.WriteString("<COMMAND>")
			} else {
				b.WriteString("[COMMAND]")
			}
		}
	} else if c.subRequired {
		b.WriteString("<COMMAND>")
	}
	return strings.TrimRight(b.String(), " ")
}

func needsOptionsTag(c *cliCmd) bool {
	for _, a := range c.args {
		if a.positional || a.isHelp || a.required {
			continue
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Help

// renderHelp is c's help text (c built), ending in a newline.
func renderHelp(c *cliCmd) string {
	var b strings.Builder
	if c.about != "" {
		b.WriteString(c.about + "\n")
	}
	b.WriteString("\nUsage: " + usageLine(c, nil) + "\n\n")
	b.WriteString(allArgs(c))
	if c.afterHelp != "" {
		b.WriteString("\n\n" + c.afterHelp)
	}
	text := strings.TrimLeft(b.String(), "\n")
	return strings.TrimRight(text, " \t\n\r") + "\n"
}

func allArgs(c *cliCmd) string {
	var sections []string
	if len(c.subs) > 0 {
		sections = append(sections, "Commands:\n"+subcommandList(c))
	}
	var pos, opts []*cliArg
	for _, a := range c.args {
		if a.positional {
			pos = append(pos, a)
		} else {
			opts = append(opts, a)
		}
	}
	if len(pos) > 0 {
		sections = append(sections, "Arguments:\n"+argList(pos))
	}
	if len(opts) > 0 {
		sortArgs(opts)
		sections = append(sections, "Options:\n"+argList(opts))
	}
	return strings.Join(sections, "\n\n")
}

// sortArgs orders options by display order, then name, as clap did.
func sortArgs(list []*cliArg) {
	key := func(a *cliArg) string {
		switch {
		case a.short != 0:
			lower := strings.ToLower(string(a.short))
			if a.short >= 'a' && a.short <= 'z' {
				return lower + "0"
			}
			return lower + "1"
		case a.long != "":
			return a.long
		}
		return "{" + a.id
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			x, y := list[j-1], list[j]
			if x.dispOrd > y.dispOrd || (x.dispOrd == y.dispOrd && key(x) > key(y)) {
				list[j-1], list[j] = y, x
			}
		}
	}
}

const termWidth = 100

func argList(list []*cliArg) string {
	longest := 2
	for _, a := range list {
		w := utf8.RuneCountInString(a.String())
		if a.long != "" {
			w += 4
		}
		if w > longest {
			longest = w
		}
	}
	nextLine := false
	for _, a := range list {
		taken := longest + 4
		hw := utf8.RuneCountInString(a.help) + utf8.RuneCountInString(specVals(a))
		if termWidth >= taken && float32(taken)/float32(termWidth) > 0.40 && hw > termWidth-taken {
			nextLine = true
		}
	}
	var b strings.Builder
	for i, a := range list {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  ")
		switch {
		case a.short != 0:
			b.WriteString("-" + string(a.short))
		case a.long != "":
			b.WriteString("    ")
		}
		if a.long != "" {
			if a.short != 0 {
				b.WriteString(", ")
			}
			b.WriteString("--" + a.long)
		}
		suffix := strings.TrimPrefix(strings.TrimPrefix(a.String(), "--"+a.long), "-"+string(a.short))
		if a.positional {
			suffix = a.String()
		}
		b.WriteString(suffix)
		if nextLine {
			b.WriteString("\n          ")
		} else {
			self := utf8.RuneCountInString(a.String())
			pad := 0
			if a.positional {
				pad = longest + 2 - self
			} else if a.long != "" {
				pad = longest + 2 - (self + 4)
			} else {
				pad = longest + 6 - (self + 4)
			}
			b.WriteString(strings.Repeat(" ", pad))
		}
		help := a.help
		if spec := specVals(a); spec != "" {
			if help != "" {
				help += " "
			}
			help += spec
		}
		b.WriteString(help)
	}
	return b.String()
}

func specVals(a *cliArg) string {
	var parts []string
	if a.takesValue && len(a.defaults) > 0 {
		vals := make([]string, len(a.defaults))
		for i, d := range a.defaults {
			vals[i] = escapeValue(d)
		}
		parts = append(parts, "[default: "+strings.Join(vals, " ")+"]")
	}
	if len(a.possible) > 0 {
		vals := make([]string, len(a.possible))
		for i, p := range a.possible {
			vals[i] = escapeValue(p)
		}
		parts = append(parts, "[possible values: "+strings.Join(vals, ", ")+"]")
	}
	return strings.Join(parts, " ")
}

func subcommandList(c *cliCmd) string {
	subs := append([]*cliCmd{}, c.subs...)
	for i := 1; i < len(subs); i++ {
		for j := i; j > 0 && (subs[j-1].dispOrd > subs[j].dispOrd || (subs[j-1].dispOrd == subs[j].dispOrd && subs[j-1].name > subs[j].name)); j-- {
			subs[j-1], subs[j] = subs[j], subs[j-1]
		}
	}
	longest := 2
	for _, s := range subs {
		if w := utf8.RuneCountInString(s.name); w > longest {
			longest = w
		}
	}
	nextLine := false
	for _, s := range subs {
		taken := longest + 4
		if termWidth >= taken && float32(taken)/float32(termWidth) > 0.40 && utf8.RuneCountInString(s.about) > termWidth-taken {
			nextLine = true
		}
	}
	var b strings.Builder
	for i, s := range subs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + s.name)
		if nextLine {
			b.WriteString("\n          ")
		} else {
			b.WriteString(strings.Repeat(" ", longest+2-utf8.RuneCountInString(s.name)))
		}
		b.WriteString(s.about)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Suggestions

// didYouMean is every candidate more than 0.7 similar to v (Jaro), least
// similar first, as clap's did_you_mean listed them.
func didYouMean(v string, candidates []string) []string {
	type scored struct {
		score float64
		name  string
	}
	var list []scored
	for _, c := range candidates {
		s := jaro(v, c)
		if s > 0.7 {
			pos := 0
			for pos < len(list) && !(list[pos].score > s) {
				pos++
			}
			list = append(list, scored{})
			copy(list[pos+1:], list[pos:])
			list[pos] = scored{s, c}
		}
	}
	names := make([]string, len(list))
	for i, s := range list {
		names[i] = s.name
	}
	return names
}

// jaro is strsim's Jaro similarity over characters.
func jaro(a, b string) float64 {
	ar, br := []rune(a), []rune(b)
	al, bl := len(ar), len(br)
	if al == 0 && bl == 0 {
		return 1
	}
	if al == 0 || bl == 0 {
		return 0
	}
	search := al
	if bl > search {
		search = bl
	}
	search = search / 2
	if search > 0 {
		search--
	}
	aFlags, bFlags := make([]bool, al), make([]bool, bl)
	matches := 0
	for i := 0; i < al; i++ {
		lo := 0
		if i > search {
			lo = i - search
		}
		hi := i + search + 1
		if hi > bl {
			hi = bl
		}
		for j := 0; j < hi; j++ {
			if lo <= j && ar[i] == br[j] && !bFlags[j] {
				aFlags[i], bFlags[j] = true, true
				matches++
				break
			}
		}
	}
	if matches == 0 {
		return 0
	}
	transpositions := 0
	j := 0
	for i := 0; i < al; i++ {
		if !aFlags[i] {
			continue
		}
		for j < bl && !bFlags[j] {
			j++
		}
		if j < bl {
			if ar[i] != br[j] {
				transpositions++
			}
			j++
		}
	}
	transpositions /= 2
	m := float64(matches)
	return (m/float64(al) + m/float64(bl) + (m-float64(transpositions))/m) / 3
}

// runCLI parses the command line, printing help or a usage error and exiting
// as clap did.
func runCLI(root *cliCmd) *cliMatches {
	m, err := parseCLI(root, os.Args[1:])
	if err != nil {
		if e, ok := err.(*cliExit); ok {
			if e.stderr {
				fmt.Fprint(os.Stderr, e.text)
			} else {
				fmt.Fprint(os.Stdout, e.text)
			}
			os.Exit(e.code)
		}
		fail(err)
	}
	return m
}
