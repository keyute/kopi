package tui

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/policy"
)

var defaultLayout = statusLayout{left: config.DefaultStatusLine().Left, right: config.DefaultStatusLine().Right}

func TestSegmentsMatchConfig(t *testing.T) {
	assert.ElementsMatch(t, config.StatusSegments, slices.Collect(maps.Keys(segments)))
}

func TestStatusLine(t *testing.T) {
	full := app.Status{Model: "anthropic/claude-x", ContextTokens: 18_000, ContextWindow: 100_000, Branch: "main"}
	custom := statusLayout{left: []string{"branch"}, right: []string{"context", "mode"}}
	noticeLeft := statusLayout{left: []string{"notice", "branch"}, right: []string{"mode"}}
	for _, tc := range []struct {
		name   string
		s      app.Status
		layout statusLayout
		notice string
		width  int
		want   string
	}{
		{"everything", full, defaultLayout, "", 40, "  claude-x · 18.0% · main" + strings.Repeat(" ", 9) + "plan  "},
		{"no window", app.Status{Model: "a/b", Branch: "main"}, defaultLayout, "", 20, "  b · main" + strings.Repeat(" ", 4) + "plan  "},
		{"no branch", app.Status{Model: "a/b", ContextTokens: 1, ContextWindow: 4}, defaultLayout, "", 20, "  b · 25.0%" + strings.Repeat(" ", 3) + "plan  "},
		{"id without provider", app.Status{Model: "local", Mode: policy.Execute}, defaultLayout, "", 20, "  local" + strings.Repeat(" ", 4) + "execute  "},
		{"drops branch first", full, defaultLayout, "", 31, "  claude-x · 18.0%" + strings.Repeat(" ", 7) + "plan  "},
		{"then context", full, defaultLayout, "", 24, "  claude-x" + strings.Repeat(" ", 8) + "plan  "},
		{"right always shows", full, defaultLayout, "", 10, strings.Repeat(" ", 4) + "plan  "},
		{"custom layout", full, custom, "", 30, "  main" + strings.Repeat(" ", 10) + "18.0% · plan  "},
		{"notice before mode", full, defaultLayout, "Copied", 50, "  claude-x · 18.0% · main" + strings.Repeat(" ", 10) + "Copied · plan  "},
		{"layout without notice", full, custom, "hello", 30, "  main" + strings.Repeat(" ", 10) + "18.0% · plan  "},
		{"notice on the left", full, noticeLeft, "hello", 30, "  hello · main" + strings.Repeat(" ", 10) + "plan  "},
		{"long notice drops left", full, defaultLayout, "ctrl+c again to exit", 40, "  claude-x ctrl+c again to exit · plan  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ansi.Strip(statusLine(tc.s, tc.layout, tc.notice, tc.width, mocha())))
		})
	}
}

func TestStatusLineWarnsFromNinetyPercent(t *testing.T) {
	th := mocha()
	s := app.Status{Model: "a/b", ContextTokens: 899, ContextWindow: 1000}
	assert.Contains(t, statusLine(s, defaultLayout, "", 40, th), fg(th.text).Render("89.9%"))

	s.ContextTokens = 900
	assert.Contains(t, statusLine(s, defaultLayout, "", 40, th), fg(th.warning).Render("90.0%"))
}
