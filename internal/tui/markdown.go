package tui

import (
	"bufio"
	"bytes"
	stdhtml "html"
	"image/color"
	"io"
	"regexp"
	"slices"
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
			// Nothing is wider than the width, however deep a block nests.
			c.lines = cutWider(md.block(n, src, md.width, fg(md.th.text)), md.width)
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
		hs := fg(md.th.heading).Bold(true)
		switch {
		case n.Level == 1:
			hs = hs.Underline(true).UnderlineSpaces(true)
		case n.Level >= 3:
			hs = st.Bold(true)
		}
		return wrapRows(md.inline(n, src, hs), width)
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
	if prev := n.PreviousSibling(); prev == nil || prev.Kind() != ast.KindHeading || prev.FirstChild() == nil {
		return true
	}
	return n.HasBlankPreviousLines()
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

const escapeSequence = `\x1b\[[0-9;:]*m|\x1b\]8;[^\x07\x1b]*(?:\x07|\x1b\\)`

var (
	// leadingDashes matches a run of dashes that starts a string or follows a
	// space or newline, with SGR or OSC 8 sequences before and between them.
	leadingDashes = regexp.MustCompile(`((?:^|[ \n])(?:` + escapeSequence + `)*)(-(?:(?:` + escapeSequence + `)*-)*)`)
	dashOrEscape  = regexp.MustCompile(escapeSequence + `|-`)
)

// wrapRows wraps s at width. x/ansi's wrap lets a hyphen after a space hang
// past the width (charmbracelet/x#785), so the dashes of a run that starts a
// word are swapped for U+E000, which it does not break at, and swapped back
// after. A string that already holds U+E000 is wrapped without the swap. A
// row still wider is cut.
func wrapRows(s string, width int) []string {
	swap := !strings.Contains(s, "\ue000")
	if swap {
		s = leadingDashes.ReplaceAllStringFunc(s, func(m string) string {
			g := leadingDashes.FindStringSubmatch(m)
			return g[1] + dashOrEscape.ReplaceAllStringFunc(g[2], func(x string) string {
				if x == "-" {
					return "\ue000"
				}
				return x
			})
		})
	}
	wrapped := lipgloss.Wrap(s, width, "")
	if swap {
		wrapped = strings.ReplaceAll(wrapped, "\ue000", "-")
	}
	return cutWider(strings.Split(wrapped, "\n"), width)
}

// cutWider cuts each row wider than width and keeps the others.
func cutWider(rows []string, width int) []string {
	var out []string
	for _, r := range rows {
		if lipgloss.Width(r) > width {
			out = append(out, cutRow(r, max(1, width))...)
			continue
		}
		out = append(out, r)
	}
	return out
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
		linked := st.Foreground(md.th.link).Hyperlink(url)
		text := md.inline(c, src, linked)
		b.WriteString(text)
		if xansi.Strip(text) != url {
			if text == "" {
				b.WriteString(linked.Render(url))
			} else if url != "" {
				b.WriteString(st.Foreground(md.th.dim).Render(" (" + url + ")"))
			}
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
			if c.HardLineBreak() || c.SoftLineBreak() {
				b.WriteByte('\n')
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
					b.WriteByte('\n')
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
	if lexer := knownLexer(lang); lexer != nil {
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
	cols := len(n.Alignments)
	var cells [][]string
	// longest is the width of each column's longest word, capped at 30 cells.
	widths, longest := make([]int, cols), make([]int, cols)
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		cst := st
		if row.Kind() == east.KindTableHeader {
			cst = st.Bold(true)
		}
		var cs []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			c := md.inline(cell, src, cst)
			widths[len(cs)] = max(widths[len(cs)], lipgloss.Width(c))
			for _, w := range strings.Fields(xansi.Strip(c)) {
				longest[len(cs)] = max(longest[len(cs)], min(lipgloss.Width(w), 30))
			}
			cs = append(cs, c)
		}
		cells = append(cells, cs)
	}
	// Too narrow for a cell per column: the rows as pipe text.
	if width < 4*cols+1 {
		var out []string
		for i, cs := range cells {
			out = append(out, wrapRows(st.Render("| ")+strings.Join(cs, st.Render(" | "))+st.Render(" |"), width)...)
			if i == 0 {
				out = append(out, wrapRows(st.Render("|"+strings.Repeat("---|", cols)), width)...)
			}
		}
		return out
	}
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(fg(md.th.dim)).
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
	// lipgloss shortens a header and can drop the end of a cell when it
	// narrows a table, so a table wider than width is narrowed here: its
	// widest column gives way a cell at a time, down to its longest word while
	// any column is above it, and the cells wrap. Each column has two cells of
	// padding and a rule before it, and one rule ends the row.
	over := 3*cols + 1 - width
	for _, w := range widths {
		over += w
	}
	// A header cell holding a newline would lose its later lines, so it is a row.
	if over <= 0 && !slices.ContainsFunc(cells[0], func(c string) bool { return strings.Contains(c, "\n") }) {
		t.Headers(cells[0]...).Rows(cells[1:]...)
	} else {
		widest := func(floors []int) int {
			at := -1
			for j, w := range widths {
				if w > floors[j] && (at < 0 || w > widths[at]) {
					at = j
				}
			}
			return at
		}
		ones := slices.Repeat([]int{1}, cols)
		for ; over > 0; over-- {
			at := widest(longest)
			if at < 0 {
				at = widest(ones)
			}
			if at < 0 {
				break
			}
			widths[at]--
		}
		for _, cs := range cells {
			for j, c := range cs {
				cs[j] = strings.Join(wrapRows(c, widths[j]), "\n")
			}
		}
		// lipgloss keeps one line of a header, so a wrapped header is a row
		// like the others, with a rule between rows.
		t.BorderRow(true).Rows(cells...)
	}
	return strings.Split(t.String(), "\n")
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
	page := src.Get(chroma.Background).Background
	sb := chroma.NewStyleBuilder(key)
	for _, tt := range append(src.Types(), chroma.Background) {
		e := src.Get(tt)
		// Gruvbox draws a diff's lines in its page colour on a shade of their own.
		if tt != chroma.Background && e.Colour == page && e.Background != page {
			e.Colour = e.Background
		}
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

var (
	lexersMu sync.Mutex
	lexerFor = map[string]chroma.Lexer{}
)

// knownLexer is the Chroma lexer for lang, or nil. Chroma matches an unknown or
// empty name against every lexer's file patterns, which takes milliseconds, so
// the answer is kept per name.
func knownLexer(lang string) chroma.Lexer {
	if lang == "" {
		return nil
	}
	lexersMu.Lock()
	defer lexersMu.Unlock()
	l, ok := lexerFor[lang]
	if !ok {
		l = lexers.Get(lang)
		lexerFor[lang] = l
	}
	return l
}

// codeBlock is a fenced or indented code block of a reply; text has no fence
// and no last line break.
type codeBlock struct{ lang, text string }

// codeBlocks lists the code blocks of text, nested ones too, in order, skipping
// empty ones. text is parsed as written, since clean turns tabs into spaces.
func codeBlocks(text string) []codeBlock {
	src := []byte(text)
	doc := markdownEngine.Parser().Parse(gmtext.NewReader(src))
	var out []codeBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var b codeBlock
		switch n := n.(type) {
		case *ast.FencedCodeBlock:
			b.lang = string(n.Language(src))
		case *ast.CodeBlock:
		default:
			return ast.WalkContinue, nil
		}
		b.text = rawText(n, src)
		if strings.HasSuffix(b.text, "\r\n") {
			b.text = strings.TrimSuffix(b.text, "\r\n")
		} else {
			b.text = strings.TrimSuffix(b.text, "\n")
		}
		if b.text != "" {
			out = append(out, b)
		}
		return ast.WalkSkipChildren, nil
	})
	return out
}
