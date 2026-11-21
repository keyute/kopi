package tui

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/quota"
)

var defaultLayout = newStatusLayout(config.DefaultStatusLine())

// layoutOf is a layout of the given entries with the " · " separator.
func layoutOf(left, right []string) statusLayout {
	return newStatusLayout(config.StatusLine{Left: left, Right: right, Separator: " · "})
}

func TestSegmentsMatchConfig(t *testing.T) {
	assert.ElementsMatch(t, config.StatusSegments, slices.Collect(maps.Keys(segments)))
}

func TestStatusLine(t *testing.T) {
	full := app.Status{Model: "anthropic/claude-x", ContextTokens: 18_000, ContextWindow: 100_000, Branch: "main"}
	custom := layoutOf([]string{"$branch"}, []string{"$context", "$mode"})
	agentsShells := layoutOf([]string{"$model"}, []string{"$agents", "$shells", "$mode"})
	noticeLeft := layoutOf([]string{"$notice", "$branch"}, []string{"$mode"})
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
		{"workflows when named", app.Status{Model: "a/b", Workflows: 1}, layoutOf(nil, []string{"$workflows", "$mode"}), "", 30, strings.Repeat(" ", 11) + "1 workflow · plan  "},
		{"workflows are not in the default layout", app.Status{Model: "a/b", Workflows: 1}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 21) + "plan  "},
		{"ultracode", app.Status{Model: "a/b", Ultracode: true}, defaultLayout, "", 30, "  b" + strings.Repeat(" ", 9) + "ultracode · plan  "},
		{"no workflow at zero", app.Status{Model: "a/b"}, defaultLayout, "", 20, "  b" + strings.Repeat(" ", 11) + "plan  "},
		{"agents are not in the default layout", app.Status{Model: "a/b", Agents: 2, Shells: 1}, defaultLayout, "", 40, "  b" + strings.Repeat(" ", 21) + "1 shell · plan  "},
		{"agents and shells", app.Status{Model: "a/b", Agents: 2, Shells: 1}, agentsShells, "", 40, "  b" + strings.Repeat(" ", 10) + "2 agents · 1 shell · plan  "},
		{"singular agent", app.Status{Model: "a/b", Agents: 1}, agentsShells, "", 40, "  b" + strings.Repeat(" ", 21) + "1 agent · plan  "},
		{"no counts at zero", app.Status{Model: "a/b"}, agentsShells, "", 20, "  b" + strings.Repeat(" ", 11) + "plan  "},
		{"tokens", app.Status{Model: "a/b", ContextTokens: 22_100}, layoutOf([]string{"$tokens"}, nil), "", 20, "  22.1k tokens" + strings.Repeat(" ", 6)},
		{"no tokens while unknown", app.Status{Model: "a/b"}, layoutOf([]string{"$tokens", "$model"}, nil), "", 20, "  b" + strings.Repeat(" ", 17)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ansi.Strip(statusLine(statusInput{Status: tc.s, notice: tc.notice}, tc.layout, tc.width, mocha())))
		})
	}
}

func TestStatusLineShowsEffortToolsAndCost(t *testing.T) {
	layout := layoutOf(nil, []string{"$effort", "$tools", "$cost"})
	in := statusInput{Effort: "high", ToolCalls: 12, Cost: 0.424}
	assert.Equal(t, strings.Repeat(" ", 5)+"high · 12 tools · $0.42  ", ansi.Strip(statusLine(in, layout, 30, mocha())))
	assert.Equal(t, "  ", ansi.Strip(statusLine(statusInput{}, layout, 2, mocha())))
}

func TestStatusLineShowsTotalAbsentAtZero(t *testing.T) {
	layout := layoutOf(nil, []string{"$cost", "$total"})
	in := statusInput{Status: app.Status{TotalCost: 1.874}, Cost: 0.424}
	assert.Equal(t, strings.Repeat(" ", 9)+"$0.42 · $1.87  ", ansi.Strip(statusLine(in, layout, 24, mocha())))
	assert.Equal(t, "  ", ansi.Strip(statusLine(statusInput{}, layout, 2, mocha())))
}

