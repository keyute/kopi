package tui

import (
	"bufio"
	"bytes"
	stdhtml "html"
	"image/color"
	"io"
	"strconv"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer/html"
	gmtext "github.com/yuin/goldmark/text"
)

var markdownEngine = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// markdown draws assistant text for one width and theme. goldmark parses the
// text; each top-level block is drawn on its own and kept, so a streaming
// delta redraws only the blocks it changed: in practice the last.
type markdown struct {
	width  int
	th     theme
	blocks map[string]drawn
	// renders counts block renders.
	renders int
}

// drawn is what lines last drew for a key.
type drawn struct {
	text   string
	chunks []chunk
}

// chunk is one drawn top-level block. key is the block as goldmark renders it
// to HTML, with raw HTML kept. Its rows are kept only while the text grows and
// the HTML stays the same: growth changes a block's drawing only through what
// that HTML carries (its text, a link a later reference definition resolves,
// list and table attributes, a fence's language).
type chunk struct {
	key   string
	lines []string
}

func newMarkdown(width int, th theme) *markdown {
	return &markdown{width: width, th: th, blocks: map[string]drawn{}}
}

// lines draws the text of the assistant block keyed key; top-level blocks
// are joined by a blank line where gap puts one.
func (md *markdown) lines(key, text string) []string {
	src := []byte(clean(text))
	doc := markdownEngine.Parser().Parse(gmtext.NewReader(src))
	var prev []chunk
	if last := md.blocks[key]; strings.HasPrefix(text, last.text) {
		prev = last.chunks
	}
	var chunks []chunk
	var out []string
	var buf bytes.Buffer
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		buf.Reset()
		_ = markdownEngine.Renderer().Render(&buf, src, n)
		c := chunk{key: buf.String()}
		if i := len(chunks); i < len(prev) && prev[i].key == c.key {
			c.lines = prev[i].lines
		} else {
			md.renders++
			c.lines = md.block(n, src, md.width, fg(md.th.text))
		}
		chunks = append(chunks, c)
		if len(out) > 0 && len(c.lines) > 0 && gap(n) {
			out = append(out, "")
		}
		out = append(out, c.lines...)
	}
	md.blocks[key] = drawn{text: text, chunks: chunks}
	return out
}

// block draws one block node at width; st is the style of its plain text.
func (md *markdown) block(n ast.Node, src []byte, width int, st lipgloss.Style) []string {
	width = max(1, width)
	switch n := n.(type) {
	case *ast.Heading:
		if n.FirstChild() == nil {
			return nil
		}
		return wrapRows(md.inline(n, src, fg(md.th.heading).Bold(true)), width)
	case *ast.Blockquote:
		return prefixed(st, "│ ", "│ ", md.children(n, src, width-2, fg(md.th.muted), true))
	case *ast.List:
		var out []string
		num := n.Start
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			marker := "- "
			if n.IsOrdered() {
				marker = strconv.Itoa(num) + ". "
				num++
			}
			rows := md.children(item, src, width-len(marker), st, !n.IsTight)
			if len(rows) == 0 {
				rows = []string{""}
			}
			out = append(out, prefixed(st, marker, strings.Repeat(" ", len(marker)), rows)...)
			if !n.IsTight && item.NextSibling() != nil {
				out = append(out, "")
			}
		}
		return out
	case *ast.FencedCodeBlock:
		return md.code(string(n.Language(src)), rawText(n, src), width)
	case *ast.CodeBlock:
		return md.code("", rawText(n, src), width)
	case *ast.ThematicBreak:
		return []string{fg(md.th.dim).Render(strings.Repeat("─", width))}
	case *east.Table:
		return md.table(n, src, width, st)
	case *ast.Paragraph, *ast.TextBlock:
		return wrapRows(md.inline(n, src, st), width)
	case *ast.LinkReferenceDefinition:
		return nil
	}
	// HTML and any block without a drawing of its own show as written.
	var out []string
	for l := range strings.SplitSeq(strings.TrimRight(rawText(n, src), "\n"), "\n") {
		out = append(out, wrapRows(st.Render(l), width)...)
	}
	return out
}

