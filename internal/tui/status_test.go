package tui

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/policy"
)

var defaultLayout = statusLayout{left: config.DefaultStatusLine().Left, right: config.DefaultStatusLine().Right, separator: " · "}

func TestSegmentsMatchConfig(t *testing.T) {
	assert.ElementsMatch(t, config.StatusSegments, slices.Collect(maps.Keys(segments)))
}

func TestStatusLine(t *testing.T) {
	full := app.Status{Model: "anthropic/claude-x", ContextTokens: 18_000, ContextWindow: 100_000, Branch: "main"}
	custom := statusLayout{left: []string{"branch"}, right: []string{"context", "mode"}, separator: " · "}
	noticeLeft := statusLayout{left: []string{"notice", "branch"}, right: []string{"mode"}, separator: " · "}
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
		{"sandboxed shell", app.Status{Model: "a/b", Shell: true, Sandboxed: true}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 9) + "sandboxed · plan  "},
		{"unsandboxed shell", app.Status{Model: "a/b", Shell: true}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 7) + "unsandboxed · plan  "},
		{"no shell", app.Status{Model: "a/b", Sandboxed: true}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 21) + "plan  "},
		{"one background shell", app.Status{Model: "a/b", Shells: 1}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 11) + "1 shell · plan  "},
		{"background shells", app.Status{Model: "a/b", Shells: 3}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 10) + "3 shells · plan  "},
		{"background agents", app.Status{Model: "a/b", Agents: 2}, defaultLayout, "", 40, "  b" + strings.Repeat(" ", 20) + "2 agents · plan  "},
		{"agents then shells", app.Status{Model: "a/b", Agents: 2, Shells: 1}, defaultLayout, "", 40, "  b" + strings.Repeat(" ", 11) + "2 agents, 1 shell · plan  "},
		{"singular agent and shell", app.Status{Model: "a/b", Agents: 1, Shells: 1}, defaultLayout, "", 40, "  b" + strings.Repeat(" ", 12) + "1 agent, 1 shell · plan  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ansi.Strip(statusLine(statusInput{Status: tc.s, notice: tc.notice}, tc.layout, tc.width, mocha())))
		})
	}
}

func TestStatusLineFitsNarrowWidthsAndKeepsMode(t *testing.T) {
	for _, layout := range []statusLayout{
		defaultLayout,
		{right: []string{"branch", "mode", "notice"}, separator: " · "},
		{left: []string{"mode"}, right: []string{"branch"}, separator: " · "},
	} {
		for width := 0; width <= 30; width++ {
			s := app.Status{Branch: "\x1b[31m" + strings.Repeat("界", 30) + "\x1b[0m", Mode: policy.Execute}
			got := statusLine(statusInput{Status: s, notice: "ctrl+c again to exit"}, layout, width, mocha())
			assert.LessOrEqual(t, ansi.StringWidth(got), width)
			if width >= len("execute") {
				assert.Contains(t, ansi.Strip(got), "execute")
			}
		}
	}
	got := statusLine(statusInput{notice: "ctrl+c again to exit"}, defaultLayout, 20, mocha())
	assert.Equal(t, 20, ansi.StringWidth(got))
	assert.Contains(t, ansi.Strip(got), "plan")
}

func TestStatusLineWarnsFromNinetyPercent(t *testing.T) {
	th := mocha()
	s := app.Status{Model: "a/b", ContextTokens: 899, ContextWindow: 1000}
	assert.Contains(t, statusLine(statusInput{Status: s}, defaultLayout, 40, th), fg(th.text).Render("89.9%"))

	s.ContextTokens = 900
	assert.Contains(t, statusLine(statusInput{Status: s}, defaultLayout, 40, th), fg(th.warning).Render("90.0%"))
}

func TestStatusLineWarnsWhenUnsandboxed(t *testing.T) {
	th := mocha()
	s := app.Status{Model: "a/b", Shell: true, Sandboxed: true}
	assert.Contains(t, statusLine(statusInput{Status: s}, defaultLayout, 40, th), fg(th.dim).Render("sandboxed"))

	s.Sandboxed = false
	assert.Contains(t, statusLine(statusInput{Status: s}, defaultLayout, 40, th), fg(th.warning).Render("unsandboxed"))
}

