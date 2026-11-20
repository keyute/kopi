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
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

var rule40 = strings.Repeat("─", 40)

func plainApprovalRows(a event.ApprovalRequested, choice, maxPreview int) []string {
	km := newKeyMap(config.DefaultKeys())
	d := newApprovalDialog(&a, &km, mocha())
	d.sel = choice
	return plainRows(approvalRows(d, &approvalPreview{}, &a, &km, 40, maxPreview, 100, mocha()))
}

func TestApprovalRowsWithNonpositiveMaxRows(t *testing.T) {
	for _, maxRows := range []int{-4, -1, 0} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			assert.Nil(t, approvalRows(nil, nil, nil, nil, 40, 10, maxRows, mocha()))
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

func numberedPreview(n int) []tool.PreviewLine {
	var p []tool.PreviewLine
	for i := range n {
		p = append(p, tool.PreviewLine{Kind: tool.Added, Text: fmt.Sprintf("l%d", i+1)})
	}
	return p
}

func TestApprovalRowsScrollThePreview(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(10)}
	km := newKeyMap(config.DefaultKeys())
	hint := "  " + scrollHint(&km)
	d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	frame := func() []string { return plainRows(approvalRows(d, p, &a, &km, 40, 4, 100, mocha()))[2:7] }

	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  … 7 more lines", hint}, frame())
	d.scroll.off += d.scroll.visible
	assert.Equal(t, []string{"  … 2 earlier lines", "  + l3", "  + l4", "  … 6 more lines", hint}, frame())
	d.scroll.off += 100
	assert.Equal(t, []string{"  … 7 earlier lines", "  + l8", "  + l9", "  + l10", hint}, frame(), "the end has no more marker")
	d.scroll.off -= 100
	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  … 7 more lines", hint}, frame())

	a.Preview = numberedPreview(4)
	d, p = newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  + l4", ""}, frame(), "an exact fit has no marker or hint")
}

func TestApprovalRowsJoinTheHintToTheNote(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Rule: "Write(a.go)", Preview: numberedPreview(10)}
	km := newKeyMap(config.DefaultKeys())
	rows := plainRows(approvalRows(newApprovalDialog(&a, &km, mocha()), &approvalPreview{}, &a, &km, 60, 4, 100, mocha()))
	assert.Equal(t, "  asked by Write(a.go) · "+scrollHint(&km), rows[6])
}

func TestApprovalRowsReachTheEndOfALongLine(t *testing.T) {
	a := event.ApprovalRequested{Title: "Bash", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: strings.Repeat("0123456789", 20) + "END"}}}
	km := newKeyMap(config.DefaultKeys())
	d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	var rows []string
	for range 20 {
		rows = plainRows(approvalRows(d, p, &a, &km, 40, 4, 100, mocha()))
		if strings.Contains(strings.Join(rows, ""), "END") {
			break
		}
		d.scroll.off += d.scroll.visible
	}
	assert.Contains(t, rows, "    01234567890123456789END")
	assert.NotContains(t, strings.Join(rows, "\n"), "more line")
}

func TestApprovalRowsReachTheEndWhenTheHintTakesARow(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(10)}
	km := newKeyMap(config.DefaultKeys())
	d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	var rows []string
	for range 20 {
		rows = plainRows(approvalRows(d, p, &a, &km, 40, 4, 10, mocha()))
		if slices.Contains(rows, "  + l10") {
			break
		}
		d.scroll.off += d.scroll.visible
	}
	assert.Contains(t, rows, "  + l10")
}

func TestApprovalRowsKeepTheOffsetAcrossAWidthChange(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(40)}
	km := newKeyMap(config.DefaultKeys())
	d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	approvalRows(d, p, &a, &km, 40, 4, 100, mocha())
	d.scroll.off = 30
	approvalRows(d, p, &a, &km, 40, 4, 100, mocha())
	require.Equal(t, 30, d.scroll.off)

	for _, width := range []int{12, 80} {
		rows := plainRows(approvalRows(d, p, &a, &km, width, 4, 100, mocha()))
		assert.Equal(t, width, p.width+2)
		assert.Equal(t, "  … 30 earlier lines", ansi.Strip(rows[2]), "width %d", width)
	}
}

func TestApprovalRowsKeepTheFieldWithATallDraftAndALongPreview(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: numberedPreview(30), Edits: true}
	km := newKeyMap(config.DefaultKeys())
	d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
	d.move(2)
	d.paste("one\ntwo\nthree\nfour\nfive")
	rows := plainRows(approvalRows(d, p, &a, &km, 40, 3, 12, mocha()))
	assert.LessOrEqual(t, len(rows), 12)
	assert.Equal(t, "  five", rows[len(rows)-2])
	assert.Equal(t, rule40, rows[len(rows)-1])
	assert.Contains(t, rows, "❭ 3. No (esc)")
	assert.True(t, d.scroll.cut())
}

func TestApprovalRowsSecondFrameAllocationsDoNotGrowWithThePreview(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool randomly drops items under -race, so allocation counts vary")
	}
	km := newKeyMap(config.DefaultKeys())
	allocs := func(n int) float64 {
		a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(n)}
		d, p := newApprovalDialog(&a, &km, mocha()), &approvalPreview{}
		approvalRows(d, p, &a, &km, 40, 4, 100, mocha())
		return testing.AllocsPerRun(10, func() { approvalRows(d, p, &a, &km, 40, 4, 100, mocha()) })
	}
	assert.LessOrEqual(t, allocs(5000), allocs(50)+2)
}

