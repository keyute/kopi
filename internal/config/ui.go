package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// KeyActions lists every bindable action in display order.
var KeyActions = []string{
	"submit",
	"newline",
	"interrupt",
	"cancel",
	"quit",
	"send-now",
	"toggle-mode",
	"background",
	"stop-agent",
	"expand-all",
	"page-up",
	"page-down",
	"paste",
	"history-prev",
	"history-next",
	"queue-edit",
	"edit-plan",
	"complete",
	"menu-up",
	"menu-down",
	"menu-close",
}

// StatusSegments lists every segment the status line can show.
var StatusSegments = []string{"model", "effort", "context", "tools", "cost", "branch", "mode", "notice", "sandbox", "agents", "shells", "workflows", "tokens", "changes", "cwd"}

// AgentSegments lists every segment an agent row can show.
var AgentSegments = []string{"agent", "tokens", "model", "effort", "context", "tools", "cost", "elapsed", "state", "activity", "id"}

// ThemeRoles lists every colour role a theme defines.
var ThemeRoles = []string{"accent", "success", "error", "warning", "muted", "dim", "text", "userMessageBg", "heading", "link", "code", "frame"}

type StatusLine struct {
	Left  []string `koanf:"left"`
	Right []string `koanf:"right"`
	// Separator is drawn between segments; "" is a valid choice.
	Separator string                   `koanf:"separator"`
	Segments  map[string]StatusSegment `koanf:"segments"`
}

type AgentLine struct {
	Left  []string `koanf:"left"`
	Right []string `koanf:"right"`
	// Separator is drawn between segments; "" is a valid choice.
	Separator string `koanf:"separator"`
}

// StatusSegment is a user-defined segment with either static Text or the
// output of Command, rerun every Interval (a Go duration) when one is set.
type StatusSegment struct {
	Text     string `koanf:"text"`
	Command  string `koanf:"command"`
	Color    string `koanf:"color"`
	Interval string `koanf:"interval"`
}

// Theme names the theme used on a dark and on a light background.
type Theme struct {
	Dark  string `koanf:"dark"`
	Light string `koanf:"light"`
}

func DefaultTheme() Theme {
	return Theme{Dark: "catppuccin-mocha", Light: "catppuccin-latte"}
}

func DefaultKeys() map[string][]string {
	return map[string][]string{
		"submit":       {"enter"},
		"newline":      {"shift+enter", "alt+enter", "ctrl+j"},
		"interrupt":    {"esc"},
		"cancel":       {"ctrl+c"},
		"quit":         {"ctrl+d"},
		"send-now":     {"ctrl+enter"},
		"toggle-mode":  {"shift+tab"},
		"background":   {"ctrl+b"},
		"stop-agent":   {"x"},
		"expand-all":   {"ctrl+o"},
		"page-up":      {"pgup"},
		"page-down":    {"pgdown"},
		"paste":        {"ctrl+v"},
		"history-prev": {"up"},
		"history-next": {"down"},
		"queue-edit":   {"up"},
		"edit-plan":    {"ctrl+g"},
		"complete":     {"tab"},
		"menu-up":      {"up"},
		"menu-down":    {"down"},
		"menu-close":   {"esc"},
	}
}

func DefaultStatusLine() StatusLine {
	return StatusLine{Left: []string{"$model", "$context", "$branch( $changes)"}, Right: []string{"$notice", "$shells", "$workflows", "$sandbox", "$mode"}, Separator: " · "}
}

func DefaultAgentLine() AgentLine {
	return AgentLine{Left: []string{"$agent", "$model", "$tokens"}, Right: []string{}, Separator: " · "}
}

// uiDefaults is the default layer's share of the UI blocks. Nested values must
// be map[string]any: koanf flattens and merges only that type, so any other map
// would become one leaf that a layer can only replace whole.
func uiDefaults() map[string]any {
	keys := map[string]any{}
	for action, ks := range DefaultKeys() {
		keys[action] = ks
	}
	s := DefaultStatusLine()
	a := DefaultAgentLine()
	t := DefaultTheme()
	return map[string]any{
		"keys":       keys,
		"statusline": map[string]any{"left": s.Left, "right": s.Right, "separator": s.Separator, "segments": map[string]any{}},
		"agentline":  map[string]any{"left": a.Left, "right": a.Right, "separator": a.Separator},
		"theme":      map[string]any{"dark": t.Dark, "light": t.Light},
	}
}

