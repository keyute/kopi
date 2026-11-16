package tui

import (
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
)

// mark is cells [from, to) of rendered textarea row row.
type mark struct {
	row, from, to int
	kind          app.Kind
}

// marks places the draft's spans on the rendered rows, leaving out the
// cursor's cell and the selection, which the textarea draws itself.
func (c *composer) marks() []mark {
	if len(c.spans) == 0 {
		return nil
	}
	spans := c.visibleSpans()
	lines := strings.Split(c.ta.Value(), "\n")
	var out []mark
	for y := range c.ta.Height() {
		// Every wrapped segment but a line's last ends where the next begins;
		// the last carries one extra space that PositionAt clamps away. A row
		// below the text maps to the buffer's end, an empty run.
		s, e := c.ta.PositionAt(0, y), c.ta.PositionAt(beyond, y)
		if s.Col >= e.Col {
			continue
		}
		var line []rune
		for _, sp := range spans {
			from, to := max(sp.Start, s.Col), min(sp.End, e.Col)
			if sp.Line != s.Row || from >= to {
				continue
			}
			if line == nil {
				line = []rune(lines[s.Row])
			}
			x := promptWidth + ansi.StringWidth(string(line[s.Col:from]))
			out = append(out, mark{y, x, x + ansi.StringWidth(string(line[from:to])), sp.Kind})
		}
	}
	return out
}

// visibleSpans is c.spans without the runes the textarea draws as selected
// or as the cursor. The textarea draws no cursor on a wrapped row that shows
// any selection.
func (c *composer) visibleSpans() []app.Span {
	spans := c.spans
	start, end, selected := c.ta.Selection()
	on := func(row int) (from, to int) {
		from, to = 0, math.MaxInt
		if row == start.Row {
			from = start.Col
		}
		if row == end.Row {
			to = end.Col
		}
		return from, to
	}
	cursorShown := true
	if row := c.ta.Line(); selected && start.Row <= row && row <= end.Row {
		li := c.ta.LineInfo()
		from, to := on(row)
		cursorShown = max(from, li.StartColumn) >= min(to, li.StartColumn+li.Width)
	}
	if cursorShown {
		spans = without(spans, c.ta.Line(), c.ta.Column(), c.ta.Column()+1)
	}
	if selected {
		for row := start.Row; row <= end.Row; row++ {
			from, to := on(row)
			spans = without(spans, row, from, to)
		}
	}
	return spans
}

// without is spans with runes [from, to) of line removed, splitting a span
// that straddles them.
func without(spans []app.Span, line, from, to int) []app.Span {
	var out []app.Span
	for _, s := range spans {
		if s.Line != line || s.End <= from || to <= s.Start {
			out = append(out, s)
			continue
		}
		if s.Start < from {
			out = append(out, app.Span{Line: s.Line, Start: s.Start, End: from, Kind: s.Kind})
		}
		if to < s.End {
			out = append(out, app.Span{Line: s.Line, Start: to, End: s.End, Kind: s.Kind})
		}
	}
	return out
}

// highlight restyles the marked cells of the textarea's rendered rows.
func (c *composer) highlight(rows []string, th theme) []string {
	ranges := map[int][]lipgloss.Range{}
	for _, mk := range c.marks() {
		ranges[mk.row] = append(ranges[mk.row], lipgloss.NewRange(mk.from, mk.to, spanStyle(th, mk.kind)))
	}
	for y, rs := range ranges {
		rows[y] = lipgloss.StyleRanges(rows[y], rs...)
	}
	return rows
}

func spanStyle(th theme, k app.Kind) lipgloss.Style {
	switch k {
	case app.KindCommand:
		return shade(th).Foreground(th.accent)
	case app.KindReference:
		return shade(th).Foreground(th.link)
	}
	return shade(th)
}
