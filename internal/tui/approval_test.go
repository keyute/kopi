package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

var rule40 = strings.Repeat("─", 40)

func plainApprovalRows(a event.ApprovalRequested, choice, maxPreview int) []string {
	km := newKeyMap(config.DefaultKeys())
	rows := approvalRows(&a, choice, &km, 40, maxPreview, 100, mocha())
	for i, r := range rows {
		rows[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return rows
}

func TestApprovalRowsWithNonpositiveMaxRows(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	for _, maxRows := range []int{-4, -1, 0} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			assert.Nil(t, approvalRows(nil, 0, &km, 40, 10, maxRows, mocha()))
		})
	}
}

func TestApprovalRowsShowTitlePreviewAndOptions(t *testing.T) {
	a := event.ApprovalRequested{
		CallID: "c1", Title: "Edit a.go",
		Preview: []tool.PreviewLine{
			{Kind: tool.Context, Text: "keep"},
			{Kind: tool.Removed, Text: "old"},
			{Kind: tool.Added, Text: "new"},
		},
		Edits: true,
	}
	assert.Equal(t, []string{
		rule40,
		"  Edit a.go",
		"    keep",
		"  - old",
		"  + new",
		"",
		"  1. Yes",
		"❭ 2. Yes, allow all edits this session",
		"  3. No (esc)",
		rule40,
	}, plainApprovalRows(a, 1, 10))
}

func TestApprovalRowsWithoutEditsOfferNoAsTwo(t *testing.T) {
	a := event.ApprovalRequested{CallID: "c1", Title: "Write a.go"}
	assert.Equal(t, []string{rule40, "  Write a.go", "", "❭ 1. Yes", "  2. No (esc)", rule40}, plainApprovalRows(a, 0, 10))
}

func TestApprovalRowsCapThePreview(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go"}
	for range 10 {
		a.Preview = append(a.Preview, tool.PreviewLine{Kind: tool.Added, Text: "x"})
	}
	rows := plainApprovalRows(a, 0, 4)
	assert.Equal(t, []string{rule40, "  Write a.go", "  + x", "  + x", "  + x", "    … 7 more lines", ""}, rows[:7])

	a.Preview = a.Preview[:4]
	assert.Equal(t, []string{rule40, "  Write a.go", "  + x", "  + x", "  + x", "  + x", ""}, plainApprovalRows(a, 0, 4)[:7], "an exact fit has no marker")
}

func TestApprovalRowsCutTheTopFirst(t *testing.T) {
	a := event.ApprovalRequested{
		Title:   "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
	km := newKeyMap(config.DefaultKeys())
	rows := approvalRows(&a, 0, &km, 40, 10, 4, mocha())
	for i, r := range rows {
		rows[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	assert.Equal(t, []string{"❭ 1. Yes", "  2. Yes, allow all edits this session", "  3. No (esc)", rule40}, rows)
}

func TestPreviewRowsWrappedBudget(t *testing.T) {
	th := mocha()
	line := tool.PreviewLine{Kind: tool.Added, Text: "abcdefgh"}
	for _, tt := range []struct {
		name  string
		lines []tool.PreviewLine
		limit int
		want  []string
	}{
		{"exact wrapped fit", []tool.PreviewLine{line}, 2, previewLine(line, 6, th)},
		{"whole line omitted", []tool.PreviewLine{line}, 1, []string{"  … 1"}},
		{"reserve marker", []tool.PreviewLine{line, line}, 2, []string{"  … 2"}},
		{"multiline overflow", []tool.PreviewLine{{Text: "ok"}, line, line}, 3, []string{"  ok", "  … 2"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := previewRows(tt.lines, tt.limit, 6, th)
			want := tt.want
			for i := range got {
				got[i] = ansi.Strip(got[i])
			}
			for i := range want {
				want[i] = ansi.Strip(want[i])
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestPreviewFitsMatchesWrapping(t *testing.T) {
	texts := []string{
		"", "abcdefgh", "one two three four", "a\nb\nc", "a\tbc",
		strings.Repeat("word- ", 100), strings.Repeat("界👩‍💻e\u0301 ", 100),
		strings.Repeat("a", 255) + "👩‍💻" + strings.Repeat("b", 300),
		strings.Repeat("a", 253) + "\U000e0100" + strings.Repeat("\u0301", 300),
		strings.Repeat("a", 255) + "\x1b[31m\u0301\x1b[0m" + strings.Repeat("b", 300),
		strings.Repeat(" \t", 300), strings.Repeat("\x1b[31m\x00\x7f", 100) + "ok",
	}
	for i, text := range texts {
		for _, width := range []int{-2, 0, 1, 2, 6, 20, 80} {
			for _, budget := range []int{0, 1, 2, 3, 20, 253, 300} {
				t.Run(fmt.Sprintf("%d/width%d/budget%d", i, width, budget), func(t *testing.T) {
					assert.Equal(t, len(wrap(text, width)) <= budget, previewFits(text, width, budget))
				})
			}
		}
	}
}

func TestPreviewRowsHugeLineBoundedAllocations(t *testing.T) {
	th := mocha()
	short := []tool.PreviewLine{{Kind: tool.Added, Text: strings.Repeat("x", 4096)}}
	huge := []tool.PreviewLine{{Kind: tool.Added, Text: strings.Repeat("x", 4<<20)}}
	assert.Equal(t, previewRows(short, 3, 20, th), previewRows(huge, 3, 20, th))
	assert.Equal(t, []string{hang("  ", fg(th.dim), "… 1 more line", fg(th.dim), 20)[0]}, previewRows(huge, 3, 20, th))
	shortAllocs := testing.AllocsPerRun(10, func() { previewRows(short, 3, 20, th) })
	hugeAllocs := testing.AllocsPerRun(10, func() { previewRows(huge, 3, 20, th) })
	// Rendering uses pools, so allow a small constant variation, not growth
	// proportional to the omitted suffix.
	assert.LessOrEqual(t, hugeAllocs, shortAllocs+2, "an overflowing suffix must not increase allocation work")
}

func BenchmarkPreviewRowsHugeLine(b *testing.B) {
	th := mocha()
	for _, size := range []int{4096, 4 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			p := []tool.PreviewLine{{Kind: tool.Added, Text: strings.Repeat("x", size)}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				previewRows(p, 3, 20, th)
			}
		})
	}
}

func TestApprovalRowsNameTheRuleOnlyWhenSet(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit .git/config", Rule: "Edit(.git) (built-in)"}
	assert.Equal(t, "  asked by Edit(.git) (built-in)", plainApprovalRows(a, 0, 10)[2])

	a.Rule = ""
	assert.NotContains(t, strings.Join(plainApprovalRows(a, 0, 10), "\n"), "asked by")
}