func TestStatusLineShowsLimits(t *testing.T) {
	read := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
	limits := func(provider string, windows ...quota.Window) app.Status {
		return app.Status{Model: "openai-codex/gpt-5.2-codex", Limits: quota.Limits{Provider: provider, Windows: windows, At: read}}
	}
	fiveHour := quota.Window{Label: "5h", UsedPercent: 12, ResetsAt: time.Date(2026, 11, 23, 14, 30, 0, 0, time.Local)}
	week := quota.Window{Label: "wk", UsedPercent: 40, ResetsAt: time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local)}
	layout := layoutOf([]string{"$model", "(5h $limit_5h( $reset_5h))", "(wk $limit_wk( $reset_wk))", "(premium $limit_premium( $reset_premium))"}, nil)
	for _, tc := range []struct {
		name string
		s    app.Status
		want string
	}{
		{"two windows", limits("openai-codex", fiveHour, week), "  gpt-5.2-codex · 5h 12% 14:30 · wk 40% Wed 09:00"},
		{"one window", limits("openai-codex", week), "  gpt-5.2-codex · wk 40% Wed 09:00"},
		{"no reset", limits("openai-codex", quota.Window{Label: "premium", UsedPercent: 34}), "  gpt-5.2-codex · premium 34%"},
		{"a window outside the set", limits("openai-codex", quota.Window{Label: "3d", UsedPercent: 5}), "  gpt-5.2-codex"},
		{"no windows", limits("openai-codex"), "  gpt-5.2-codex"},
		{"never read", app.Status{Model: "openai-codex/gpt-5.2-codex"}, "  gpt-5.2-codex"},
		{"another provider", limits("github-copilot", week), "  gpt-5.2-codex"},
		{"model without a provider", app.Status{Model: "local", Limits: quota.Limits{Provider: "openai-codex", Windows: []quota.Window{week}}}, "  local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, strings.TrimRight(ansi.Strip(statusLine(statusInput{Status: tc.s}, layout, 60, mocha())), " "))
		})
	}
}

func TestStatusLineLimitsDrawOnlyTheirValuesInTheFormatsOrder(t *testing.T) {
	read := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
	other := quota.Window{Label: "other", UsedPercent: 99, ResetsAt: time.Date(2026, 11, 23, 11, 0, 0, 0, time.Local)}
	for suffix, label := range map[string]string{"5h": "5h", "wk": "wk", "mo": "mo", "mo_code": "mo code", "premium": "premium", "chat": "chat"} {
		t.Run(suffix, func(t *testing.T) {
			target := quota.Window{Label: label, UsedPercent: 12, ResetsAt: time.Date(2026, 11, 23, 14, 30, 0, 0, time.Local)}
			s := app.Status{Model: "p/m", Limits: quota.Limits{Provider: "p", Windows: []quota.Window{other, target}, At: read}}
			layout := newStatusLayout(config.StatusLine{Left: []string{"$reset_" + suffix + " $limit_" + suffix}})

			assert.Equal(t, "  14:30 12%", strings.TrimRight(ansi.Strip(statusLine(statusInput{Status: s}, layout, 40, mocha())), " "))
		})
	}
}

func TestStatusLineLimitsFollowTheOpenView(t *testing.T) {
	week := quota.Window{Label: "wk", UsedPercent: 40}
	s := app.Status{Limits: quota.Limits{Provider: "openai-codex", Windows: []quota.Window{week}}}
	layout := layoutOf([]string{"(wk $limit_wk)"}, nil)

	s.Model = "openai-codex/a"
	assert.Contains(t, ansi.Strip(statusLine(statusInput{Status: s}, layout, 30, mocha())), "wk 40%")
	s.Model = "xai/b"
	assert.NotContains(t, ansi.Strip(statusLine(statusInput{Status: s}, layout, 30, mocha())), "wk")
}

func TestStatusLineWarnsOnALimitNearlyUsed(t *testing.T) {
	th := mocha()
	layout := layoutOf([]string{"(wk $limit_wk( $reset_wk))"}, nil)
	status := func(used float64) statusInput {
		resets := time.Date(2026, 11, 23, 14, 30, 0, 0, time.Local)
		read := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
		return statusInput{Status: app.Status{Model: "p/m", Limits: quota.Limits{Provider: "p", At: read, Windows: []quota.Window{{Label: "wk", UsedPercent: used, ResetsAt: resets}}}}}
	}

	assert.Contains(t, statusLine(status(89), layout, 40, th), fg(th.text).Render("89%"))
	assert.Contains(t, statusLine(status(90), layout, 40, th), fg(th.warning).Render("90%"))
	assert.Contains(t, statusLine(status(89.6), layout, 40, th), fg(th.warning).Render("90%"), "a window warns by the percentage it shows")

	got := statusLine(status(90), layout, 40, th)
	assert.Contains(t, got, fg(th.text).Render("14:30"))
	assert.Contains(t, got, "wk")
	assert.NotContains(t, got, fg(th.warning).Render("14:30"))
	assert.NotContains(t, got, fg(th.warning).Render("wk"))
}

