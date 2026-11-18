package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
)

// markdown renders assistant text for one width and theme. Text is split
// into top-level chunks rendered independently, and each block's rendered
// chunks are kept, so a streaming delta re-renders only the chunks it
// changed: in practice the last.
type markdown struct {
	r      *glamour.TermRenderer
	width  int
	blocks map[string][]chunk
	// renders counts chunk renders.
	renders int
}

type chunk struct {
	source string
	lines  []string
}

func newMarkdown(width int, th theme) *markdown {
	// A failed renderer leaves r nil, and every chunk falls back to plain
	// wrapped text.
	r, _ := glamour.NewTermRenderer(
		glamour.WithStyles(markdownStyle(width, th)),
		glamour.WithWordWrap(width),
		glamour.WithChromaFormatter(th.codeFormatter),
	)
	return &markdown{r: r, width: width, blocks: map[string][]chunk{}}
}

// lines renders the text of the assistant block keyed key; chunks are
// joined by one blank line.
func (md *markdown) lines(key, text string) []string {
	prev := md.blocks[key]
	var chunks []chunk
	var out []string
	sources := splitChunks(text)
	// Reference definitions apply across the whole document, not just a chunk.
	if strings.Contains(text, "]:") {
		sources = []string{text}
	}
	for i, src := range sources {
		c := chunk{source: src}
		if i < len(prev) && prev[i].source == src {
			c.lines = prev[i].lines
		} else {
			c.lines = md.render(src)
		}
		chunks = append(chunks, c)
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, c.lines...)
	}
	md.blocks[key] = chunks
	return out
}

// render falls back to plain wrapped text when Glamour fails. Tabs become
// spaces because they have no fixed width.
func (md *markdown) render(src string) []string {
	md.renders++
	src = strings.ReplaceAll(src, "\t", "    ")
	if md.r == nil {
		return wrap(src, md.width)
	}
	s, err := md.r.Render(src)
	if err != nil {
		return wrap(src, md.width)
	}
	return fitLines(strings.Split(s, "\n"), md.width)
}

// fitLines drops Glamour's trailing padding from each line and its blank
// lines at either end, and breaks lines it leaves wider than width, such as
// long code lines.
func fitLines(rendered []string, width int) []string {
	var ls []string
	for _, l := range rendered {
		l = xansi.Truncate(l, xansi.StringWidth(strings.TrimRight(xansi.Strip(l), " ")), "")
		if xansi.StringWidth(l) > width {
			ls = append(ls, strings.Split(xansi.Hardwrap(l, width, true), "\n")...)
			continue
		}
		ls = append(ls, l)
	}
	for len(ls) > 0 && xansi.Strip(ls[0]) == "" {
		ls = ls[1:]
	}
	for len(ls) > 0 && xansi.Strip(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// splitChunks cuts text at blank lines outside a fenced code block where the
// next line starts a new top-level block, so lists, quotes, tables and
// indented continuations stay whole. Blank lines at a cut are dropped.
func splitChunks(text string) []string {
	lines := strings.Split(text, "\n")
	var chunks []string
	var cur []string
	fence := ""
	flush := func() {
		if s := strings.Trim(strings.Join(cur, "\n"), "\n"); s != "" {
			chunks = append(chunks, s)
		}
		cur = nil
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if fence == "" && strings.TrimSpace(l) == "" {
			j := i
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j < len(lines) && startsBlock(lines[j]) {
				flush()
				i = j - 1
				continue
			}
		}
		fence = nextFence(fence, l)
		cur = append(cur, l)
	}
	flush()
	return chunks
}

// startsBlock reports whether a line after a blank line begins a new
// top-level block rather than continuing the one before.
func startsBlock(l string) bool {
	if l == "" || l[0] == ' ' || l[0] == '\t' || l[0] == '>' || l[0] == '|' {
		return false
	}
	return !isListMarker(l)
}

func isListMarker(l string) bool {
	marker := func(rest string) bool { return rest == "" || rest[0] == ' ' || rest[0] == '\t' }
	switch l[0] {
	case '-', '*', '+':
		return marker(l[1:])
	}
	i := 0
	for i < len(l) && l[i] >= '0' && l[i] <= '9' {
		i++
	}
	return i > 0 && i < len(l) && (l[i] == '.' || l[i] == ')') && marker(l[i+1:])
}

// nextFence is the fence open after line l: fence is the open one's marker
// ("```" or "~~~", possibly longer), or "" outside a fence.
func nextFence(fence, l string) string {
	t := strings.TrimSpace(l)
	if fence != "" {
		if strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
			return ""
		}
		return fence
	}
	for _, c := range []string{"`", "~"} {
		n := len(t) - len(strings.TrimLeft(t, c))
		if n >= 3 {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

var (
	codeThemesMu sync.Mutex
	codeThemes   = map[string]string{}
)

// codeStyle registers, once per base, a copy of the Chroma style base that
// draws no background: Catppuccin shades diff lines, and only what the user
// typed is shaded here. It returns the copy's name, or "" if the copy fails.
func codeStyle(base string) string {
	codeThemesMu.Lock()
	defer codeThemesMu.Unlock()
	if name, ok := codeThemes[base]; ok {
		return name
	}
	src := styles.Get(base)
	b := chroma.NewStyleBuilder("kopi-" + base)
	for _, tt := range src.Types() {
		e := src.Get(tt)
		e.Background = 0
		b.AddEntry(tt, e)
	}
	name := ""
	if s, err := b.Build(); err == nil {
		name = styles.Register(s).Name
	}
	codeThemes[base] = name
	return name
}

func markdownStyle(width int, th theme) ansi.StyleConfig {
	c := func(col color.Color) *string {
		switch col := col.(type) {
		case lipgloss.NoColor:
			return nil
		case xansi.BasicColor:
			s := strconv.Itoa(int(col))
			return &s
		case lipgloss.ANSIColor:
			s := strconv.Itoa(int(col))
			return &s
		}
		r, g, b, _ := col.RGBA()
		s := fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
		return &s
	}
	yes := true
	zero, one := uint(0), uint(1)
	gutter := "│ "
	return ansi.StyleConfig{
		Document:       ansi.StyleBlock{Margin: &zero},
		BlockQuote:     ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: c(th.muted)}, Indent: &one, IndentToken: &gutter},
		List:           ansi.StyleList{LevelIndent: 2},
		Heading:        ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: c(th.heading), Bold: &yes}},
		Strong:         ansi.StylePrimitive{Bold: &yes},
		Emph:           ansi.StylePrimitive{Italic: &yes},
		Strikethrough:  ansi.StylePrimitive{CrossedOut: &yes},
		HorizontalRule: ansi.StylePrimitive{Color: c(th.dim), Format: "\n" + strings.Repeat("─", width) + "\n"},
		Item:           ansi.StylePrimitive{BlockPrefix: "- "},
		Enumeration:    ansi.StylePrimitive{BlockPrefix: ". "},
		Task:           ansi.StyleTask{Ticked: "[x] ", Unticked: "[ ] "},
		Link:           ansi.StylePrimitive{Color: c(th.link)},
		LinkText:       ansi.StylePrimitive{Color: c(th.link)},
		Code:           ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: c(th.code)}},
		CodeBlock:      ansi.StyleCodeBlock{StyleBlock: ansi.StyleBlock{Margin: &zero}, Theme: codeStyle(th.codeTheme)},
	}
}
