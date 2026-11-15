package tui

import (
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// multiClick is how soon a press on the same cell must follow the previous
// one to count as a double or triple click.
const multiClick = 400 * time.Millisecond

// point is a place in the text. In the transcript it is an index into
// model.lines and a cell column; indices rather than screen rows keep a
// selection on its text while the view scrolls. In the composer it is a
// logical line and a rune index into it.
type point struct{ line, col int }

func (p point) before(q point) bool {
	if p.line != q.line {
		return p.line < q.line
	}
	return p.col < q.col
}

// selection is the transcript text from anchor to head, half-open in cells:
// the cell at the later point is not included. It is empty when they are
// equal.
type selection struct{ anchor, head point }

func (s selection) ordered() (from, to point) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

func (s selection) empty() bool { return s.anchor == s.head }

// stretch is the selection of a drag that began on unit (the word or line a
// double or triple press took, else the pressed point) and is now over under,
// the same kind of unit at the pointer: both whole, anchored at unit's far
// end. Both are ordered, anchor no later than head.
func stretch(unit, under selection) selection {
	if under.anchor.before(unit.anchor) {
		return selection{anchor: unit.head, head: under.anchor}
	}
	if unit.head.before(under.head) {
		return selection{anchor: unit.anchor, head: under.head}
	}
	return unit
}

// span is the cells [from, to) that s covers of line i, width cells wide;
// from == to when it covers none.
func (s selection) span(i, width int) (from, to int) {
	a, b := s.ordered()
	if i < a.line || i > b.line {
		return 0, 0
	}
	from, to = 0, width
	if i == a.line {
		from = min(a.col, width)
	}
	if i == b.line {
		to = min(b.col, width)
	}
	return from, max(from, to)
}

// text is the plain text s covers in lines, each line's trailing spaces
// trimmed, joined by newlines.
func (s selection) text(lines []line) string {
	a, b := s.ordered()
	var out []string
	for i := max(a.line, 0); i <= b.line && i < len(lines); i++ {
		plain := ansi.Strip(lines[i].text)
		from, to := s.span(i, ansi.StringWidth(plain))
		out = append(out, strings.TrimRight(ansi.Cut(plain, from, to), " "))
	}
	return strings.Join(out, "\n")
}

// wordAt is the cells [from, to) of the maximal run of non-space runes in
// text covering cell col; from == to == col when col is on a space or past
// the end.
func wordAt(text string, col int) (from, to int) {
	type cell struct {
		from, to int
		space    bool
	}
	var cells []cell
	x := 0
	for _, r := range ansi.Strip(text) {
		w := ansi.StringWidth(string(r))
		cells = append(cells, cell{x, x + w, unicode.IsSpace(r)})
		x += w
	}
	at := -1
	for i, c := range cells {
		if c.from <= col && col < c.to {
			at = i
			break
		}
	}
	if at < 0 || cells[at].space {
		return col, col
	}
	from, to = cells[at].from, cells[at].to
	for i := at - 1; i >= 0 && !cells[i].space; i-- {
		from = cells[i].from
	}
	for i := at + 1; i < len(cells) && !cells[i].space; i++ {
		to = cells[i].to
	}
	return from, to
}

// clicks counts presses on one cell in quick succession: 1, 2, 3, then 1
// again.
type clicks struct {
	at   time.Time
	x, y int
	n    int
}

func (c *clicks) press(now time.Time, x, y int) int {
	if c.n > 0 && c.n < 3 && x == c.x && y == c.y && now.Sub(c.at) <= multiClick {
		c.n++
	} else {
		c.n = 1
	}
	c.at, c.x, c.y = now, x, y
	return c.n
}