func TestResetText(t *testing.T) {
	at := func(day, hour, min int) time.Time { return time.Date(2026, 11, day, hour, min, 0, 0, time.Local) }
	read := at(23, 10, 0)
	for _, tc := range []struct {
		name   string
		resets time.Time
		want   string
	}{
		{"unknown", time.Time{}, ""},
		{"later today", at(23, 14, 30), "14:30"},
		{"just under a day", at(24, 9, 59), "09:59"},
		{"a day", at(24, 10, 0), "Tue 10:00"},
		{"under a week", at(30, 9, 59), "Mon 09:59"},
		{"a week", at(30, 10, 0), "Nov 30"},
		{"next month", time.Date(2026, 12, 1, 0, 0, 0, 0, time.Local), "Dec 1"},
		{"already passed", at(23, 9, 0), "09:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resetText(tc.resets, read))
		})
	}
}

func TestStatusInputKeepsTheTotalWithASubagentOpen(t *testing.T) {
	m, c := agentLineModel(t, 60, config.DefaultAgentLine(), explorer("t1"))
	c.status.TotalCost = 3
	send(m, event.TasksChanged{})
	assert.Equal(t, 3.0, m.statusInput().TotalCost)

	display(m, "t1")
	assert.Equal(t, 3.0, m.statusInput().TotalCost)
}

func TestStatusInputTakesTheOpenSubagentsEffortToolsAndCost(t *testing.T) {
	sub := explorer("t1")
	sub.Effort, sub.ToolCalls, sub.Cost = "low", 3, 0.5
	m, c := agentLineModel(t, 60, config.DefaultAgentLine(), sub)
	c.agents[0].Effort, c.agents[0].ToolCalls, c.agents[0].Cost = "high", 12, 2
	send(m, event.TasksChanged{})

	in := m.statusInput()
	assert.Equal(t, []any{"high", 12, 2.0}, []any{in.Effort, in.ToolCalls, in.Cost})

	display(m, "t1")
	in = m.statusInput()
	assert.Equal(t, []any{"low", 3, 0.5}, []any{in.Effort, in.ToolCalls, in.Cost})
}