func TestStatusLineDimsTheTaskCount(t *testing.T) {
	th := mocha()
	assert.Contains(t, statusLine(statusInput{Status: app.Status{Shells: 1}}, defaultLayout, 40, th), fg(th.dim).Render("1 shell"))
}

func TestStatusLineShowsCwdUnderHome(t *testing.T) {
	th := mocha()
	layout := statusLayout{left: []string{"cwd"}, separator: " · "}
	for _, tc := range []struct{ workspace, home, want string }{
		{"/home/u/src/kopi", "/home/u", "~/src/kopi"},
		{"/home/u", "/home/u", "~"},
		{"/home/userx/src", "/home/u", "/home/userx/src"},
		{"/srv/kopi", "/home/u", "/srv/kopi"},
		{"/srv/kopi", "", "/srv/kopi"},
	} {
		in := statusInput{Status: app.Status{Workspace: tc.workspace}, home: tc.home}
		assert.Contains(t, statusLine(in, layout, 40, th), fg(th.muted).Render(tc.want))
	}
	assert.Empty(t, strings.TrimSpace(statusLine(statusInput{}, layout, 40, th)))
}

func TestStatusLineHidesAnEmptyModel(t *testing.T) {
	th := mocha()
	got := statusLine(statusInput{Status: app.Status{Branch: "main"}}, statusLayout{left: []string{"model", "branch"}, separator: " | "}, 20, th)
	assert.Equal(t, "  main", strings.TrimRight(ansi.Strip(got), " "))
}

func TestStatusLineSeparator(t *testing.T) {
	th := mocha()
	in := statusInput{Status: app.Status{Model: "a/b", Branch: "main"}}
	got := statusLine(in, statusLayout{left: []string{"model", "branch"}, separator: " | "}, 20, th)
	assert.Equal(t, "  b | main", strings.TrimRight(ansi.Strip(got), " "))
	assert.Contains(t, got, fg(th.dim).Render(" | "))
	got = statusLine(in, statusLayout{left: []string{"model", "branch"}}, 20, th)
	assert.Equal(t, "  bmain", strings.TrimRight(ansi.Strip(got), " "))
}

func TestStatusLineUserSegments(t *testing.T) {
	th := mocha()
	layout := statusLayout{
		left: []string{"role", "hex", "plain", "cmd", "styled", "cmdplain"}, separator: " ",
		segments: map[string]config.StatusSegment{
			"role":     {Text: "A", Color: "accent"},
			"hex":      {Text: "B", Color: "#ff0000"},
			"plain":    {Text: "C"},
			"cmd":      {Command: "x", Color: "warning"},
			"styled":   {Command: "x", Color: "warning"},
			"cmdplain": {Command: "x"},
		},
	}
	in := statusInput{commands: map[string]string{"cmd": "D", "cmdplain": "E"}}
	got := statusLine(in, layout, 40, th)
	assert.Contains(t, got, fg(th.accent).Render("A"))
	assert.Contains(t, got, fg(lipgloss.Color("#ff0000")).Render("B"))
	assert.Contains(t, got, fg(th.text).Render("C"))
	assert.Contains(t, got, fg(th.warning).Render("D"))
	assert.Equal(t, "  A B C D E", strings.TrimRight(ansi.Strip(got), " "))

	light := latte()
	assert.Contains(t, statusLine(in, layout, 40, light), fg(light.accent).Render("A"))
}

func TestCleanStatusOutput(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "main\n", "main"},
		{"first line only", "one\ntwo\n", "one"},
		{"carriage return", "one\r\ntwo", "one"},
		{"sgr kept and closed", "\x1b[31mred\x1b[0m ok", "\x1b[31mred\x1b[0m ok\x1b[m"},
		{"cursor move dropped", "a\x1b[2Jb\x1b[10;5Hc", "abc"},
		{"osc dropped", "a\x1b]0;title\x07b\x1b]8;;http://x\x1b\\c", "abc"},
		{"private csi ending in m dropped", "a\x1b[>4;2mb", "ab"},
		{"control characters dropped", "a\x00\x07\tb\x7f", "ab"},
		{"empty", "\n", ""},
		{"only sgr", "\x1b[31m\x1b[0m", ""},
		{"trailing space before reset", "\x1b[31mred \x1b[0m", "\x1b[31mred\x1b[m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanStatusOutput(tc.in))
		})
	}
}
