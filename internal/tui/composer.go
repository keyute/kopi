package tui

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// maxComposerLines lifts the textarea's content limit, which without it
// falls back to capping logical lines at MaxHeight, the visible height.
const maxComposerLines = 10000

const (
	// A paste over either limit collapses into a paste block.
	pasteMaxLines = 10
	pasteMaxRunes = 1000
)

// beyond is past every row and column of the input; PositionAt maps
// coordinates past the content to its end.
const beyond = math.MaxInt32

// promptWidth is the cells of the composer's prompt gutter.
const promptWidth = 2

// imageCount numbers image blocks for the whole process, so markers from drafts
// that are later joined never collide.
var imageCount atomic.Int64

// markerPattern finds candidate markers; no marker contains a bracket.
var markerPattern = regexp.MustCompile(`\[[^\[\]]*\]`)

var imageMarker = regexp.MustCompile(`\[image #\d+\]`)

// block is what a marker in the draft stands for: pasted text or an image
// file. The marker is plain text in the textarea and counts only while its
// exact text is present.
type block struct {
	text, image string
	// source preserves clipboard identity when a small overflow paste is sanitized.
	source string
	// images accompanies text when an overflowing restore is collapsed.
	images []string
}

// composer is the input box.
type composer struct {
	ta textarea.Model
	// blocks is keyed by marker.
	blocks map[string]block
	// imageOf is the file of every image marker this composer inserted; it
	// outlives reset so a restored input can tell its images from typed text.
	imageOf map[string]string
	pastes  int
	// lastPaste is the marker of the paste block inserted by the latest
	// input, or empty once any other input followed it.
	lastPaste string
	hist      recall
	// clicks and unit are the count and selection of the latest press, which
	// a drag extends from.
	clicks int
	unit   selection
	spans  []app.Span
	// secret draws the draft as one bullet per character; only fields set it.
	secret bool
}

// baseComposer keeps the textarea's stock editing keys except newline, which
// is Kopi's, and paste, which Kopi owns.
func baseComposer(newline key.Binding) composer {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.MaxWidth = 0
	ta.DynamicHeight = true
	ta.MaxContentHeight = maxComposerLines
	ta.KeyMap.InsertNewline = newline
	ta.KeyMap.Paste.SetEnabled(false)
	return composer{ta: ta, blocks: map[string]block{}, imageOf: map[string]string{}}
}

