package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
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
	"expand-all",
	"page-up",
	"page-down",
	"paste",
	"history-prev",
	"history-next",
	"queue-edit",
	"complete",
	"menu-up",
	"menu-down",
	"menu-close",
}

// StatusSegments lists every segment the status line can show.
var StatusSegments = []string{"model", "context", "branch", "mode", "notice", "sandbox", "tasks"}

type StatusLine struct {
	Left  []string `koanf:"left"`
	Right []string `koanf:"right"`
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
		"expand-all":   {"ctrl+o"},
		"page-up":      {"pgup"},
		"page-down":    {"pgdown"},
		"paste":        {"ctrl+v"},
		"history-prev": {"up"},
		"history-next": {"down"},
		"queue-edit":   {"up"},
		"complete":     {"tab"},
		"menu-up":      {"up"},
		"menu-down":    {"down"},
		"menu-close":   {"esc"},
	}
}

func DefaultStatusLine() StatusLine {
	return StatusLine{Left: []string{"model", "context", "branch"}, Right: []string{"notice", "sandbox", "tasks", "mode"}}
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
	return map[string]any{
		"keys":       keys,
		"statusline": map[string]any{"left": s.Left, "right": s.Right},
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
	for _, side := range []struct {
		name string
		segs []string
	}{{"left", c.StatusLine.Left}, {"right", c.StatusLine.Right}} {
		for _, s := range side.segs {
			if !slices.Contains(StatusSegments, s) {
				return fmt.Errorf("%w: statusline.%s: unknown segment %q", ErrInvalidValue, side.name, s)
			}
		}
	}
	return nil
}
