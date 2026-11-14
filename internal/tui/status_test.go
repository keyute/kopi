package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/policy"
)

func TestStatusLine(t *testing.T) {
	full := app.Status{Model: "anthropic/claude-x", ContextTokens: 18_000, ContextWindow: 100_000, Branch: "main"}
	for _, tc := range []struct {
		name  string
		s     app.Status
		width int
		want  string
	}{
		{"everything", full, 40, "  claude-x · 18% · main" + strings.Repeat(" ", 11) + "plan  "},
		{"no window", app.Status{Model: "a/b", Branch: "main"}, 20, "  b · main" + strings.Repeat(" ", 4) + "plan  "},
		{"no branch", app.Status{Model: "a/b", ContextTokens: 1, ContextWindow: 4}, 20, "  b · 25%" + strings.Repeat(" ", 5) + "plan  "},
		{"id without provider", app.Status{Model: "local", Mode: policy.Execute}, 20, "  local" + strings.Repeat(" ", 4) + "execute  "},
		{"drops branch first", full, 29, "  claude-x · 18%" + strings.Repeat(" ", 7) + "plan  "},
		{"then context", full, 22, "  claude-x" + strings.Repeat(" ", 6) + "plan  "},
		{"mode always shows", full, 10, strings.Repeat(" ", 4) + "plan  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ansi.Strip(statusLine(tc.s, tc.width, mocha())))
		})
	}
}

func TestStatusLineWarnsFromNinetyPercent(t *testing.T) {
	th := mocha()
	s := app.Status{Model: "a/b", ContextTokens: 89, ContextWindow: 100}
	assert.Contains(t, statusLine(s, 40, th), fg(th.dim).Render("89%"))

	s.ContextTokens = 90
	assert.Contains(t, statusLine(s, 40, th), fg(th.warning).Render("90%"))
}