// gap reports whether a blank row goes above block n: always, except under a
// heading the source puts text right under.
func gap(n ast.Node) bool {
	if prev := n.PreviousSibling(); prev == nil || prev.Kind() != ast.KindHeading {
		return true
	}
	b, ok := n.(interface{ HasBlankPreviousLines() bool })
	return !ok || b.HasBlankPreviousLines()
}

// children draws the blocks under n, a blank line between them when loose.
func (md *markdown) children(n ast.Node, src []byte, width int, st lipgloss.Style, loose bool) []string {
	var out []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		rows := md.block(c, src, width, st)
		if loose && len(out) > 0 && len(rows) > 0 && gap(c) {
			out = append(out, "")
		}
		out = append(out, rows...)
	}
	return out
}

// prefixed puts first before the first row and rest before the others.
func prefixed(st lipgloss.Style, first, rest string, rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		p := rest
		if i == 0 {
			p = first
		}
		if r == "" {
			p = strings.TrimRight(p, " ")
		}
		out[i] = st.Render(p) + r
	}
	return out
}

func wrapRows(s string, width int) []string {
	return strings.Split(lipgloss.Wrap(s, width, ""), "\n")
}

func rawText(n ast.Node, src []byte) string {
	var b strings.Builder
	for i := range n.Lines().Len() {
		seg := n.Lines().At(i)
		b.Write(seg.Value(src))
	}
	// goldmark keeps the closing line of HTML block types 1 to 5 apart.
	if h, ok := n.(*ast.HTMLBlock); ok && h.HasClosure() {
		b.Write(h.ClosureLine.Value(src))
	}
	return b.String()
}

// inline draws the inline nodes under n; st is the style they inherit.
func (md *markdown) inline(n ast.Node, src []byte, st lipgloss.Style) string {
	var b strings.Builder
	link := func(c ast.Node, url string) {
		text := md.inline(c, src, st.Foreground(md.th.link).Hyperlink(url))
		b.WriteString(text)
		if xansi.Strip(text) != url {
			if text != "" {
				b.WriteString(st.Render(" "))
			}
			b.WriteString(st.Foreground(md.th.link).Hyperlink(url).Render(url))
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Text:
			text := c.Value(src)
			if !c.IsRaw() {
				text = plainText(text)
			}
			b.WriteString(st.Render(clean(string(text))))
			if c.HardLineBreak() {
				b.WriteByte('\n')
			} else if c.SoftLineBreak() {
				b.WriteString(st.Render(" "))
			}
		case *ast.String:
			b.WriteString(st.Render(string(c.Value)))
		case *ast.CodeSpan:
			// A newline inside a span is a space, as in CommonMark.
			span := strings.ReplaceAll(xansi.Strip(md.inline(c, src, lipgloss.NewStyle())), "\n", " ")
			b.WriteString(st.Foreground(md.th.code).Render(span))
		case *ast.Emphasis:
			if c.Level == 2 {
				b.WriteString(md.inline(c, src, st.Bold(true)))
			} else {
				b.WriteString(md.inline(c, src, st.Italic(true)))
			}
		case *east.Strikethrough:
			b.WriteString(md.inline(c, src, st.Strikethrough(true)))
		case *ast.Link:
			link(c, clean(string(plainText(c.Destination))))
		case *ast.Image:
			link(c, clean(string(plainText(c.Destination))))
		case *ast.AutoLink:
			url := string(c.URL(src))
			if c.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(url), "mailto:") {
				url = "mailto:" + url
			}
			b.WriteString(st.Foreground(md.th.link).Hyperlink(url).Render(string(c.Label(src))))
		case *ast.RawHTML:
			for i := range c.Segments.Len() {
				seg := c.Segments.At(i)
				v := string(seg.Value(src))
				t := strings.TrimSuffix(v, "\n")
				b.WriteString(st.Render(t))
				if t != v {
					b.WriteString(st.Render(" "))
				}
			}
		case *east.TaskCheckBox:
			if c.IsChecked {
				b.WriteString(st.Render("[x] "))
			} else {
				b.WriteString(st.Render("[ ] "))
			}
		default:
			b.WriteString(md.inline(c, src, st))
		}
	}
	return b.String()
}