// validateUI checks one layer's UI values; c holds only what that layer set.
func validateUI(c Config) error {
	for _, action := range slices.Sorted(maps.Keys(c.Keys)) {
		if !slices.Contains(KeyActions, action) {
			return fmt.Errorf("%w: keys.%s", ErrUnknownKey, action)
		}
		ks := c.Keys[action]
		if len(ks) == 0 {
			return fmt.Errorf("%w: keys.%s: empty key list", ErrInvalidValue, action)
		}
		for _, k := range ks {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%w: keys.%s: blank key %q", ErrInvalidValue, action, k)
			}
		}
	}
	if strings.ContainsFunc(c.StatusLine.Separator, unicode.IsControl) {
		return fmt.Errorf("%w: statusline.separator: control character", ErrInvalidValue)
	}
	if strings.ContainsFunc(c.AgentLine.Separator, unicode.IsControl) {
		return fmt.Errorf("%w: agentline.separator: control character", ErrInvalidValue)
	}
	return validateSegments(c.StatusLine.Segments)
}

func validateSegments(segments map[string]StatusSegment) error {
	for _, name := range slices.Sorted(maps.Keys(segments)) {
		seg := segments[name]
		key := "statusline.segments." + name
		switch {
		case !validSegmentName(name):
			return fmt.Errorf("%w: %s: name must be letters, digits or _", ErrInvalidValue, key)
		case slices.Contains(StatusSegments, name):
			return fmt.Errorf("%w: %s: name is a built-in segment", ErrInvalidValue, key)
		case strings.ContainsFunc(seg.Text, unicode.IsControl):
			return fmt.Errorf("%w: %s.text: control character", ErrInvalidValue, key)
		case (seg.Text == "") == (seg.Command == ""):
			return fmt.Errorf("%w: %s: want text or command, not both or neither", ErrInvalidValue, key)
		case seg.Interval != "" && seg.Command == "":
			return fmt.Errorf("%w: %s: interval needs command", ErrInvalidValue, key)
		}
		if seg.Interval != "" {
			d, err := time.ParseDuration(seg.Interval)
			if err != nil || d < time.Second {
				return fmt.Errorf("%w: %s: interval %q is not a duration of at least 1s", ErrInvalidValue, key, seg.Interval)
			}
		}
		if seg.Color != "" {
			if err := ValidColor(seg.Color); err != nil {
				return fmt.Errorf("%w: %s.color: %w", ErrInvalidValue, key, err)
			}
		}
	}
	return nil
}

// validateSegmentRefs checks the merged entries of the bar named by prefix
// against its known segments, naming the layer that set each list.
func validateSegmentRefs(prefix string, left, right []string, known func(string) bool, origins map[string]string) error {
	for _, side := range []struct {
		name string
		segs []string
	}{{"left", left}, {"right", right}} {
		key := prefix + "." + side.name
		for _, entry := range side.segs {
			if err := validateEntry(entry, known); err != nil {
				return fmt.Errorf("config layer %s: %w: %s: %w", originOf(origins, key), ErrInvalidValue, key, err)
			}
		}
	}
	return nil
}

func validateEntry(entry string, known func(string) bool) error {
	if strings.ContainsFunc(entry, unicode.IsControl) {
		return fmt.Errorf("%q: control character", entry)
	}
	f, err := ParseFormat(entry)
	if err != nil {
		return fmt.Errorf("%q: %w", entry, err)
	}
	names := f.Segments()
	for _, name := range names {
		if !known(name) {
			return fmt.Errorf("unknown segment %q", name)
		}
	}
	if len(names) == 0 {
		if known(entry) {
			return fmt.Errorf("%q names no segment; write %q", entry, "$"+entry)
		}
		return fmt.Errorf("%q names no segment", entry)
	}
	if hasEmptyGroup(f) {
		return fmt.Errorf("%q: a group names no segment", entry)
	}
	return nil
}

