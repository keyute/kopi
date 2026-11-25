package tui

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/quota"
)

// usageFixture is a session on a plan-limited model with everything to show,
// read now. Run it inside usageBubble.
func usageFixture() app.Usage {
	now := time.Now()
	return app.Usage{
		SessionID: "0195c2d4", Messages: 42, ToolCalls: 128, Model: "openai-codex/gpt-5-codex",
		ContextTokens: 84_200, ContextWindow: 1_000_000,
		Context:   app.Context{SystemPrompt: 1_900, Tools: 8_100, Instructions: 6_200, Skills: 1_800, Agents: 1_300, MCP: 3_000, Messages: 61_900},
		CompactAt: 983_616, Compactions: 2, ModelTime: 12*time.Minute + 40*time.Second, ToolTime: 3*time.Minute + 5*time.Second,
		Limits: quota.Limits{Provider: "openai-codex", At: now, Windows: []quota.Window{
			{Label: "5h", UsedPercent: 23, ResetsAt: now.Add(3*time.Hour + 40*time.Minute)},
			{Label: "wk", UsedPercent: 52, ResetsAt: now.Add(4*24*time.Hour + 21*time.Hour)},
		}},
		Tokens: app.Tokens{Input: 502_000, Output: 58_000, CacheRead: 940_000, CacheWrite: 12_000},
		Models: []app.ModelUsage{
			{Model: "openai-codex/gpt-5-codex", Tokens: 1_200_000, Cost: 3.90},
			{Model: "openai-codex/gpt-5-mini", Tokens: 310_000, Cost: 0.41},
		},
		Cache: app.Cache{Reported: true, Hit: 91, Misses: 2, MissedCost: 0.42, Expires: now.Add(3*time.Hour + 42*time.Minute)},
	}
}

// usageBubble runs f in a synctest bubble with the clock moved to noon local
// time, so a few hours either way stay in the day.
func usageBubble(t *testing.T, f func(t *testing.T)) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().Local()
		noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
		if noon.Before(now) {
			noon = noon.AddDate(0, 0, 1)
		}
		time.Sleep(noon.Sub(now))
		f(t)
	})
}

// usageRows is a screen tall enough to show the whole fixture unscrolled.
const usageRows = 90

// usageModel has /usage ready to open on u, or on usageFixture when u is nil.
func usageModel(t *testing.T, width int, u *app.Usage) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, width, usageRows)
	c.usage = usageFixture()
	if u != nil {
		c.usage = *u
	}
	c.submitErr = app.ErrShowUsage
	return m, c
}

// clock is the local time d from now, in layout.
func clock(d time.Duration, layout string) string {
	return time.Now().Add(d).Local().Format(layout)
}

func openUsage(t *testing.T, m *model) []string {
	t.Helper()
	submitText(m, "/usage")
	return dialogRows(t, m)
}

func usageOpen(t *testing.T, m *model) *usageDialog {
	t.Helper()
	for _, h := range m.prompts {
		if u, ok := h.prompt.(*usageDialog); ok {
			return u
		}
	}
	require.FailNow(t, "no /usage dialog is open")
	return nil
}

func TestUsageShowsEveryRow(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, c := usageModel(t, 70, nil)
		resets, warm := clock(3*time.Hour+40*time.Minute, "15:04"), clock(3*time.Hour+42*time.Minute, "15:04")
		week := clock(4*24*time.Hour+21*time.Hour, "Mon 15:04")
		typeText(m, "/usage")
		assert.NotNil(t, press(m, enter), "the cache's wake")

		rule := strings.Repeat("─", 70)
		assert.Equal(t, []string{
			rule,
			"  Usage",
			"",
			"  Context     ▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱   8%  84.2k / 1.0m",
			"  5h          ▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱  23%  resets " + resets,
			"  wk          ▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱  52%  resets " + week,
			"",
			"  System prompt    1.9k   0.2%",
			"  Tools            8.1k   0.8%",
			"  Instructions     6.2k   0.6%",
			"  Skills           1.8k   0.2%",
			"  Agents           1.3k   0.1%",
			"  MCP              3.0k   0.3%",
			"  Messages        61.9k   6.2%",
			"",
			"  openai-codex/gpt-5-codex     1.2m   $3.90",
			"  openai-codex/gpt-5-mini    310.0k   $0.41",
			"  Total (estimate)             1.5m   $4.31",
			"",
			"  Tokens      502.0k in · 58.0k out · 940.0k read · 12.0k write",
			"  Cache       91% hit · warm until " + warm + " · 2 misses $0.42",
			"  Compaction  2 done · next at 983.6k",
			"  Time        12m 40s model · 3m 5s tools",
			"  Session     0195c2d4 · 42 messages · 128 tool calls",
			"",
			"  esc to close",
			rule,
		}, dialogRows(t, m))
		assert.Empty(t, m.composer.value(), "the draft is accepted")
		assert.Equal(t, 1, c.usageReads)
		s := screen(m)
		assert.Len(t, s, usageRows)
		assert.Equal(t, rule, s[len(s)-1], "the status line is hidden")
	})
}

