package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// usageDialog is the /usage dialog, drawn in the composer's slot.
type usageDialog struct {
	d  *dialog
	km *keyMap
	u  app.Usage
	// body is u as rows for bodyWidth and bodyTheme, kept across renders;
	// expired is the answer about the cache's lifetime it was built with.
	body      []string
	bodyWidth int
	bodyTheme theme
	expired   bool
}

// showUsage opens the dialog on what the session has used.
func (m *model) showUsage() tea.Cmd {
	m.open(promptUsage, &usageDialog{d: &dialog{title: "Usage"}, km: &m.keys, u: m.c.Usage()})
	return nil
}

// key closes on dismiss; every other key is swallowed.
func (u *usageDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	return nil, !key.Matches(msg, m.keys.dismiss)
}

// seen ends the dialog with a session switch. Anything that changes what it shows
// has it read the usage again.
func (u *usageDialog) seen(m *model, msg tea.Msg) bool {
	switch msg.(type) {
	case event.SessionChanged:
		return false
	case event.TurnEnd, event.ToolEnd, event.TasksChanged, event.LimitsChanged, event.ModelChanged, event.SessionNamed:
		u.u, u.bodyWidth = m.c.Usage(), 0
	}
	return true
}

func (u *usageDialog) dialog(width int, th theme) *dialog {
	now := time.Now()
	expired := !u.u.Cache.Expires.IsZero() && !now.Before(u.u.Cache.Expires)
	if u.bodyWidth != width || u.bodyTheme != th || u.expired != expired {
		u.body, u.bodyWidth, u.bodyTheme, u.expired = usageBody(u.u, width-2, th, now), width, th, expired
	}
	u.d.body, u.d.hint = u.body, scrollHint(u.km)
	return u.d
}

// usageBody is the dialog's rows for width: the bars, the models and the facts,
// a group with no rows left out.
func usageBody(u app.Usage, width int, th theme, now time.Time) []string {
	bars := usageBars(u)
	facts := usageFacts(u, th, now)
	col := 0
	for _, b := range bars {
		col = max(col, lipgloss.Width(b.label))
	}
	for _, f := range facts {
		col = max(col, lipgloss.Width(f.label))
	}
	col += 2
	label := func(s string) string { return fg(th.muted).Render(s) + strings.Repeat(" ", col-lipgloss.Width(s)) }

	var bar, fact []string
	for _, b := range bars {
		row := label(b.label) + percentBar(b.pct, b.warnAt, th)
		if b.tail != "" {
			row += "  " + b.tail
		}
		bar = append(bar, row)
	}
	for _, f := range facts {
		fact = append(fact, label(f.label)+f.value)
	}
	body := []string{""}
	for _, group := range [][]string{bar, usageModels(u, width), fact} {
		if len(group) == 0 {
			continue
		}
		if len(body) > 1 {
			body = append(body, "")
		}
		body = append(body, group...)
	}
	for i, row := range body {
		body[i] = cut(row, width)
	}
	return body
}

type usageBar struct {
	label string
	pct   float64
	// warnAt is the percent from which the bar warns.
	warnAt int
	tail   string
}

// usageBars is the context and, when the plan limits are the session model's
// provider's, a bar for each window.
func usageBars(u app.Usage) []usageBar {
	var bars []usageBar
	if u.ContextWindow > 0 {
		bars = append(bars, usageBar{
			label: "Context", pct: float64(u.ContextTokens) * 100 / float64(u.ContextWindow), warnAt: contextWarnPercent,
			tail: compactCount(u.ContextTokens) + " / " + compactCount(u.ContextWindow),
		})
	}
	if l := u.Limits; l.Provider != "" && l.Provider == modelProvider(u.Model) {
		for _, w := range l.Windows {
			b := usageBar{label: w.Label, pct: w.UsedPercent, warnAt: limitWarnPercent}
			if text := resetText(w.ResetsAt, l.At); text != "" {
				b.tail = "resets " + text
			}
			bars = append(bars, b)
		}
	}
	return bars
}