func BenchmarkApprovalRowsFirstFrame(b *testing.B) {
	km := newKeyMap(config.DefaultKeys())
	for _, n := range []int{100, 5000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(n)}
			d := newApprovalDialog(&a, &km, mocha())
			b.ReportAllocs()
			for b.Loop() {
				approvalRows(d, &approvalPreview{}, &a, &km, 40, 10, 100, mocha())
			}
		})
	}
}

func BenchmarkApprovalRowsFirstFrameHugeLine(b *testing.B) {
	km := newKeyMap(config.DefaultKeys())
	for _, size := range []int{4096, 4 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			a := event.ApprovalRequested{Title: "Bash", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: strings.Repeat("x", size)}}}
			d := newApprovalDialog(&a, &km, mocha())
			b.ReportAllocs()
			for b.Loop() {
				approvalRows(d, &approvalPreview{}, &a, &km, 40, 10, 100, mocha())
			}
		})
	}
}

func TestApprovalRowsCutTheTopFirst(t *testing.T) {
	a := event.ApprovalRequested{
		Title:   "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
	km := newKeyMap(config.DefaultKeys())
	rows := plainRows(approvalRows(newApprovalDialog(&a, &km, mocha()), &approvalPreview{}, &a, &km, 40, 10, 4, mocha()))
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
	p := &approvalPreview{}
	f := d.options[2].field
	// "  3. No (esc)  " takes 15 cells.
	approvalRows(d, p, &a, &km, 40, 10, 100, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 7, h: 1}}, d.areas)

	approvalRows(d, p, &a, &km, 40, 10, 4, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 2, h: 1}}, d.areas, "shifted by the cut")

	require.Equal(t, []string{rule40}, plainRows(approvalRows(d, p, &a, &km, 40, 10, 1, mocha())))
	assert.Empty(t, d.areas, "a field the cut reaches is dropped")
}

func TestApprovalRowsKeepTheFieldWithATallDraft(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, maxRows := range []int{5, 6, 7, 8, 12} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			d := newApprovalDialog(&a, &km, mocha())
			p := &approvalPreview{}
			d.move(2)
			d.paste("one\ntwo\nthree\nfour\nfive")
			rows := plainRows(approvalRows(d, p, &a, &km, 40, 3, maxRows, mocha()))
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
	p := &approvalPreview{}
	d.move(2)
	d.paste("one\ntwo")
	assert.Equal(t, []string{"❭ 3. No (esc)", "  two", rule40}, plainRows(approvalRows(d, p, &a, &km, 40, 3, 3, mocha())), "too few rows for the options cuts the top")
}

func TestApprovalRowsDropAnUnselectedFieldBeforeAnOption(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, tt := range []struct{ width, maxPreview, maxRows int }{{60, 2, 4}, {30, 3, 5}} {
		t.Run(fmt.Sprint(tt.width), func(t *testing.T) {
			rule := strings.Repeat("─", tt.width)
			d := newApprovalDialog(&a, &km, mocha())
			p := &approvalPreview{}
			d.move(2)
			d.paste("one\ntwo\nthree")
			d.move(0)
			rows := plainRows(approvalRows(d, p, &a, &km, tt.width, tt.maxPreview, tt.maxRows, mocha()))
			assert.LessOrEqual(t, len(rows), tt.maxRows)
			assert.Equal(t, "❭ 1. Yes", rows[0], "%q", rows)
			assert.Equal(t, []string{"  3. No (esc)", rule}, rows[len(rows)-2:])
			for _, line := range []string{"one", "two", "three"} {
				assert.NotContains(t, rows, "  "+line)
			}

			d.move(2)
			rows = plainRows(approvalRows(d, p, &a, &km, tt.width, tt.maxPreview, tt.maxRows, mocha()))
			assert.Equal(t, []string{"❭ 3. No (esc)", "  three", rule}, rows[len(rows)-3:], "the selected field keeps one row")
		})
	}
}

func TestApprovalRowsNameTheRuleOnlyWhenSet(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit .git/config", Rule: "Edit(.git) (built-in)"}
	assert.Equal(t, "  asked by Edit(.git) (built-in)", plainApprovalRows(a, 0, 10)[2])

	a.Rule = ""
	assert.NotContains(t, strings.Join(plainApprovalRows(a, 0, 10), "\n"), "asked by")
}

func TestApprovalRowsPrefixASubagentTitle(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Agent: "explore", TaskID: "t3"}
	assert.Equal(t, "  explore t3 › Edit a.go", plainApprovalRows(a, 0, 10)[1])

	a.Agent, a.TaskID = "", ""
	assert.Equal(t, "  Edit a.go", plainApprovalRows(a, 0, 10)[1])
}

func TestApprovalRowsNoteTheClassifierReview(t *testing.T) {
	for name, tt := range map[string]struct {
		review policy.Review
		note   string
	}{
		"deny with reason":      {policy.Review{Verdict: policy.VerdictDeny, Reason: "reads a credential"}, "  the classifier would deny this: reads a credential"},
		"deny without reason":   {policy.Review{Verdict: policy.VerdictDeny}, "  the classifier would deny this"},
		"undecided with reason": {policy.Review{Reason: "unclear intent"}, "  the classifier could not decide: unclear intent"},
		"undecided":             {policy.Review{}, "  the classifier could not decide"},
	} {
		t.Run(name, func(t *testing.T) {
			a := event.ApprovalRequested{Title: "Bash", Review: &tt.review}

			// The note wraps at the dialog's width and ends at the blank row.
			rows := plainApprovalRows(a, 0, 10)[2:]
			note := strings.Join(rows[:slices.Index(rows, "")], " ")
			assert.Equal(t, tt.note, strings.ReplaceAll(note, "   ", " "))
		})
	}
}