func TestUsageLeavesOutWhatItHasNothingFor(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(u *app.Usage)
		absent []string
	}{
		"no context window":          {func(u *app.Usage) { u.ContextWindow = 0 }, []string{"Context", "System prompt", "Messages"}},
		"limits of another provider": {func(u *app.Usage) { u.Limits.Provider = "xai" }, []string{"5h ", "wk "}},
		"no models":                  {func(u *app.Usage) { u.Models = nil }, []string{"Total", "gpt-5"}},
		"no tokens":                  {func(u *app.Usage) { u.Tokens = app.Tokens{} }, []string{"Tokens"}},
		"no cache parts":             {func(u *app.Usage) { u.Cache = app.Cache{} }, []string{"Cache"}},
		"zero counts":                {func(u *app.Usage) { u.Messages, u.ToolCalls = 0, 0 }, []string{"messages", "tool calls"}},
		"no context tokens":          {func(u *app.Usage) { u.ContextTokens, u.Context = 0, app.Context{} }, []string{"System prompt", "Messages"}},
		"an empty category":          {func(u *app.Usage) { u.Context.MCP = 0 }, []string{"MCP"}},
		"no compaction parts":        {func(u *app.Usage) { u.CompactAt, u.Compactions = 0, 0 }, []string{"Compaction"}},
		"no compaction yet":          {func(u *app.Usage) { u.Compactions = 0 }, []string{"done"}},
		"no time":                    {func(u *app.Usage) { u.ModelTime, u.ToolTime = 0, 0 }, []string{"Time"}},
		"no tool time":               {func(u *app.Usage) { u.ToolTime = 999 * time.Millisecond }, []string{" tools"}},
	} {
		t.Run(name, func(t *testing.T) {
			usageBubble(t, func(t *testing.T) {
				u := usageFixture()
				tc.change(&u)
				m, _ := usageModel(t, 60, &u)

				text := strings.Join(openUsage(t, m), "\n")

				for _, s := range tc.absent {
					assert.NotContains(t, text, s)
				}
				assert.NotContains(t, text, "\n\n\n", "no group leaves its blank behind")
			})
		})
	}
}

func TestUsageWithNothingUsedShowsTheSession(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 60, &app.Usage{SessionID: "0195c2d4", Model: "openai-codex/gpt-5-codex"})

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{rule, "  Usage", "", "  Session  0195c2d4", "", "  esc to close", rule}, openUsage(t, m))
	})
}

func TestUsageWithoutLimitsOrCacheLifetime(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := usageFixture()
		u.Model, u.Limits, u.Cache.Expires = "mock/model-1", quota.Limits{}, time.Time{}
		u.Models = []app.ModelUsage{{Model: "mock/model-1", Tokens: 1_500, Cost: 0.0049}, {Model: "", Tokens: 20, Cost: 0}}
		u.Context, u.CompactAt, u.Compactions, u.ModelTime, u.ToolTime = app.Context{Messages: 84_200}, 0, 0, 0, 0
		m, _ := usageModel(t, 60, &u)

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{
			rule,
			"  Usage",
			"",
			"  Context  ▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱   8%  84.2k / 1.0m",
			"",
			"  Messages   84.2k   8.4%",
			"",
			"  mock/model-1       1.5k   $0.00",
			"  summaries            20   $0.00",
			"  Total (estimate)   1.5k   $0.00",
			"",
			"  Tokens   502.0k in · 58.0k out · 940.0k read · 12.0k write",
			"  Cache    91% hit · 2 misses $0.42",
			"  Session  0195c2d4 · 42 messages · 128 tool calls",
			"",
			"  esc to close",
			rule,
		}, openUsage(t, m))
	})
}

func TestUsageWindowOmitsAnUnknownReset(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := usageFixture()
		u.Limits.Windows[0].ResetsAt = time.Time{}
		m, _ := usageModel(t, 60, &u)

		assert.Equal(t, "  5h          ▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱  23%", openUsage(t, m)[4])
	})
}

func TestUsageContextShowsATinyShareAsUnderATenth(t *testing.T) {
	u := app.Usage{ContextTokens: 84_200, ContextWindow: 1_000_000, Context: app.Context{Agents: 400, Messages: 83_800}}
	rows := usageContext(u, 60, mocha())
	require.Len(t, rows, 2)
	assert.Equal(t, "Agents       400   <0.1%", ansi.Strip(rows[0]))
	assert.Equal(t, "Messages   83.8k    8.4%", ansi.Strip(rows[1]))
}

