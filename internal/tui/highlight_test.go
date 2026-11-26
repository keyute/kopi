package tui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/skill"
)

var (
	shiftLeft  = tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift}
	shiftRight = tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift}
)

// pieces is the text under each mark, read from the unhighlighted rows.
func pieces(m *model) []string {
	rows := strings.Split(m.composer.ta.View(), "\n")
	var out []string
	for _, mk := range m.composer.marks() {
		out = append(out, ansi.Cut(ansi.Strip(rows[mk.row]), mk.from, mk.to))
	}
	return out
}

func TestHighlightMarksKnownCommandAndReferences(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"/plan", []string{"/plan"}},
		{"/nope", nil},
		{"x /plan", nil},
		{"/plan x", []string{"/plan"}},
		{"see @a and @b ", []string{"@a", "@b"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			pasteText(m, tc.text)
			assert.Equal(t, tc.want, pieces(m))
		})
	}
}

func TestHighlightFollowsSoftWrap(t *testing.T) {
	m, _ := newTestModel(t, 12, 30)
	token := "@abcdefghijklmnopqrstuvwxyz"
	pasteText(m, token)

	marks := m.composer.marks()
	require.Len(t, marks, 3)
	for i, mk := range marks {
		assert.Equal(t, i, mk.row)
		assert.Equal(t, promptWidth, mk.from)
	}
	assert.Equal(t, token, strings.Join(pieces(m), ""))
}

func TestHighlightMarksOnlyVisibleRowsOfScrolledDraft(t *testing.T) {
	m, _ := newTestModel(t, 40, 12)
	pasteText(m, "@a0\n@a1\n@a2\n@a3\n@a4\n@a5\n@a6\n@a7\n@a8\n@a9")
	require.Equal(t, 4, m.composer.height())

	assert.Equal(t, []string{"@a6", "@a7", "@a8", "@a9"}, pieces(m))
	for i, mk := range m.composer.marks() {
		assert.Equal(t, i, mk.row)
	}
}

func TestHighlightCountsWideRunesBeforeToken(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, "日本語 @x ")

	marks := m.composer.marks()
	require.Len(t, marks, 1)
	assert.Equal(t, mark{0, promptWidth + 7, promptWidth + 9, app.KindReference}, marks[0])
	assert.Equal(t, []string{"@x"}, pieces(m))
}

func TestHighlightLeavesCursorCell(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, "@abcdef")
	for range 3 {
		press(m, left)
	}

	assert.Equal(t, []string{"@abc", "ef"}, pieces(m))
	cursor := promptWidth + m.composer.ta.Column()
	for _, mk := range m.composer.marks() {
		assert.False(t, mk.from <= cursor && cursor < mk.to, "cursor cell is marked")
	}
}

func TestHighlightLeavesSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []tea.KeyPressMsg
		want []string
	}{
		// The cursor sits at the selection's start, inside it.
		{"backward", []tea.KeyPressMsg{left, shiftLeft, shiftLeft}, []string{"@abc", "f"}},
		// No cursor is drawn on a row showing a selection, so the rune after it keeps its colour.
		{"forward", []tea.KeyPressMsg{left, left, left, shiftRight}, []string{"@abc", "ef"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			pasteText(m, "@abcdef")
			for _, k := range tc.keys {
				press(m, k)
			}
			require.True(t, m.composer.ta.HasSelection())
			assert.Equal(t, tc.want, pieces(m))
		})
	}
}

func TestHighlightKeepsTextAndShade(t *testing.T) {
	for _, tc := range []struct {
		draft, piece string
		kind         app.Kind
	}{
		{"/plan x", "/plan", app.KindCommand},
		{"see @ref x", "@ref", app.KindReference},
	} {
		m, _ := newTestModel(t, 40, 30)
		pasteText(m, tc.draft)

		plain := strings.Split(m.composer.ta.View(), "\n")
		got := m.composer.view(m.width, m.th)
		got = got[1 : len(got)-1]
		require.Len(t, got, len(plain))
		for i := range plain {
			assert.Equal(t, ansi.Strip(plain[i]), ansi.Strip(got[i]))
		}
		assert.Contains(t, got[0], spanStyle(m.th, tc.kind).Render(tc.piece))
		assert.Equal(t, shade(m.th).GetBackground(), spanStyle(m.th, tc.kind).GetBackground())
	}
}

func TestHighlightMarksSkillCommands(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.skills, _ = skill.NewSet("skill", []skill.Skill{{Name: "a"}, {Name: "b"}}, nil)

	pasteText(m, "/skill:a see @x and /skill:b ")

	assert.Equal(t, []string{"/skill:a", "@x", "/skill:b"}, pieces(m))
}

func TestHighlightFollowsShownSessionWithUnchangedDraft(t *testing.T) {
	m, _ := agentsModel(t)
	const draft = "ultracode now"
	pasteText(m, draft)
	assert.Equal(t, []string{"ultracode"}, pieces(m))

	display(m, "t1")
	m.Update(nil)
	assert.Equal(t, draft, m.composer.value())
	assert.Nil(t, pieces(m))

	display(m, app.Main)
	m.Update(nil)
	assert.Equal(t, draft, m.composer.value())
	assert.Equal(t, []string{"ultracode"}, pieces(m))
}

func TestHighlightMarksTheKeywordOnlyInTheMainView(t *testing.T) {
	m, _ := agentsModel(t)
	pasteText(m, "ultracode now")
	assert.Equal(t, []string{"ultracode"}, pieces(m))

	display(m, "t1")
	pasteText(m, "ultracode now")
	assert.Nil(t, pieces(m))
}

func TestComposerTurnsRedWhileTheDraftIsAShellCommand(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	view := func() []string { return m.composer.view(m.width, m.th) }
	blank := func(st lipgloss.Style) string { return st.Render(strings.Repeat(" ", m.width)) }
	glyph := func(rows []string) string { return rows[1] }

	typeText(m, "ls")
	m.Update(nil)
	assert.Equal(t, blank(shade(m.th)), view()[0])
	assert.Contains(t, glyph(view()), shade(m.th).Foreground(m.th.accent).Render("❯ "))

	m.composer.reset()
	typeText(m, "!ls")
	m.syncInput(false)
	assert.Equal(t, blank(commandShade(m.th)), view()[0])
	assert.Contains(t, glyph(view()), commandShade(m.th).Foreground(m.th.error).Render("! "))
	assert.Equal(t, "ls", m.composer.ta.Value())
	assert.Nil(t, pieces(m))

	m.composer.reset()
	typeText(m, " !ls")
	m.syncInput(false)
	assert.Equal(t, blank(shade(m.th)), view()[0])
	assert.Contains(t, glyph(view()), shade(m.th).Foreground(m.th.accent).Render("❯ "))
}

func TestComposerKeepsTheRedGlyphAcrossAThemeChange(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	typeText(m, "!ls")
	m.syncInput(false)
	before := m.th

	m.Update(tea.BackgroundColorMsg{Color: color.White})
	if m.th == before {
		m.Update(tea.BackgroundColorMsg{Color: color.Black})
	}
	require.NotEqual(t, before, m.th)

	assert.Contains(t, m.composer.view(m.width, m.th)[1], commandShade(m.th).Foreground(m.th.error).Render("! "))
}
