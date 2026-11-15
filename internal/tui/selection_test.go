package tui

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
)

func TestSelectionSpanCoversAnchorToHead(t *testing.T) {
	for _, s := range []selection{
		{anchor: point{1, 3}, head: point{3, 2}},
		{anchor: point{3, 2}, head: point{1, 3}},
	} {
		for i, want := range [][2]int{{0, 0}, {3, 10}, {0, 10}, {0, 2}, {0, 0}} {
			from, to := s.span(i, 10)
			assert.Equal(t, want, [2]int{from, to}, "line %d of %+v", i, s)
		}
	}

	from, to := selection{anchor: point{0, 2}, head: point{0, 8}}.span(0, 5)
	assert.Equal(t, [2]int{2, 5}, [2]int{from, to}, "clamped to the line's width")
	from, to = selection{anchor: point{0, 7}, head: point{1, 1}}.span(0, 5)
	assert.Equal(t, from, to, "an anchor past the end covers nothing")
}

func TestSelectionTextIsPlainAndTrimmed(t *testing.T) {
	bold := lipgloss.NewStyle().Bold(true)
	lines := []line{
		{text: bold.Render("first") + " line   "},
		{text: "middle  "},
		{text: ""},
		{text: "last " + bold.Render("row")},
	}
	s := selection{anchor: point{3, 6}, head: point{0, 2}}

	assert.Equal(t, "rst line\nmiddle\n\nlast r", s.text(lines))
	assert.Equal(t, "ddl", selection{anchor: point{1, 2}, head: point{1, 5}}.text(lines))
}

func TestStretch(t *testing.T) {
	at := func(p point) selection { return selection{anchor: p, head: p} }
	word := selection{anchor: point{0, 6}, head: point{0, 10}}
	line := selection{anchor: point{2, 0}, head: point{2, 8}}
	for _, tc := range []struct {
		name              string
		unit, under, want selection
	}{
		{"point, pointer after", at(point{0, 3}), at(point{1, 2}), selection{point{0, 3}, point{1, 2}}},
		{"point, pointer before", at(point{1, 2}), at(point{0, 3}), selection{point{1, 2}, point{0, 3}}},
		{"word, pointer on a later word", word, selection{point{0, 11}, point{0, 15}}, selection{point{0, 6}, point{0, 15}}},
		{"word, pointer on an earlier word", word, selection{point{0, 0}, point{0, 5}}, selection{point{0, 10}, point{0, 0}}},
		{"pointer inside the unit", word, word, word},
		{"pointer in a gap after the unit", word, at(point{0, 12}), selection{point{0, 6}, point{0, 12}}},
		{"line, pointer on a later line", line, selection{point{3, 0}, point{3, 4}}, selection{point{2, 0}, point{3, 4}}},
		{"line, pointer on an earlier line", line, selection{point{1, 0}, point{1, 5}}, selection{point{2, 8}, point{1, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stretch(tc.unit, tc.under))
		})
	}
}

func TestWordAt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		col      int
		from, to int
	}{
		{"ascii", "foo bar.baz qux", 6, 4, 11},
		{"first cell", "foo bar", 0, 0, 3},
		{"styled", lipgloss.NewStyle().Bold(true).Render("foo") + " bar", 1, 0, 3},
		{"wide runes", "ab 漢字x yz", 5, 3, 8},
		{"second cell of a wide rune", "漢字 x", 1, 0, 4},
		{"on a space", "foo  bar", 4, 4, 4},
		{"past the end", "foo", 7, 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to := wordAt(tc.text, tc.col)
			assert.Equal(t, [2]int{tc.from, tc.to}, [2]int{from, to})
		})
	}
}

func TestClicksCountQuickPressesOnOneCell(t *testing.T) {
	var c clicks
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	assert.Equal(t, 1, c.press(at, 3, 4))
	assert.Equal(t, 2, c.press(at.Add(multiClick), 3, 4))
	assert.Equal(t, 3, c.press(at.Add(multiClick+time.Millisecond), 3, 4))
	assert.Equal(t, 1, c.press(at.Add(multiClick+2*time.Millisecond), 3, 4), "a fourth starts over")
	assert.Equal(t, 1, c.press(at.Add(2*multiClick+3*time.Millisecond), 3, 4), "too late")
	assert.Equal(t, 1, c.press(at.Add(2*multiClick+4*time.Millisecond), 4, 4), "another cell")
}