func TestPercentBarWarnsWhenAsked(t *testing.T) {
	th := mocha()
	cells := func(n int) string { return strings.Repeat("▰", n) }

	assert.True(t, strings.HasPrefix(percentBar(89.5, false, th), fg(th.text).Render(cells(14))))
	assert.True(t, strings.HasPrefix(percentBar(89.5, true, th), fg(th.warning).Render(cells(14))))
	assert.Contains(t, percentBar(95, true, th), fg(th.warning).Render(" 95%"))
	assert.Contains(t, percentBar(80, false, th), fg(th.text).Render(" 80%"))
	assert.Equal(t, fg(th.warning).Render(cells(barCells))+fg(th.dim).Render("")+" "+fg(th.warning).Render("150%"), percentBar(150, true, th), "over 100 fills the bar")
	assert.Equal(t, fg(th.text).Render("")+fg(th.dim).Render(strings.Repeat("▱", barCells))+" "+fg(th.text).Render("  0%"), percentBar(0, false, th))
}

func TestUsageBarsWarnAsTheFooterDoes(t *testing.T) {
	u := usageFixture()
	u.ContextTokens, u.ContextWindow = 896, 1000
	u.Limits.Windows[0].UsedPercent = 89.4
	u.Limits.Windows[1].UsedPercent = 89.5

	bars := usageBars(u)
	require.Len(t, bars, 3)
	assert.False(t, bars[0].warn, "context warns on the exact share, as contextText does")
	assert.False(t, bars[1].warn, "89.4% rounds to 89%")
	assert.True(t, bars[2].warn, "89.5% rounds to 90%, as limitSegment does")
}

func TestUsageDrawsAWarningWindowInTheWarningColour(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := usageFixture()
		u.Limits.Windows[1].UsedPercent = 91
		m, _ := usageModel(t, 60, &u)
		submitText(m, "/usage")

		rows := strings.Split(m.View().Content, "\n")
		var wk string
		for _, r := range rows {
			if strings.Contains(ansi.Strip(r), "wk ") {
				wk = r
			}
		}
		assert.Contains(t, wk, fg(m.th.warning).Render(" 91%"))
	})
}

func TestUsageCacheRow(t *testing.T) {
	for name, tc := range map[string]struct {
		cache func(now time.Time) app.Cache
		want  string
	}{
		"warm":        {func(now time.Time) app.Cache { return app.Cache{Expires: now.Add(time.Minute)} }, "  Cache    warm until 12:01"},
		"expired":     {func(now time.Time) app.Cache { return app.Cache{Expires: now.Add(-2 * time.Minute)} }, "  Cache    expired 11:58"},
		"no lifetime": {func(time.Time) app.Cache { return app.Cache{Reported: true, Hit: 40.4} }, "  Cache    40% hit"},
		"one miss":    {func(time.Time) app.Cache { return app.Cache{Misses: 1, MissedCost: 1.234} }, "  Cache    1 miss $1.23"},
		"two misses":  {func(time.Time) app.Cache { return app.Cache{Misses: 2, MissedCost: 0.5} }, "  Cache    2 misses $0.50"},
		"hit and no misses": {func(now time.Time) app.Cache {
			return app.Cache{Reported: true, Hit: 100, Expires: now.Add(time.Minute)}
		}, "  Cache    100% hit · warm until 12:01"},
	} {
		t.Run(name, func(t *testing.T) {
			usageBubble(t, func(t *testing.T) {
				u := app.Usage{SessionID: "s", Cache: tc.cache(time.Now())}
				m, _ := usageModel(t, 60, &u)

				rows := openUsage(t, m)

				assert.Contains(t, rows, tc.want)
			})
		})
	}
}

func TestUsageCacheRowNamesTheDateWhenNotToday(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := app.Usage{SessionID: "s", Cache: app.Cache{Expires: time.Now().Add(13 * time.Hour)}}
		m, _ := usageModel(t, 60, &u)

		assert.Contains(t, openUsage(t, m), "  Cache    warm until "+clock(13*time.Hour, "Jan 2")+" 01:00")
	})
}

func TestUsageCacheTurnsExpiredWithTheClock(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := app.Usage{SessionID: "s", Cache: app.Cache{Expires: time.Now().Add(time.Minute)}}
		m, c := usageModel(t, 60, &u)
		assert.Contains(t, openUsage(t, m), "  Cache    warm until 12:01")
		assert.Equal(t, u.Cache.Expires, usageOpen(t, m).wakeAt, "opening arms the wake")
		assert.Nil(t, m.usageWake(), "one wake per expiry")

		time.Sleep(2 * time.Minute)
		m.Update(usageWakeMsg{})

		assert.Contains(t, dialogRows(t, m), "  Cache    expired 12:01")
		assert.Equal(t, 1, c.usageReads, "the clock alone reads nothing")
		assert.Nil(t, m.usageWake(), "an expired cache arms nothing")
	})
}

