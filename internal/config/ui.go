package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
var StatusSegments = []string{"model", "effort", "context", "tools", "cost", "branch", "mode", "notice", "sandbox", "tasks", "cwd"}

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
	return StatusLine{Left: []string{"model", "context", "branch"}, Right: []string{"notice", "sandbox", "tasks", "mode"}, Separator: " · "}
}

func DefaultAgentLine() AgentLine {
	return AgentLine{Left: []string{"agent", "tokens", "model"}, Right: []string{}, Separator: " · "}
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

// validateSegmentRefs checks the merged lists of the bar named by prefix
// against its known segments, naming the layer that set each list.
func validateSegmentRefs(prefix string, left, right []string, known func(string) bool, origins map[string]string) error {
	for _, side := range []struct {
		name string
		segs []string
	}{{"left", left}, {"right", right}} {
		for _, name := range side.segs {
			if !known(name) {
				key := prefix + "." + side.name
				return fmt.Errorf("config layer %s: %w: %s: unknown segment %q", originOf(origins, key), ErrInvalidValue, key, name)
			}
		}
	}
	return nil
}

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
