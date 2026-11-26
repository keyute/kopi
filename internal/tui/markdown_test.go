package tui

import (
	"bytes"
	"fmt"
	"html"
	"image/color"
	"regexp"
	"strings"
	"testing"
	"unicode"

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

	assert.Equal(t, []string{"Results:", "", "┌───┬───┐", "│ a │ b │", "├───┼───┤", "│ 1 │ 2 │", "└───┴───┘"}, got)
}

func TestMarkdownDrawsNoControlBytesFromReferences(t *testing.T) {
	got := strings.Join(renderMarkdown("x &#27;c y\n\n&#155;2J", 40), "\n")

	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\u009b")
	assert.Contains(t, got, "2J")
}

func TestMarkdownSmallTableIsDrawnAtItsContentWidth(t *testing.T) {
	got := renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |", 60)

	assert.Equal(t, []string{"┌───┬───┐", "│ a │ b │", "├───┼───┤", "│ 1 │ 2 │", "└───┴───┘"}, got)
}

func TestMarkdownTableWiderThanTheWidthWrapsItsCells(t *testing.T) {
	const src = "| Name | Description | Default | Type | Notes |\n|---|---|---|---|---|\n| timeout | How long to wait before giving up | 30s | duration | applies per call |\n| retries | How many times | 3 | int | |"
	got := renderMarkdown(src, 82)

	assert.Equal(t, []string{
		"┌─────────┬─────────────────────────┬─────────┬──────────┬──────────────────┐",
		"│ Name    │ Description             │ Default │ Type     │ Notes            │",
		"├─────────┼─────────────────────────┼─────────┼──────────┼──────────────────┤",
		"│ timeout │ How long to wait before │ 30s     │ duration │ applies per call │",
		"│         │ giving up               │         │          │                  │",
		"├─────────┼─────────────────────────┼─────────┼──────────┼──────────────────┤",
		"│ retries │ How many times          │ 3       │ int      │                  │",
		"└─────────┴─────────────────────────┴─────────┴──────────┴──────────────────┘",
	}, got)
	assert.NotContains(t, strings.Join(got, ""), "…")
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

func TestMarkdownReusesListsAndOpenFences(t *testing.T) {
	md := newMarkdown(40, mocha())
	md.lines("k", "- a\n- b\n\npa")
	before := md.renders

	md.lines("k", "- a\n- b\n\npar")

	assert.Equal(t, 1, md.renders-before)
	assert.Equal(t, []string{"code", "", "more"}, renderMarkdown("```\ncode\n\nmore", 40))

	md.lines("k", "- a\n\n```\ncode")
	before = md.renders

	md.lines("k", "- a\n\n```\ncode\nmo")

	assert.Equal(t, 1, md.renders-before)
}

func TestMarkdownInlineStylesEmitTheirSGRCode(t *testing.T) {
	for src, code := range map[string]string{"**b**": "1", "*i*": "3", "~~s~~": "9"} {
		got := newMarkdown(40, mocha()).lines("k", src)
		require.Len(t, got, 1)
		assert.Regexp(t, `\x1b\[(?:[0-9]+;)*`+code+`(?:;[0-9]+)*m`, got[0], src)
	}
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
	got := renderMarkdown("| name | n |\n|:--|--:|\n| x | 1 |\n| y | 22 |", 40)
	assert.Equal(t, []string{"┌──────┬────┐", "│ name │  n │", "├──────┼────┤", "│ x    │  1 │", "│ y    │ 22 │", "└──────┴────┘"}, got)
}

func TestMarkdownTableCentresColumnsPerSource(t *testing.T) {
	got := renderMarkdown("| name | n |\n|:-:|:-:|\n| x | 1 |", 40)
	assert.Equal(t, []string{"┌──────┬───┐", "│ name │ n │", "├──────┼───┤", "│  x   │ 1 │", "└──────┴───┘"}, got)
}

func TestMarkdownEmptyHeadingIsNotTheHeadingABlockSitsUnder(t *testing.T) {
	assert.Equal(t, []string{"before", "", "after"}, renderMarkdown("before\n\n#\nafter", 30))
}

func TestMarkdownRowWithAHangingDashStaysInsideTheWidth(t *testing.T) {
	assert.Equal(t, []string{"the gate", "returns -", "keeps a long row"}, renderMarkdown("the gate returns - keeps a long row", 16))
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
		{"list item that wraps", "- one two three four five six seven\n- b", []string{"- one two three four", "  five six seven", "- b"}},
		{"ordered list from 9 to 10", "9. a b c d e f g h\n1. i j k l m n o p q r", []string{"9. a b c d e f g h", "10. i j k l m n o p", "    q r"}},
		{"quote that wraps with an empty line", "> " + strings.Repeat("word ", 8) + "\n>\n> tail", []string{"│ word word word", "│ word word word", "│ word word", "│", "│ tail"}},
		{"inline html across a line", "x <span\nclass=\"y\">z</span> w", []string{"x <span", "class=\"y\">z</span> w"}},
		{"empty heading", "#\n\nb", []string{"b"}},
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
		{"escaped target", "[https://e.com/a_b](https://e.com/a\\_b)", "https://e.com/a_b", "https://e.com/a_b"},
		{"reference in target", "[a](https://e.com/?x=1&amp;y=2)", "a https://e.com/?x=1&y=2", "https://e.com/?x=1&y=2"},
		{"empty text", "[](https://e.com)", "https://e.com", "https://e.com"},
		{"empty image text", "![](https://e.com/i.png)", "https://e.com/i.png", "https://e.com/i.png"},
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

var markdownTags = regexp.MustCompile(`(?s)<[^>]*>`)

func squashSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func runeCounts(s string) map[rune]int {
	m := map[rune]int{}
	for _, r := range s {
		m[r]++
	}
	return m
}

func TestMarkdownProperties(t *testing.T) {
	const hyphens = "The well-known read-only plan-mode profile - which is chosen from the mode the gate returns - keeps a long-running renderer from being re-drawn on every key-press - end"
	const wide3 = "| File | Change | Why |\n|---|---|---|\n| `tui/markdown.go` | rewritten over goldmark | glamour had no hook |"
	const wide5 = "| Name | Description | Default | Type | Notes |\n|---|---|---|---|---|\n| timeout | How long to wait | 30s | duration | per call |"
	const cjk = "| 名前 | 説明 |\n|---|---|\n| タイムアウト | 待機する **bold** [l](https://x.io) |"
	corpus := []string{
		"# Title\n\nSome text.", "a paragraph\nover two lines",
		"> quoted text\n> over two lines", "- one\n- two\n  - nested",
		"- [ ] todo\n- [x] done", "```go\nfunc main() {}\n```", "```\nplain fence long line\n```",
		"    indented", "above\n\n---\n\nbelow", "| a | b |\n|---|---|\n| 1 | 2 |", "<div>\nhtml block\n</div>",
		"`code`, *em*, **strong**, ~~strike~~, [link](https://x.io/a-b), ![alt](a.png), <https://x.io> <b>raw</b>",
		"hard\\\nbreak and a \\* escape &amp; &lt;entity&gt;",
		"- `c` **s** [l](https://x.io)\n- > quoted\n- ```\n  fenced\n  ```",
		"> - ~~struck~~ ![i](a.png)\n>\n> ```\n> fenced\n> ```",
		hyphens, "- " + hyphens, "> " + hyphens,
		"| a | b |\n|:-:|--:|\n| x | 1 |", wide3, wide5, cjk,
		"- item\n\n  | a | b |\n  |---|---|\n  | one two three | four five six |", "> | k | v |\n> |---|---|\n> | key | a value long enough to wrap |",
		"one paragraph\n\n#\n\nanother paragraph",
		"1. **First**\n\n   Explained here.\n\n   ```sh\n   ! kopi login --flag=value\n   ```\n\n2. **Second**\n   - nested bullet\n     > and a quote",
	}
	th := mocha()
	for _, src := range corpus {
		var buf bytes.Buffer
		require.NoError(t, markdownEngine.Convert([]byte(clean(src)), &buf))
		want := runeCounts(squashSpace(html.UnescapeString(markdownTags.ReplaceAllString(buf.String(), ""))))

		for width := 1; width <= 120; width++ {
			rows := newMarkdown(width, th).lines("k", src)
			for _, r := range rows {
				if ansi.StringWidth(r) > max(2, width) {
					require.FailNowf(t, "row too wide", "width %d for %q: %q", width, src, ansi.Strip(r))
				}
			}

			got := runeCounts(squashSpace(ansi.Strip(strings.Join(rows, "\n"))))
			for r, n := range want {
				if got[r] < n {
					require.FailNowf(t, "text dropped", "rune %q at width %d for %q", r, width, src)
				}
			}

			if width != 80 && width != 24 {
				continue
			}
			md := newMarkdown(width, th)
			var streamed []string
			for i := range len(src) + 1 {
				streamed = md.lines("k", src[:i])
			}
			require.Equal(t, rows, streamed, "streamed reply differs at width %d for %q", width, src)
		}
	}
}