func TestUsageWakeFollowsAnExpiryReadAfterOpening(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, c := usageModel(t, 60, &app.Usage{SessionID: "s"})
		openUsage(t, m)
		assert.True(t, usageOpen(t, m).wakeAt.IsZero(), "no cache lifetime: nothing to wake for")

		c.usage.Cache.Expires = time.Now().Add(5 * time.Minute)
		send(m, event.TurnEnd{})

		assert.Equal(t, c.usage.Cache.Expires, usageOpen(t, m).wakeAt)
	})
}

func TestUsageWakeTicksAtTheExpiry(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 60, &app.Usage{SessionID: "s"})
		openUsage(t, m)
		expires := time.Now().Add(5 * time.Minute)
		usageOpen(t, m).u.Cache.Expires = expires

		wake := m.usageWake()
		require.NotNil(t, wake)

		assert.Equal(t, usageWakeMsg{}, wake())
		assert.True(t, time.Now().Equal(expires), "woke at %v, not %v", time.Now(), expires)
	})
}

func TestUsageCutsTheModelNameAndKeepsTheNumbers(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 40, nil)

		rows := openUsage(t, m)

		assert.Equal(t, []string{
			"  openai-codex/gpt-5-c…     1.2m   $3.90",
			"  openai-codex/gpt-5-m…   310.0k   $0.41",
			"  Total (estimate)          1.5m   $4.31",
		}, rows[15:18])
	})
}

func TestUsageReadsAgainOnWhatChangesIt(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := usageFixture()
		u.Limits = quota.Limits{}
		m, c := usageModel(t, 60, &u)
		assert.NotContains(t, strings.Join(openUsage(t, m), "\n"), "5h")

		for i, ev := range []event.Event{event.TurnStart{}, event.ModeChanged{}, event.LimitsChanged{}, event.TurnEnd{}, event.ToolEnd{}, event.TasksChanged{}, event.ModelChanged{Model: "openai-codex/gpt-5-codex"}, event.SessionNamed{Name: "usage"}} {
			c.usage = usageFixture()
			c.usage.Tokens.Input = 1_000 * (i + 1)
			send(m, ev)

			assert.NotNil(t, usageOf(m), "%T keeps it open", ev)
			assert.Equal(t, i+2, c.usageReads)
			assert.Contains(t, strings.Join(dialogRows(t, m), "\n"), compactCount(1_000*(i+1))+" in")
		}
		assert.Contains(t, strings.Join(dialogRows(t, m), "\n"), "  5h          ▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱  23%  resets 15:40")
	})
}

func TestUsageKeys(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 60, nil)
		rows := openUsage(t, m)

		for _, k := range []tea.KeyPressMsg{enter, {Code: 'x'}, {Code: tea.KeyDown}, {Code: '1'}} {
			press(m, k)
			assert.NotNil(t, usageOf(m), "%v is swallowed", k)
			assert.Equal(t, rows, dialogRows(t, m))
		}
		assert.Empty(t, m.composer.value())

		press(m, escape)

		assert.Nil(t, usageOf(m))
		assert.NotContains(t, screen(m), "  Usage")
	})
}

func TestSessionChangeClosesUsage(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 60, nil)
		submitText(m, "/usage")

		send(m, event.SessionChanged{SessionID: "s2"})

		assert.Nil(t, usageOf(m))
		assert.NotContains(t, screen(m), "  Usage")
	})
}

func TestUsageYieldsToAnApproval(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		m, _ := usageModel(t, 60, nil)
		submitText(m, "/usage")

		send(m, event.ApprovalRequested{Token: 1, CallID: "c1", Title: "ls"})

		require.NotNil(t, approvalOf(m))
		assert.NotNil(t, usageOf(m), "covered, not closed")
	})
}

func TestUsageCutsEveryRowToTheWidth(t *testing.T) {
	usageBubble(t, func(t *testing.T) {
		u := usageFixture()
		u.SessionID = "0195c2d4-7a3e-7b1c-9d2f-4e5a6b7c8d9e"
		m, _ := usageModel(t, 40, &u)

		rows := openUsage(t, m)

		for _, r := range rows {
			assert.LessOrEqual(t, lipgloss.Width(r), 40, r)
		}
		assert.True(t, strings.HasSuffix(rows[len(rows)-4], "…"), "the session row is cut")
		assert.Contains(t, rows, "  openai-codex/gpt-5-c…     1.2m   $3.90")
		assert.Contains(t, rows, "  Total (estimate)          1.5m   $4.31")
	})
}
