package tui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func stripped(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = ansi.Strip(l)
	}
	return out
}

func renderMarkdown(text string, width int) []string {
	return stripped(newMarkdown(width, mocha()).lines("k", text))
}

func TestMarkdownHeadingAndParagraphAtEdge(t *testing.T) {
	assert.Equal(t, []string{"Title", "", "Some text."}, renderMarkdown("## Title\n\nSome text.\n", 40))
}

func TestMarkdownListUsesDash(t *testing.T) {
	assert.Equal(t, []string{"- one", "- two"}, renderMarkdown("* one\n* two", 40))
}

func TestMarkdownInlineCodeAndLinkKeepText(t *testing.T) {
	got := strings.Join(renderMarkdown("Call `Load` per [the docs](https://example.com).", 80), "\n")
	assert.Contains(t, got, "Call Load per the docs")
	assert.Contains(t, got, "https://example.com")
}

func TestMarkdownANSIThemesUsePaletteColors(t *testing.T) {
	for _, name := range []string{"ansi-dark", "ansi-light"} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name, source, visible, palette string
			}{
				{"heading", "## Title", "Title", "33"},
				{"inline code", "Call `Load` now.", "Call Load now.", "36"},
				{"fenced code", "```go\nfunc main() {}\n```", "func main() {}", "(?:3[0-7]|9[0-7])"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					lines := newMarkdown(80, builtinThemes[name]()).lines("k", tc.source)
					got := strings.Join(lines, "\n")
					assert.Equal(t, tc.visible, ansi.Strip(got))
					assert.Regexp(t, `\x1b\[(?:[0-9]+;)*`+tc.palette+`(?:;[0-9]+)*m`, got)
					assert.NotRegexp(t, `\x1b\[[0-9;:]*(?:38|48|58)[;:]2[;:]`, got)
				})
			}
		})
	}
}

func TestMarkdownReferenceLinksAcrossBlankLines(t *testing.T) {
	for _, text := range []string{
		"Read [the docs][docs].\n\nAnother paragraph.\n\n[docs]: https://example.com/docs",
		"[docs]: https://example.com/docs\n\nAnother paragraph.\n\nRead [the docs][docs].",
	} {
		got := strings.Join(renderMarkdown(text, 80), "\n")
		assert.Contains(t, got, "Read the docs")
		assert.Contains(t, got, "https://example.com/docs")
		assert.NotContains(t, got, "[docs]")
	}
}

func TestMarkdownStreamingReferenceDefinitionResolvesEarlierLink(t *testing.T) {
	md := newMarkdown(80, mocha())
	text := "Read [the docs][docs].\n\nAnother paragraph."
	got := strings.Join(stripped(md.lines("k", text)), "\n")
	assert.Contains(t, got, "[the docs][docs]")

	text += "\n\n[docs]: https://example.com/docs"
	got = strings.Join(stripped(md.lines("k", text)), "\n")

	assert.Contains(t, got, "Read the docs")
	assert.Contains(t, got, "https://example.com/docs")
	assert.NotContains(t, got, "[docs]")
	assert.Equal(t, renderMarkdown(text, 80), stripped(md.lines("k", text)))
}

func TestMarkdownFenceIsVerbatimAndOneChunk(t *testing.T) {
	src := "```go\nfunc A() {}\n\nfunc B() {}\n```"
	assert.Equal(t, []string{src}, splitChunks(src))
	assert.Equal(t, []string{"func A() {}", "", "func B() {}"}, renderMarkdown(src, 40))
}

func TestSplitChunks(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"paragraphs split", "one\n\n\ntwo", []string{"one", "two"}},
		{"list after paragraph stays", "intro\n\n- a\n- b", []string{"intro\n\n- a\n- b"}},
		{"list continuation stays", "- a\n\n  more of a\n\n- b", []string{"- a\n\n  more of a\n\n- b"}},
		{"quote and table stay", "> a\n\n> b\n\n| x |", []string{"> a\n\n> b\n\n| x |"}},
		{"paragraph after list splits", "- a\n\nafter", []string{"- a", "after"}},
		{"unclosed fence keeps the rest", "intro\n\n```\ncode\n\nmore\n\nstill code", []string{"intro", "```\ncode\n\nmore\n\nstill code"}},
		{"closed fence splits after", "```\ncode\n\nx\n```\n\nafter", []string{"```\ncode\n\nx\n```", "after"}},
		{"leading and trailing blanks dropped", "\n\none\n\n", []string{"one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, splitChunks(tc.text))
		})
	}
}

const streamDoc = "## Plan\n\nRead **two** files with `Read`.\n\n- first\n- second\n\n> note\n\n```go\nfunc main() {\n\n}\n```\n\nDone."

func TestMarkdownStreamingEndsLikeWholeRender(t *testing.T) {
	md := newMarkdown(40, mocha())
	var text string
	var got []string
	for _, r := range streamDoc {
		text += string(r)
		got = md.lines("k", text)
	}

	assert.Equal(t, newMarkdown(40, mocha()).lines("k", streamDoc), got)
}

func TestMarkdownDeltaRerendersOnlyLastChunk(t *testing.T) {
	md := newMarkdown(40, mocha())
	md.lines("k", "one\n\ntwo\n\nthr")
	before := md.renders

	md.lines("k", "one\n\ntwo\n\nthree")

	assert.Equal(t, 1, md.renders-before)
}

func TestAssistantMarkdownReflowsOnWidthAndTheme(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.TextDelta{Text: "alpha beta gamma delta"})
	require.Equal(t, []string{"", "alpha beta gamma delta"}, body(m))

	m.Update(tea.WindowSizeMsg{Width: 12, Height: 30})
	assert.Equal(t, []string{"", "alpha beta", "gamma delta"}, body(m))
	for _, l := range m.view().lines[len(header(m.version, m.th)):] {
		assert.LessOrEqual(t, ansi.StringWidth(l.text), 12)
	}

	m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	send(m, event.TextDelta{Text: " `code`"})
	dark := m.view().lines[len(m.view().lines)-1].text
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.NotEqual(t, dark, m.view().lines[len(m.view().lines)-1].text, "a new theme re-renders")
}

func TestMarkdownWrapsLongCodeLines(t *testing.T) {
	ls := newMarkdown(10, mocha()).lines("k", "```\n"+strings.Repeat("x", 25)+"\n```")
	require.Len(t, ls, 3)
	for _, l := range ls {
		assert.LessOrEqual(t, ansi.StringWidth(l), 10)
	}
}
