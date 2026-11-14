package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

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
}

// newComposer keeps the textarea's stock editing keys except newline, which
// is Kopi's, and paste, which Kopi owns.
func newComposer(th theme, newline key.Binding) composer {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.MaxWidth = 0
	ta.DynamicHeight = true
	ta.MaxContentHeight = maxComposerLines
	ta.KeyMap.InsertNewline = newline
	ta.KeyMap.Paste.SetEnabled(false)
	ta.SetPromptFunc(2, func(p textarea.PromptInfo) string {
		if p.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	c := composer{ta: ta, blocks: map[string]block{}, imageOf: map[string]string{}}
	c.style(th)
	c.ta.Focus()
	return c
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
				delete(c.blocks, mk)
				return c.repeat(k, len([]rune(mk)))
			}
		case key.Matches(k, c.ta.KeyMap.DeleteCharacterForward):
			if mk := c.markerAfter(); mk != "" {
				delete(c.blocks, mk)
				return c.repeat(k, len([]rune(mk)))
			}
		}
	}
	return c.forward(msg)
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

// press moves the cursor to (x, y), relative to the textarea's top-left
// including the prompt gutter, and starts a selection there. It is other
// input for the paste-expand rule; it leaves the text, so history browsing
// goes on.
func (c *composer) press(x, y int) {
	c.lastPaste = ""
	c.ta.BeginSelection(x, y)
}

// selectWord selects the run of non-space runes under (x, y), coordinates as
// for press, through the textarea's own select-word-forward key. On a space or
// past the line's end it is a plain press.
func (c *composer) selectWord(x, y int) tea.Cmd {
	c.press(x, y)
	before, after := c.cursorLine()
	if r, _ := utf8.DecodeRuneInString(after); after == "" || unicode.IsSpace(r) {
		return nil
	}
	head := []rune(before)
	from := len(head)
	for from > 0 && !unicode.IsSpace(head[from-1]) {
		from--
	}
	c.ta.ClearSelection()
	c.ta.SetCursorColumn(from)
	return c.forward(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl | tea.ModShift})
}

// drag extends the selection press started and moves the cursor with it.
func (c *composer) drag(x, y int) { c.ta.ExtendSelection(x, y) }

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
	if b, ok := c.blocks[last]; ok && b.text == text && c.markerBefore() == last {
		delete(c.blocks, last)
		del := c.repeat(tea.KeyPressMsg{Code: tea.KeyBackspace}, len([]rune(last)))
		return tea.Batch(del, c.forward(tea.PasteMsg{Content: text}))
	}
	// A final newline ends the last line rather than starting another.
	lines, runes := strings.Count(strings.TrimSuffix(text, "\n"), "\n")+1, len([]rune(text))
	if lines <= pasteMaxLines && runes <= pasteMaxRunes {
		return c.forward(tea.PasteMsg{Content: text})
	}
	c.pastes++
	mk := fmt.Sprintf("[paste #%d %d chars]", c.pastes, runes)
	if lines > pasteMaxLines {
		mk = fmt.Sprintf("[paste #%d +%d lines]", c.pastes, lines)
	}
	c.blocks[mk] = block{text: text}
	c.lastPaste = mk
	return c.forward(tea.PasteMsg{Content: mk})
}

// insertImage inserts a new image block for the file at path, replacing
// any selection.
func (c *composer) insertImage(path string) tea.Cmd {
	c.stopBrowsing()
	c.lastPaste = ""
	mk := fmt.Sprintf("[image #%d]", imageCount.Add(1))
	c.blocks[mk] = block{image: path}
	c.imageOf[mk] = path
	return c.forward(tea.PasteMsg{Content: mk})
}

// expanded is the draft as sent: paste markers become their text, image
// markers stay and their files are listed in order of appearance.
func (c *composer) expanded() (text string, imgs []string) {
	text = markerPattern.ReplaceAllStringFunc(c.ta.Value(), func(mk string) string {
		b, ok := c.blocks[mk]
		switch {
		case !ok:
			return mk
		case b.image != "":
			imgs = append(imgs, b.image)
			return mk
		}
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
	c.ta.SetValue(strings.Join(parts, "\n\n"))
	c.lastPaste = ""
	c.stopBrowsing()
}

// height is the textarea's visible lines, without the shaded frame.
func (c *composer) height() int { return c.ta.Height() }

// view frames the input with a blank shaded row above and below.
func (c *composer) view(width int, th theme) []string {
	blank := shade(th).Render(strings.Repeat(" ", width))
	rows := []string{blank}
	rows = append(rows, strings.Split(c.ta.View(), "\n")...)
	return append(rows, blank)
}
