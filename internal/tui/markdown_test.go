package tui

import (
	"fmt"
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

func TestMarkdownBodyTextUsesThemeText(t *testing.T) {
	r, g, b, _ := mocha().text.RGBA()
	got := strings.Join(newMarkdown(80, mocha()).lines("k", "Some text."), "\n")
	assert.Equal(t, "Some text.", ansi.Strip(got))
	assert.Contains(t, got, fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r>>8, g>>8, b>>8))

	got = strings.Join(newMarkdown(80, builtinThemes["ansi-dark"]()).lines("k", "Some text."), "\n")
	assert.Equal(t, "Some text.", got)
}

func TestMarkdownLinesCarryNoPaddingEscapes(t *testing.T) {
	const text = "Some text.\n\n- item one\n\n> quote"
	got := newMarkdown(200, mocha()).lines("k", text)

	require.Equal(t, []string{"Some text.", "", "- item one", "", "│ quote"}, stripped(got))
	assert.Equal(t, "", got[1])
	for _, l := range got {
		assert.NotRegexp(t, `(?:\x1b\[[0-9;:]*m){2,}$`, l)
		if strings.Contains(l, "\x1b") {
			assert.True(t, strings.HasSuffix(l, "\x1b[m"), "%q", l)
		}
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

func TestMarkdownFenceIsVerbatim(t *testing.T) {
	src := "```go\nfunc A() {}\n\nfunc B() {}\n```"
	assert.Equal(t, []string{"func A() {}", "", "func B() {}"}, renderMarkdown(src, 40))
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

const streamTrickyDoc = " Results:\n| a | b |\n|---|---|\n| 1 | 2 |\n\nSee [docs][r].\n\n> quoted [q][r]\n>\n> [r]: http://example.com/long/path\n\n[r]: http://example.com/long/path\n\nTail."

func TestMarkdownStreamingTableAndDefinitionsEndLikeWholeRender(t *testing.T) {
	md := newMarkdown(40, mocha())
	var text string
	var got []string
	for _, r := range streamTrickyDoc {
		text += string(r)
		got = md.lines("k", text)
	}

	assert.Equal(t, newMarkdown(40, mocha()).lines("k", streamTrickyDoc), got)
	assert.Contains(t, strings.Join(stripped(got), "\n"), "See docs http://example.com/long/path.")
}

func TestMarkdownReplacedTextIsDrawnAfresh(t *testing.T) {
	md := newMarkdown(40, mocha())
	md.lines("k", "> # H\n> text")

	replaced := "> # H\n>\n> text"
	assert.Equal(t, renderMarkdown(replaced, 40), stripped(md.lines("k", replaced)))
}

func TestMarkdownTableDirectlyAfterParagraphDraws(t *testing.T) {
	got := renderMarkdown(" Results:\n| a | b |\n|---|---|\n| 1 | 2 |", 20)

	require.Len(t, got, 5)
	assert.Equal(t, "Results:", got[0])
	assert.Contains(t, got[3], "─")
}

func TestMarkdownDrawsNoControlBytesFromReferences(t *testing.T) {
	got := strings.Join(renderMarkdown("x &#27;c y\n\n&#155;2J", 40), "\n")

	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\u009b")
	assert.Contains(t, got, "2J")
}

func TestMarkdownSmallTableSpansTheWidth(t *testing.T) {
	got := renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |", 60)

	assert.Equal(t, " a                            │ b", got[0])
}

func TestMarkdownWiderTableLosesNoText(t *testing.T) {
	const src = "| Name | Description | Default | Type | Notes |\n|---|---|---|---|---|\n| timeout | How long to wait before giving up | 30s | duration | applies per call |"
	got := renderMarkdown(src, 50)
	for _, l := range got {
		assert.LessOrEqual(t, ansi.StringWidth(l), 50)
	}
	// A cut splits a word over two rows; no text is dropped.
	joined := strings.Join(got, "")
	for _, word := range []string{"Default", "How long to wait before giving up", "applies per call"} {
		assert.Contains(t, joined, word)
	}
	assert.NotContains(t, joined, "…")
}

func TestMarkdownHTMLBlockKeepsItsClosingLine(t *testing.T) {
	assert.Equal(t, []string{"<!--", "note", "-->"}, renderMarkdown("<!--\nnote\n-->", 40))
	assert.Equal(t, []string{"<pre>", "x", "</pre>"}, renderMarkdown("<pre>\nx\n</pre>", 40))
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

func TestMarkdownCodeBlocksUseCodeColourForPlainText(t *testing.T) {
	r, g, b, _ := mocha().code.RGBA()
	truecolor := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r>>8, g>>8, b>>8)
	for _, src := range []string{"```\n! gcloud auth login\n```", "```sh\n! gcloud auth login\n```"} {
		got := strings.Join(newMarkdown(80, mocha()).lines("k", src), "\n")
		assert.Equal(t, "! gcloud auth login", ansi.Strip(got))
		assert.Contains(t, got, truecolor, src)
	}

	got := strings.Join(newMarkdown(80, builtinThemes["ansi-dark"]()).lines("k", "```\nplain\n```"), "\n")
	assert.Regexp(t, `\x1b\[(?:[0-9]+;)*36(?:;[0-9]+)*m`, got)

	got = strings.Join(newMarkdown(80, mocha()).lines("k", "```go\nfunc main() {}\n```"), "\n")
	assert.Equal(t, "func main() {}", ansi.Strip(got))
	assert.NotContains(t, strings.SplitN(got, "func", 2)[0], truecolor, "a keyword keeps the style's colour")
}

func TestMarkdownLooseListItemKeepsItsBlocksUnderItsText(t *testing.T) {
	got := renderMarkdown("1. one\n\n   para in one\n\n   ```\n   code\n   ```\n2. two", 40)
	assert.Equal(t, []string{"1. one", "", "   para in one", "", "   code", "", "2. two"}, got)
}

func TestMarkdownResolvesEscapesAndEntities(t *testing.T) {
	assert.Equal(t, []string{"a * b & <tag>"}, renderMarkdown(`a \* b &amp; &lt;tag&gt;`, 40))
}

func TestMarkdownHardBreakStartsANewRow(t *testing.T) {
	assert.Equal(t, []string{"one", "two"}, renderMarkdown("one\\\ntwo", 40))
}

func TestMarkdownTableAlignsColumnsPerSource(t *testing.T) {
	got := renderMarkdown("| a | b |\n|---|--:|\n| x | 1 |\n| y | 22 |", 40)
	require.Len(t, got, 4)
	assert.Equal(t, " x                  │                 1", got[2])
	assert.Equal(t, " y                  │                22", got[3])
}

func TestMarkdownMultiLineCommentKeepsItsColourOnEachRow(t *testing.T) {
	got := newMarkdown(40, mocha()).lines("k", "```go\n/* one\ntwo */\n```")
	require.Len(t, got, 2)
	for _, l := range got {
		assert.Regexp(t, `^\x1b\[[0-9;]*m`, l)
	}
}

func TestMarkdownCutRowsCarryTheirStyle(t *testing.T) {
	got := newMarkdown(10, mocha()).lines("k", "```go\n// "+strings.Repeat("comment ", 6)+"\n```")
	require.Greater(t, len(got), 1)
	for _, l := range got {
		assert.Regexp(t, `^\x1b\[[0-9;]*m`, l)
		assert.Regexp(t, `\x1b\[0?m$`, l)
	}
}

func TestMarkdownTextAndLists(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"text under a heading", "## H\ntext", []string{"H", "text"}},
		{"ordered list from 3", "3. a\n4. b", []string{"3. a", "4. b"}},
		{"nested list", "- a\n  - b\n- c", []string{"- a", "  - b", "- c"}},
		{"empty items keep their marker", "1.\n2. two", []string{"1.", "2. two"}},
		{"empty bullet", "-\n- b", []string{"-", "- b"}},
		{"task checkbox", "- [x] done\n- [ ] todo", []string{"- [x] done", "- [ ] todo"}},
		{"indented code", "    a := 1\n    b := 2", []string{"a := 1", "b := 2"}},
		{"thematic break", "a\n\n---\n\nb", []string{"a", "", strings.Repeat("─", 20), "", "b"}},
		{"inline html as written", "a <b>bold</b> c", []string{"a <b>bold</b> c"}},
		{"image as text and URL", "![alt](https://e.com/i)", []string{"alt https://e.com/i"}},
		{"code span across a line", "a `b\nc` d", []string{"a b c d"}},
		{"escaped entity", `\&amp; &amp;`, []string{"&amp; &"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, renderMarkdown(tc.src, 20))
		})
	}
}

func TestMarkdownLinksDrawOnceWithTheirTarget(t *testing.T) {
	for _, tc := range []struct {
		name, src, want, target string
	}{
		{"autolink", "<https://e.com/a>", "https://e.com/a", "https://e.com/a"},
		{"link text equal to the URL", "[https://e.com/a](https://e.com/a)", "https://e.com/a", "https://e.com/a"},
		{"email autolink", "<me@e.com>", "me@e.com", "mailto:me@e.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newMarkdown(60, mocha()).lines("k", tc.src)
			require.Len(t, got, 1)
			assert.Equal(t, tc.want, ansi.Strip(got[0]))
			assert.Contains(t, got[0], "\x1b]8;;"+tc.target)
		})
	}
}

func TestMarkdownWideTableFitsTheWidthInsideAList(t *testing.T) {
	const src = "- | a | b |\n  |---|---|\n  | " + "xxxxxxxxxxxxxxxxxxxx | yyyyyyyyyyyyyyyyyyyy |"
	for _, l := range renderMarkdown(src, 20) {
		assert.LessOrEqual(t, ansi.StringWidth(l), 20)
	}
}