// percentBar is pct as a bar of barCells cells and the rounded percent in four
// cells, both in the warning colour from warnAt percent; over 100 fills the bar.
func percentBar(pct float64, warnAt int, th theme) string {
	c := th.text
	if math.Round(pct) >= float64(warnAt) {
		c = th.warning
	}
	filled := min(barCells, max(0, int(math.Round(pct*barCells/100))))
	return fg(c).Render(strings.Repeat("▰", filled)) + fg(th.dim).Render(strings.Repeat("▱", barCells-filled)) +
		" " + fg(c).Render(fmt.Sprintf("%4s", fmt.Sprintf("%.0f%%", pct)))
}

// usageModels is a row for each model and the total, with the names cut to
// leave the numbers whole within width.
func usageModels(u app.Usage, width int) []string {
	if len(u.Models) == 0 {
		return nil
	}
	type row struct{ name, tokens, cost string }
	var rows []row
	total := 0
	for _, m := range u.Models {
		name := m.Model
		if name == "" {
			name = "summaries"
		}
		total += m.Tokens
		rows = append(rows, row{name, compactCount(m.Tokens), fmt.Sprintf("$%.2f", m.Cost)})
	}
	rows = append(rows, row{"Total (estimate)", compactCount(total), fmt.Sprintf("$%.2f", u.TotalCost)})
	var name, tokens, cost int
	for _, r := range rows {
		name, tokens, cost = max(name, lipgloss.Width(r.name)), max(tokens, len(r.tokens)), max(cost, len(r.cost))
	}
	name = min(name, max(1, width-tokens-cost-6))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		n := cut(r.name, name)
		out = append(out, n+strings.Repeat(" ", name-lipgloss.Width(n))+"   "+
			fmt.Sprintf("%*s", tokens, r.tokens)+"   "+fmt.Sprintf("%*s", cost, r.cost))
	}
	return out
}

type usageFact struct{ label, value string }

// usageFacts is the Tokens, Cache and Session rows that have something to say.
func usageFacts(u app.Usage, th theme, now time.Time) []usageFact {
	sep := fg(th.muted).Render(" · ")
	var facts []usageFact
	if t := u.Tokens; t != (app.Tokens{}) {
		facts = append(facts, usageFact{"Tokens", strings.Join([]string{
			compactCount(t.Input) + " in", compactCount(t.Output) + " out", compactCount(t.CacheRead) + " read", compactCount(t.CacheWrite) + " write",
		}, sep)})
	}
	var cache []string
	c := u.Cache
	if c.Reported {
		cache = append(cache, fmt.Sprintf("%.0f%% hit", c.Hit))
	}
	if !c.Expires.IsZero() {
		if now.Before(c.Expires) {
			cache = append(cache, "warm until "+clockText(c.Expires, now))
		} else {
			cache = append(cache, fg(th.warning).Render("expired "+clockText(c.Expires, now)))
		}
	}
	if c.Misses > 0 {
		noun := "misses"
		if c.Misses == 1 {
			noun = "miss"
		}
		cache = append(cache, fmt.Sprintf("%d %s $%.2f", c.Misses, noun, c.MissedCost))
	}
	if len(cache) > 0 {
		facts = append(facts, usageFact{"Cache", strings.Join(cache, sep)})
	}
	var session []string
	if u.SessionID != "" {
		session = append(session, u.SessionID)
	}
	if u.Messages > 0 {
		session = append(session, countText(u.Messages, "message"))
	}
	if u.ToolCalls > 0 {
		session = append(session, countText(u.ToolCalls, "tool call"))
	}
	if len(session) > 0 {
		facts = append(facts, usageFact{"Session", strings.Join(session, sep)})
	}
	return facts
}

// clockText is t as a local time of day when it is today, else with the date.
func clockText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