// code draws a code block: plain text in the theme's code colour, and the
// tokens of a language Chroma knows in the code theme's colours. A line wider
// than width is cut, not wrapped at a word.
func (md *markdown) code(lang, body string, width int) []string {
	var rows []string
	var it chroma.Iterator
	if lexer := lexers.Get(lang); lexer != nil {
		it, _ = chroma.Coalesce(lexer).Tokenise(nil, body)
	}
	if it != nil {
		f, style := formatters.Get(md.th.codeFormatter), codeStyle(md.th.codeTheme, md.th.code)
		for _, tokens := range chroma.SplitTokensIntoLines(it.Tokens()) {
			var b strings.Builder
			_ = f.Format(&b, style, chroma.Literator(tokens...))
			rows = append(rows, strings.ReplaceAll(b.String(), "\n", ""))
		}
	} else {
		for l := range strings.Lines(body) {
			rows = append(rows, fg(md.th.code).Render(strings.TrimSuffix(l, "\n")))
		}
	}
	var out []string
	for _, r := range rows {
		out = append(out, cutRow(r, width)...)
	}
	return out
}

// cutRow cuts a row at width, not at a word. Each cut row closes the styles
// and link it opened and the next reopens them.
func cutRow(row string, width int) []string {
	var b strings.Builder
	w := lipgloss.NewWrapWriter(&b)
	_, _ = io.WriteString(w, xansi.Hardwrap(row, width, true))
	_ = w.Close()
	return strings.Split(b.String(), "\n")
}

// plainText resolves backslash escapes and character references in one pass,
// as goldmark's HTML writer does: it writes HTML, whose escaping is undone.
func plainText(text []byte) []byte {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	html.DefaultWriter.Write(w, text)
	_ = w.Flush()
	return []byte(stdhtml.UnescapeString(buf.String()))
}

func (md *markdown) table(n *east.Table, src []byte, width int, st lipgloss.Style) []string {
	t := table.New().
		Border(lipgloss.NormalBorder()).BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderStyle(st).
		StyleFunc(func(_, col int) lipgloss.Style {
			cell := lipgloss.NewStyle().Padding(0, 1)
			switch n.Alignments[col] {
			case east.AlignRight:
				return cell.Align(lipgloss.Right)
			case east.AlignCenter:
				return cell.Align(lipgloss.Center)
			}
			return cell
		})
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, md.inline(cell, src, st))
		}
		if row.Kind() == east.KindTableHeader {
			t.Headers(cells...)
		} else {
			t.Row(cells...)
		}
	}
	// A table that fits spans the width. For a wider one a width would make
	// lipgloss ellipsize headers and split words, losing text, so it keeps its
	// natural width and a row wider than width is cut.
	s := t.String()
	if lipgloss.Width(s) <= width {
		s = t.Width(width).String()
	}
	var rows []string
	for _, r := range strings.Split(s, "\n") {
		r = strings.TrimRight(r, " ")
		if sep := xansi.Strip(r); sep != "" && strings.Trim(sep, "─┼") == "" {
			// The separator row has no words to keep.
			rows = append(rows, xansi.Truncate(r, width, ""))
			continue
		}
		rows = append(rows, cutRow(r, width)...)
	}
	return rows
}

var (
	codeStylesMu sync.Mutex
	codeStyles   = map[string]*chroma.Style{}
)

// codeStyle is the Chroma style base with no background (Catppuccin shades
// diff lines, and only what the user typed is shaded here) and with its plain
// text in the theme's code colour.
func codeStyle(base string, code color.Color) *chroma.Style {
	r, g, b, _ := code.RGBA()
	col := chroma.NewColour(uint8(r>>8), uint8(g>>8), uint8(b>>8))
	key := base + col.String()
	codeStylesMu.Lock()
	defer codeStylesMu.Unlock()
	if s, ok := codeStyles[key]; ok {
		return s
	}
	src := styles.Get(base)
	plain := src.Get(chroma.Text).Colour
	sb := chroma.NewStyleBuilder(key)
	for _, tt := range append(src.Types(), chroma.Background) {
		e := src.Get(tt)
		e.Background = 0
		if e.Colour == plain || tt == chroma.Background {
			e.Colour = col
		}
		sb.AddEntry(tt, e)
	}
	s, err := sb.Build()
	if err != nil {
		s = src
	}
	codeStyles[key] = s
	return s
}