func hasEmptyGroup(f Format) bool {
	return slices.ContainsFunc(f, func(p Part) bool {
		return p.Kind == Group && (len(Format(p.Parts).Segments()) == 0 || hasEmptyGroup(p.Parts))
	})
}

// PartKind says what a Part of a Format is.
type PartKind int

const (
	// Literal is text drawn as written.
	Literal PartKind = iota
	// Segment names a segment in Text.
	Segment
	// Group holds Parts, drawn only when a segment inside has something to show.
	Group
)

// Part is one piece of a Format.
type Part struct {
	Kind  PartKind
	Text  string
	Parts []Part
}

// Format is a parsed status line or agent row entry.
type Format []Part

// SegmentNames lists the segments named by every entry of sides, which must
// already be validated.
func SegmentNames(sides ...[]string) []string {
	var names []string
	for _, entry := range slices.Concat(sides...) {
		f, _ := ParseFormat(entry)
		names = append(names, f.Segments()...)
	}
	return names
}

// Segments lists every segment the format names, in order, at any depth.
func (f Format) Segments() []string {
	var names []string
	for _, p := range f {
		switch p.Kind {
		case Segment:
			names = append(names, p.Text)
		case Group:
			names = append(names, Format(p.Parts).Segments()...)
		}
	}
	return names
}

// ParseFormat parses an entry: "$name" is a segment, "(...)" a group, "\x"
// the character x, and anything else literal text.
func ParseFormat(s string) (Format, error) {
	f, _, err := parseParts(s, false)
	return f, err
}

// parseParts reads parts up to the ")" that closes a group, or to the end of s,
// and returns what follows.
func parseParts(s string, inGroup bool) (Format, string, error) {
	var f Format
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			f = append(f, Part{Kind: Literal, Text: lit.String()})
			lit.Reset()
		}
	}
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		switch r {
		case '\\':
			if size == len(s) {
				return nil, "", errors.New(`trailing "\"`)
			}
			next, n := utf8.DecodeRuneInString(s[size:])
			lit.WriteRune(next)
			s = s[size+n:]
		case '$':
			name := leadingName(s[size:])
			if name == "" {
				return nil, "", errors.New(`"$" not followed by a segment name`)
			}
			flush()
			f = append(f, Part{Kind: Segment, Text: name})
			s = s[size+len(name):]
		case '(':
			flush()
			inner, rest, err := parseParts(s[size:], true)
			if err != nil {
				return nil, "", err
			}
			f = append(f, Part{Kind: Group, Parts: inner})
			s = rest
		case ')':
			if !inGroup {
				return nil, "", errors.New(`")" without "("`)
			}
			flush()
			return f, s[size:], nil
		default:
			lit.WriteRune(r)
			s = s[size:]
		}
	}
	if inGroup {
		return nil, "", errors.New(`unclosed "("`)
	}
	flush()
	return f, "", nil
}

func isNameRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// leadingName is the longest prefix of s made of name characters.
func leadingName(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool { return !isNameRune(r) })
	if end < 0 {
		return s
	}
	return s[:end]
}

func validSegmentName(name string) bool { return name != "" && leadingName(name) == name }

func validateThemeName(key, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %s: %q is not a theme name", ErrInvalidValue, key, name)
	}
	return nil
}

// ValidColor accepts a ThemeRoles name or a literal colour.
func ValidColor(s string) error {
	if slices.Contains(ThemeRoles, s) {
		return nil
	}
	return ValidLiteralColor(s)
}

// ValidLiteralColor accepts "#rgb", "#rrggbb" or an ANSI index from 0 to 255.
func ValidLiteralColor(s string) error {
	if hex, ok := strings.CutPrefix(s, "#"); ok {
		if _, err := strconv.ParseUint(hex, 16, 32); err == nil && (len(hex) == 3 || len(hex) == 6) {
			return nil
		}
	} else if _, err := strconv.ParseUint(s, 10, 8); err == nil {
		return nil
	}
	return fmt.Errorf("invalid colour %q: want #rgb, #rrggbb or 0-255", s)
}