func TestStatusLineFitsNarrowWidthsAndKeepsMode(t *testing.T) {
	for _, layout := range []statusLayout{
		defaultLayout,
		layoutOf(nil, []string{"$branch", "$mode", "$notice"}),
		layoutOf([]string{"$mode"}, []string{"$branch"}),
		layoutOf(nil, []string{"$branch", "($sandbox )$mode"}),
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

func TestStatusLineKeepsTheWholeModeEntryAtNarrowWidths(t *testing.T) {
	layout := layoutOf([]string{"$model"}, []string{"$branch", "$sandbox $mode"})
	s := app.Status{Model: "a/b", Branch: strings.Repeat("x", 30), Shell: true, Sandboxed: true, Mode: policy.Execute}
	for _, tc := range []struct {
		width int
		want  string
	}{
		{40, strings.Repeat("x", 20) + " · sandboxed execute"},
		{25, strings.Repeat("x", 5) + " · sandboxed execute"},
		{20, "   sandboxed execute"},
		{17, "sandboxed execute"},
		{10, strings.Repeat("x", 10)},
	} {
		got := ansi.Strip(statusLine(statusInput{Status: s}, layout, tc.width, mocha()))
		assert.Equal(t, tc.want, got, "width %d", tc.width)
	}
}

func TestStatusLineKeepsTheRightModeEntryWhenModeIsOnBothSides(t *testing.T) {
	layout := layoutOf([]string{"$mode"}, []string{"$branch", "$sandbox $mode"})
	s := app.Status{Branch: strings.Repeat("x", 30), Shell: true, Sandboxed: true, Mode: policy.Execute}
	got := ansi.Strip(statusLine(statusInput{Status: s}, layout, 25, mocha()))
	assert.Equal(t, strings.Repeat("x", 5)+" · sandboxed execute", got)
}

func TestStatusLineFormats(t *testing.T) {
	branchUnit := layoutOf([]string{"$branch( $changes)"}, nil)
	for _, tc := range []struct {
		name   string
		layout statusLayout
		s      statusInput
		want   string
	}{
		{"branch with changes", branchUnit, statusInput{Status: app.Status{Branch: "main"}, added: 12, removed: 3}, "  main +12 -3"},
		{"branch clean", branchUnit, statusInput{Status: app.Status{Branch: "main"}}, "  main"},
		{"no branch", layoutOf([]string{"$branch( $changes)", "$model"}, nil), statusInput{Status: app.Status{Model: "a/b"}}, "  b"},
		{"only added", layoutOf([]string{"$changes"}, nil), statusInput{added: 12}, "  +12"},
		{"only removed", layoutOf([]string{"$changes"}, nil), statusInput{removed: 3}, "  -3"},
		{"label in parentheses hides with its value", layoutOf([]string{"(ctx $context)", "$model"}, nil), statusInput{Status: app.Status{Model: "a/b"}}, "  b"},
		{"label in parentheses shows with its value", layoutOf([]string{"(ctx $context)"}, nil), statusInput{Status: app.Status{ContextTokens: 1, ContextWindow: 4}}, "  ctx 25.0%"},
		{"label outside parentheses always shows", layoutOf([]string{"ctx $context"}, nil), statusInput{}, "  ctx"},
		{"adjacent segments", layoutOf([]string{"$model$branch"}, nil), statusInput{Status: app.Status{Model: "a/b", Branch: "main"}}, "  bmain"},
		{"nested groups", layoutOf([]string{"((a$model)$branch)"}, nil), statusInput{Status: app.Status{Branch: "main"}}, "  main"},
		{"escapes", layoutOf([]string{`\$\(\)\\$model`}, nil), statusInput{Status: app.Status{Model: "a/b"}}, `  $()\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, strings.TrimRight(ansi.Strip(statusLine(tc.s, tc.layout, 40, mocha())), " "))
		})
	}
}

func TestStatusLineColoursChanges(t *testing.T) {
	th := mocha()
	got := statusLine(statusInput{added: 12, removed: 3}, layoutOf([]string{"$changes"}, nil), 40, th)
	assert.Contains(t, got, fg(th.success).Render("+12")+" "+fg(th.error).Render("-3"))
}

func TestStatusLineColoursLiteralText(t *testing.T) {
	th := mocha()
	got := statusLine(statusInput{Status: app.Status{Model: "a/b"}}, layoutOf([]string{"at $model"}, nil), 40, th)
	assert.Contains(t, got, fg(th.text).Render("at "))
}

func TestStatusLineDropsAnEntryWholeAtANarrowWidth(t *testing.T) {
	layout := layoutOf([]string{"$model", "$branch( $changes)"}, nil)
	in := statusInput{Status: app.Status{Model: "a/b", Branch: "main"}, added: 12, removed: 3}
	assert.Equal(t, "  b · main +12 -3", strings.TrimRight(ansi.Strip(statusLine(in, layout, 20, mocha())), " "))
	assert.Equal(t, "  b", strings.TrimRight(ansi.Strip(statusLine(in, layout, 15, mocha())), " "))
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

func TestStatusLineDimsTheTaskCounts(t *testing.T) {
	th := mocha()
	layout := layoutOf(nil, []string{"$agents", "$shells", "$workflows"})
	got := statusLine(statusInput{Status: app.Status{Agents: 2, Shells: 1, Workflows: 1}}, layout, 40, th)
	assert.Contains(t, got, fg(th.dim).Render("2 agents"))
	assert.Contains(t, got, fg(th.dim).Render("1 shell"))
	assert.Contains(t, got, fg(th.dim).Render("1 workflow"))
}

func TestStatusLineShowsCwdUnderHome(t *testing.T) {
	th := mocha()
	layout := layoutOf([]string{"$cwd"}, nil)
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
	got := statusLine(statusInput{Status: app.Status{Branch: "main"}}, newStatusLayout(config.StatusLine{Left: []string{"$model", "$branch"}, Separator: " | "}), 20, th)
	assert.Equal(t, "  main", strings.TrimRight(ansi.Strip(got), " "))
}

func TestStatusLineSeparator(t *testing.T) {
	th := mocha()
	in := statusInput{Status: app.Status{Model: "a/b", Branch: "main"}}
	got := statusLine(in, newStatusLayout(config.StatusLine{Left: []string{"$model", "$branch"}, Separator: " | "}), 20, th)
	assert.Equal(t, "  b | main", strings.TrimRight(ansi.Strip(got), " "))
	assert.Contains(t, got, fg(th.dim).Render(" | "))
	got = statusLine(in, newStatusLayout(config.StatusLine{Left: []string{"$model", "$branch"}}), 20, th)
	assert.Equal(t, "  bmain", strings.TrimRight(ansi.Strip(got), " "))
}

func TestStatusLineUserSegments(t *testing.T) {
	th := mocha()
	layout := newStatusLayout(config.StatusLine{
		Left: []string{"$role", "$hex", "$plain", "$cmd", "$styled", "$cmdplain"}, Separator: " ",
		Segments: map[string]config.StatusSegment{
			"role":     {Text: "A", Color: "accent"},
			"hex":      {Text: "B", Color: "#ff0000"},
			"plain":    {Text: "C"},
			"cmd":      {Command: "x", Color: "warning"},
			"styled":   {Command: "x", Color: "warning"},
			"cmdplain": {Command: "x"},
		},
	})
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