func newComposer(th theme, newline key.Binding) composer {
	c := baseComposer(newline)
	c.ta.SetPromptFunc(promptWidth, func(p textarea.PromptInfo) string {
		if p.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	c.style(th)
	c.ta.Focus()
	return c
}

// newField is a blurred text field inside a dialog, without a prompt or
// shade. Only its editing, paste and expanded are used.
func newField(th theme, newline key.Binding, placeholder string) composer {
	c := baseComposer(newline)
	// A nil func falls back to the textarea's stock prompt.
	c.ta.SetPromptFunc(0, func(textarea.PromptInfo) string { return "" })
	c.ta.Placeholder = placeholder
	c.fieldStyle(th)
	c.ta.Blur()
	return c
}

func (c *composer) fieldStyle(th theme) {
	none, text := lipgloss.NewStyle(), fg(th.text)
	state := textarea.StyleState{
		Text:        text,
		CursorLine:  text,
		EndOfBuffer: none,
		Placeholder: fg(th.dim),
		Prompt:      none,
		Selection:   none.Reverse(true),
	}
	c.ta.SetStyles(textarea.Styles{Focused: state, Blurred: state, Cursor: textarea.CursorStyle{Color: th.text}})
}

func (c *composer) style(th theme) {
	shaded := shade(th)
	state := textarea.StyleState{
		Text:        shaded,
		CursorLine:  shaded,
		EndOfBuffer: shaded,
		Prompt:      shaded.Foreground(th.accent),
		Selection:   shaded.Reverse(true),
	}
	c.ta.SetStyles(textarea.Styles{Focused: state, Blurred: state, Cursor: textarea.CursorStyle{Color: th.text}})
}

// resize lets the input grow to a third of the terminal height.
func (c *composer) resize(width, height int) {
	c.ta.MaxHeight = max(1, height/3)
	c.ta.SetWidth(width)
}

// update is input to the composer. Backspace and Delete next to a marker
// delete all of it. Input that changes the draft stops history browsing.
func (c *composer) update(msg tea.Msg) tea.Cmd {
	c.lastPaste = ""
	before := c.ta.Value()
	defer func() {
		if c.ta.Value() != before {
			c.stopBrowsing()
		}
	}()
	if k, ok := msg.(tea.KeyPressMsg); ok && !c.ta.HasSelection() {
		switch {
		case key.Matches(k, c.ta.KeyMap.DeleteCharacterBackward):
			if mk := c.markerBefore(); mk != "" {
				cmd := c.repeat(k, len([]rune(mk)))
				c.discardAbsent(mk)
				return cmd
			}
		case key.Matches(k, c.ta.KeyMap.DeleteCharacterForward):
			if mk := c.markerAfter(); mk != "" {
				cmd := c.repeat(k, len([]rune(mk)))
				c.discardAbsent(mk)
				return cmd
			}
		}
	}
	return c.forward(msg)
}

func (c *composer) discardAbsent(mk string) {
	if !strings.Contains(c.ta.Value(), mk) {
		delete(c.blocks, mk)
	}
}

func (c *composer) forward(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	return cmd
}

// repeat feeds k n times: the textarea cannot place its cursor at an
// arbitrary column, and a marker never spans logical lines.
func (c *composer) repeat(k tea.KeyPressMsg, n int) tea.Cmd {
	cmds := make([]tea.Cmd, n)
	for i := range cmds {
		cmds[i] = c.forward(k)
	}
	return tea.Batch(cmds...)
}

// replace puts s in place of runes [from, to) of the cursor's line, which hold
// the cursor, leaving the cursor after s. The deletes go straight to the
// textarea, so a marker inside the range loses only its runes in the range.
func (c *composer) replace(from, to int, s string) tea.Cmd {
	c.stopBrowsing()
	c.lastPaste = ""
	c.ta.ClearSelection()
	c.ta.SetCursorColumn(to)
	back := c.repeat(tea.KeyPressMsg{Code: tea.KeyBackspace}, to-from)
	c.ta.InsertString(s)
	return back
}

// press handles the n-th quick press at (x, y), relative to the textarea's
// top-left including the prompt gutter: the first places the cursor and
// starts a selection there, the second selects the word under it and the
// third its logical line. It is other input for the paste-expand rule; it
// leaves the text, so history browsing goes on.
func (c *composer) press(x, y, n int) {
	c.lastPaste = ""
	c.clicks = n
	if n == 1 {
		c.ta.BeginSelection(x, y)
		return
	}
	c.unit = c.unitAt(c.pointAt(x, y))
	c.selectRange(c.unit)
}

// drag stretches the selection press started to (x, y) by the unit press
// selected and moves the cursor with it.
func (c *composer) drag(x, y int) {
	if c.clicks == 1 {
		c.ta.ExtendSelection(x, y)
		return
	}
	c.selectRange(stretch(c.unit, c.unitAt(c.pointAt(x, y))))
}

// unitAt is what the latest press selects at p: the point itself, its word
// or its logical line.
func (c *composer) unitAt(p point) selection {
	line := []rune(strings.Split(c.ta.Value(), "\n")[p.line])
	switch c.clicks {
	case 2:
		from, to := runeWordAt(line, p.col)
		return selection{anchor: point{p.line, from}, head: point{p.line, to}}
	case 3:
		return selection{anchor: point{p.line, 0}, head: point{p.line, len(line)}}
	}
	return selection{anchor: p, head: p}
}

// runeWordAt is the runes [from, to) of the maximal run of non-space runes in
// line covering line[i]; from == to == i when i is on a space or past the end.
func runeWordAt(line []rune, i int) (from, to int) {
	if i >= len(line) || unicode.IsSpace(line[i]) {
		return i, i
	}
	from, to = i, i+1
	for from > 0 && !unicode.IsSpace(line[from-1]) {
		from--
	}
	for to < len(line) && !unicode.IsSpace(line[to]) {
		to++
	}
	return from, to
}

func (c *composer) pointAt(x, y int) point {
	p := c.ta.PositionAt(x, y)
	return point{line: p.Row, col: p.Col}
}

// cellOf is the first textarea-relative cell, in reading order, that
// PositionAt maps to p or later. The textarea can select only between cells.
func (c *composer) cellOf(p point) (x, y int) {
	top := -c.ta.ScrollYOffset()
	y = top + sort.Search(beyond, func(i int) bool { return !c.pointAt(beyond, top+i).before(p) })
	x = sort.Search(beyond, func(x int) bool { return !c.pointAt(x, y).before(p) })
	return x, y
}

// selectRange selects s and leaves the cursor at its head.
func (c *composer) selectRange(s selection) {
	ax, ay := c.cellOf(s.anchor)
	hx, hy := c.cellOf(s.head)
	c.ta.BeginSelection(ax, ay)
	c.ta.ExtendSelection(hx, hy)
}

// release ends the drag, returning the selected text when there is any.
func (c *composer) release() (text string, ok bool) {
	c.ta.EndSelection()
	if !c.ta.HasSelection() {
		return "", false
	}
	return c.ta.SelectedText(), true
}

// cursorLine is the cursor's logical line split at the cursor.
func (c *composer) cursorLine() (before, after string) {
	line := []rune(strings.Split(c.ta.Value(), "\n")[c.ta.Line()])
	col := min(c.ta.Column(), len(line))
	return string(line[:col]), string(line[col:])
}

func (c *composer) markerBefore() string {
	before, _ := c.cursorLine()
	for mk := range c.blocks {
		if strings.HasSuffix(before, mk) {
			return mk
		}
	}
	return ""
}

func (c *composer) markerAfter() string {
	_, after := c.cursorLine()
	for mk := range c.blocks {
		if strings.HasPrefix(after, mk) {
			return mk
		}
	}
	return ""
}

// paste inserts text. Text over the limits collapses into a paste block;
// pasting the text of the block the previous input inserted expands that
// block in place instead.
func (c *composer) paste(text string) tea.Cmd {
	c.stopBrowsing()
	last := c.lastPaste
	c.lastPaste = ""
	if b, ok := c.blocks[last]; ok && ((b.source == "" && b.text == text) || (b.source != "" && b.source == text)) && c.markerBefore() == last {
		before, after := c.cursorLine()
		lines := strings.Split(c.ta.Value(), "\n")
		lines[c.ta.Line()] = strings.TrimSuffix(before, last) + sanitizedText(text) + after
		want := strings.Join(lines, "\n")

		// SetValue detaches the trial's content before editing: a shallow
		// textarea copy alone still shares its rune slices with the draft.
		trial := *c
		trial.ta.SetValue(c.ta.Value())
		p := point{c.ta.Line(), c.ta.Column()}
		trial.selectRange(selection{anchor: p, head: p})
		del := trial.repeat(tea.KeyPressMsg{Code: tea.KeyBackspace}, len([]rune(last)))
		ins := trial.forward(tea.PasteMsg{Content: text})
		if trial.ta.Value() != want {
			c.lastPaste = last
			return nil
		}
		c.ta = trial.ta
		c.discardAbsent(last)
		return tea.Batch(del, ins)
	}
	// A final newline ends the last line rather than starting another.
	lines, runes := strings.Count(strings.TrimSuffix(text, "\n"), "\n")+1, len([]rune(text))
	content := text
	if lines > pasteMaxLines || runes > pasteMaxRunes {
		content = c.collapse(text, nil)
		c.lastPaste = content
	}
	trial, want, cmd := c.trialPaste(content)
	if trial.ta.Value() == want {
		c.ta = trial.ta
		return cmd
	}
	if content == text {
		clean := sanitizedText(text)
		content = c.collapse(clean, nil)
		if clean != text {
			b := c.blocks[content]
			b.source = text
			c.blocks[content] = b
		}
		c.lastPaste = content
		var markerWant string
		trial, markerWant, cmd = c.trialPaste(content)
		if trial.ta.Value() == markerWant {
			c.ta = trial.ta
			return cmd
		}
	}
	// Even a marker can exceed the remaining visual capacity. Collapse the
	// whole intended draft so no part of the insertion is silently lost.
	full, imgs := c.expand(want)
	c.ta.SetValue(c.collapse(full, imgs))
	c.lastPaste = ""
	return nil
}

// trialPaste detaches the textarea before applying the insertion, including
// selection replacement, and returns its uncapped sanitized target.
func (c *composer) trialPaste(text string) (composer, string, tea.Cmd) {
	start, end, selected := c.ta.Selection()
	if !selected {
		start.Row, start.Col = c.ta.Line(), c.ta.Column()
		end = start
	}
	lines := strings.Split(c.value(), "\n")
	before := strings.Join(lines[:start.Row], "\n")
	if start.Row > 0 {
		before += "\n"
	}
	before += string([]rune(lines[start.Row])[:start.Col])
	after := string([]rune(lines[end.Row])[end.Col:])
	if end.Row < len(lines)-1 {
		after += "\n" + strings.Join(lines[end.Row+1:], "\n")
	}
	want := before + sanitizedText(text) + after
	trial := *c
	trial.ta.SetValue(c.value())
	if selected || trial.ta.Line() != start.Row || trial.ta.Column() != start.Col {
		ax, ay := trial.cellOf(point{start.Row, start.Col})
		trial.ta.BeginSelection(ax, ay)
		hx, hy := trial.cellOf(point{end.Row, end.Col})
		trial.ta.ExtendSelection(hx, hy)
	}
	cmd := trial.forward(tea.PasteMsg{Content: text})
	return trial, want, cmd
}

// sanitizedText uses textarea's input rules without its whole-draft limits.
// Sanitizing one rune at a time cannot hit either logical or visual capacity.
func sanitizedText(text string) string {
	ta := textarea.New()
	runes := make(map[rune]string)
	var clean strings.Builder
	for _, r := range text {
		s, ok := runes[r]
		if !ok {
			ta.SetValue(string(r))
			s = ta.Value()
			runes[r] = s
		}
		clean.WriteString(s)
	}
	return clean.String()
}

func (c *composer) collapse(text string, images []string) string {
	c.pastes++
	lines := strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
	mk := fmt.Sprintf("[paste #%d %d chars]", c.pastes, len([]rune(text)))
	if lines > pasteMaxLines {
		mk = fmt.Sprintf("[paste #%d +%d lines]", c.pastes, lines)
	}
	c.blocks[mk] = block{text: text, images: images}
	return mk
}

// insertImage inserts a new image block for the file at path, replacing
// any selection.
func (c *composer) insertImage(path string) tea.Cmd {
	c.stopBrowsing()
	c.lastPaste = ""
	mk := fmt.Sprintf("[image #%d]", imageCount.Add(1))
	c.blocks[mk] = block{image: path}
	c.imageOf[mk] = path
	trial, want, cmd := c.trialPaste(mk)
	if trial.ta.Value() == want {
		c.ta = trial.ta
		return cmd
	}
	full, imgs := c.expand(want)
	c.ta.SetValue(c.collapse(full, imgs))
	return nil
}

// expanded is the draft as sent: paste markers become their text, image
// markers stay and their files are listed in order of appearance.
func (c *composer) expanded() (text string, imgs []string) {
	return c.expand(c.ta.Value())
}

func (c *composer) expand(value string) (text string, imgs []string) {
	text = markerPattern.ReplaceAllStringFunc(value, func(mk string) string {
		b, ok := c.blocks[mk]
		switch {
		case !ok:
			return mk
		case b.image != "":
			imgs = append(imgs, b.image)
			return mk
		}
		imgs = append(imgs, b.images...)
		return b.text
	})
	return text, imgs
}

func (c *composer) value() string { return c.ta.Value() }

func (c *composer) empty() bool { return c.ta.Value() == "" }

// reset clears the draft and its blocks; paste numbers restart, image
// numbers do not.
func (c *composer) reset() {
	c.ta.Reset()
	c.blocks, c.pastes, c.lastPaste = map[string]block{}, 0, ""
	c.stopBrowsing()
}

// restore puts inputs before the draft, everything joined by a blank line.
// An [image #N] marker in an input's text becomes an image block again only
// if this composer inserted it and its file is among that input's Images not
// yet claimed; any other marker stays plain text.
func (c *composer) restore(inputs []event.Input) {
	if len(inputs) == 0 {
		return
	}
	parts := make([]string, 0, len(inputs)+1)
	for _, in := range inputs {
		imgs := slices.Clone(in.Images)
		for _, mk := range imageMarker.FindAllString(in.Text, -1) {
			path, ok := c.imageOf[mk]
			if i := slices.Index(imgs, path); ok && i >= 0 {
				c.blocks[mk] = block{image: path}
				imgs = slices.Delete(imgs, i, i+1)
			}
		}
		parts = append(parts, in.Text)
	}
	if draft := c.ta.Value(); draft != "" {
		parts = append(parts, draft)
	}
	joined := strings.Join(parts, "\n\n")
	trial := c.ta
	trial.SetValue(joined)
	if trial.Value() != sanitizedText(joined) {
		// Keep the full expanded input and its image references outside the
		// textarea when either logical lines or soft wraps exceed its limits.
		text, imgs := c.expand(joined)
		trial.SetValue(c.collapse(text, imgs))
	}
	c.ta = trial
	c.lastPaste = ""
	c.stopBrowsing()
}

// height is the textarea's visible lines, without the shaded frame.
func (c *composer) height() int { return c.ta.Height() }

// view frames the input with a blank shaded row above and below.
func (c *composer) view(width int, th theme) []string {
	blank := shade(th).Render(strings.Repeat(" ", width))
	rows := []string{blank}
	rows = append(rows, c.highlight(strings.Split(c.ta.View(), "\n"), th)...)
	return append(rows, blank)
}
