package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/config"
)

func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return out
}

func TestDialogRows(t *testing.T) {
	th := mocha()
	rule20 := strings.Repeat("─", 20)
	d := dialog{title: "Pick", body: []string{"- a"}, note: "why", options: []string{"Yes", "No"}, sel: 1}
	rows := d.rows(20, th)
	assert.Equal(t, []string{rule20, "  Pick", "  - a", "  why", "", "  1. Yes", "❭ 2. No", rule20}, plainRows(rows))
	assert.Equal(t, fg(th.frame).Render(rule20), rows[0])

	d.note = ""
	assert.Equal(t, []string{rule20, "  Pick", "  - a", "", "  1. Yes", "❭ 2. No", rule20}, plainRows(d.rows(20, th)))

	d.options = []string{"a long option that has to wrap"}
	for _, r := range d.rows(20, th) {
		assert.LessOrEqual(t, ansi.StringWidth(r), 20, r)
	}
}

func TestDialogKey(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	for _, tt := range []struct {
		name     string
		msg      tea.KeyPressMsg
		sel, n   int
		want     int
		wantPick bool
	}{
		{"up at the top", up, 0, 3, 0, false},
		{"up", up, 2, 3, 1, false},
		{"down at the bottom", down, 2, 3, 2, false},
		{"down", down, 0, 3, 1, false},
		{"enter", enter, 1, 3, 1, true},
		{"digit in range", digit('2'), 0, 3, 1, true},
		{"zero", digit('0'), 2, 3, 2, false},
		{"digit past n", digit('4'), 2, 3, 2, false},
		{"other key", digit('x'), 2, 3, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sel, picked := dialogKey(tt.msg, &km, tt.sel, tt.n)
			assert.Equal(t, tt.want, sel)
			assert.Equal(t, tt.wantPick, picked)
		})
	}
}
