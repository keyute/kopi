package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

var rule40 = strings.Repeat("─", 40)

func plainApprovalRows(a event.ApprovalRequested, choice, maxPreview int) []string {
	km := newKeyMap(config.DefaultKeys())
	d := newApprovalDialog(&a, &km, mocha())
	d.sel = choice
	return plainRows(approvalRows(d, &a, 40, maxPreview, 100, mocha()))
}

func TestApprovalRowsWithNonpositiveMaxRows(t *testing.T) {
	for _, maxRows := range []int{-4, -1, 0} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			assert.Nil(t, approvalRows(nil, nil, 40, 10, maxRows, mocha()))
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
		"  3. No (esc)  What should change?",
		rule40,
	}, plainApprovalRows(a, 1, 10))
}

func TestApprovalRowsWithoutEditsOfferNoAsTwo(t *testing.T) {
	a := event.ApprovalRequested{CallID: "c1", Title: "Write a.go"}
	assert.Equal(t, []string{rule40, "  Write a.go", "", "❭ 1. Yes", "  2. No (esc)  What should change?", rule40}, plainApprovalRows(a, 0, 10))
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
	rows := plainRows(approvalRows(newApprovalDialog(&a, &km, mocha()), &a, 40, 10, 4, mocha()))
	assert.Equal(t, []string{"❭ 1. Yes", "  2. Yes, allow all edits this session", "  3. No (esc)  What should change?", rule40}, rows)
}

func TestApprovalRowsShiftAreasForTheCut(t *testing.T) {
	a := event.ApprovalRequested{
		Title:   "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
	km := newKeyMap(config.DefaultKeys())
	d := newApprovalDialog(&a, &km, mocha())
	f := d.options[2].field
	// "  3. No (esc)  " takes 15 cells.
	approvalRows(d, &a, 40, 10, 100, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 7, h: 1}}, d.areas)

	approvalRows(d, &a, 40, 10, 4, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 2, h: 1}}, d.areas, "shifted by the cut")

	require.Equal(t, []string{rule40}, plainRows(approvalRows(d, &a, 40, 10, 1, mocha())))
	assert.Empty(t, d.areas, "a field the cut reaches is dropped")
}

func TestApprovalRowsKeepTheFieldWithATallDraft(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, maxRows := range []int{5, 6, 7, 8, 12} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			d := newApprovalDialog(&a, &km, mocha())
			d.move(2)
			d.paste("one\ntwo\nthree\nfour\nfive")
			rows := plainRows(approvalRows(d, &a, 40, 3, maxRows, mocha()))
			assert.LessOrEqual(t, len(rows), maxRows)
			at := slices.Index(rows, "  1. Yes")
			require.GreaterOrEqual(t, at, 0, "%q", rows)
			assert.Equal(t, []string{"  1. Yes", "  2. Yes, allow all edits this session", "❭ 3. No (esc)"}, rows[at:at+3])
			field := rows[at+3 : len(rows)-1]
			assert.NotEmpty(t, field)
			assert.LessOrEqual(t, len(field), 3)
			assert.Equal(t, "  five", field[len(field)-1])
			assert.Equal(t, rule40, rows[len(rows)-1])
		})
	}

	d := newApprovalDialog(&a, &km, mocha())
	d.move(2)
	d.paste("one\ntwo")
	assert.Equal(t, []string{"❭ 3. No (esc)", "  two", rule40}, plainRows(approvalRows(d, &a, 40, 3, 3, mocha())), "too few rows for the options cuts the top")
}

func TestApprovalRowsDropAnUnselectedFieldBeforeAnOption(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, tt := range []struct{ width, maxPreview, maxRows int }{{60, 2, 4}, {30, 3, 5}} {
		t.Run(fmt.Sprint(tt.width), func(t *testing.T) {
			rule := strings.Repeat("─", tt.width)
			d := newApprovalDialog(&a, &km, mocha())
			d.move(2)
			d.paste("one\ntwo\nthree")
			d.move(0)
			rows := plainRows(approvalRows(d, &a, tt.width, tt.maxPreview, tt.maxRows, mocha()))
			assert.LessOrEqual(t, len(rows), tt.maxRows)
			assert.Equal(t, "❭ 1. Yes", rows[0], "%q", rows)
			assert.Equal(t, []string{"  3. No (esc)", rule}, rows[len(rows)-2:])
			for _, line := range []string{"one", "two", "three"} {
				assert.NotContains(t, rows, "  "+line)
			}

			d.move(2)
			rows = plainRows(approvalRows(d, &a, tt.width, tt.maxPreview, tt.maxRows, mocha()))
			assert.Equal(t, []string{"❭ 3. No (esc)", "  three", rule}, rows[len(rows)-3:], "the selected field keeps one row")
		})
	}
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
