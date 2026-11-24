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
	p := newApprovalDialog(&a, &km, mocha())
	p.d.sel = choice
	return plainRows(p.dialog(40, mocha()).rows(40, 100, maxPreview, mocha()))
}

func TestApprovalRowsWithNonpositiveMaxRows(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	for _, maxRows := range []int{-4, -1, 0} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			a := event.ApprovalRequested{}
			p := newApprovalDialog(&a, &km, mocha())
			assert.Nil(t, p.dialog(40, mocha()).rows(40, maxRows, 10, mocha()))
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
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	frame := func() []string { return plainRows(p.dialog(40, mocha()).rows(40, 100, 4, mocha()))[2:7] }

	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  … 7 more lines", hint}, frame())
	d.scroll.off += d.scroll.visible
	assert.Equal(t, []string{"  … 2 earlier lines", "  + l3", "  + l4", "  … 6 more lines", hint}, frame())
	d.scroll.off += 100
	assert.Equal(t, []string{"  … 7 earlier lines", "  + l8", "  + l9", "  + l10", hint}, frame(), "the end has no more marker")
	d.scroll.off -= 100
	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  … 7 more lines", hint}, frame())

	a.Preview = numberedPreview(4)
	p = newApprovalDialog(&a, &km, mocha())
	assert.Equal(t, []string{"  + l1", "  + l2", "  + l3", "  + l4", ""}, frame(), "an exact fit has no marker or hint")
}

func TestPreviewRowsHaveASignColumnOnlyWithSignedLines(t *testing.T) {
	th := mocha()
	context := []tool.PreviewLine{{Kind: tool.Context, Text: "ls -la"}, {Kind: tool.Context, Text: "pwd"}}
	assert.Equal(t, []string{fg(th.muted).Render("ls -la"), fg(th.muted).Render("pwd")}, previewRows(context, 40, th))

	signed := append([]tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}}, context[0])
	assert.Equal(t, []string{
		fg(th.error).Render("- ") + fg(th.error).Render("old"),
		fg(th.success).Render("+ ") + fg(th.success).Render("new"),
		fg(th.dim).Render("  ") + fg(th.dim).Render("ls -la"),
	}, previewRows(signed, 40, th))
}

func TestApprovalRowsJoinTheHintToTheNote(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Rule: "Write(a.go)", Preview: numberedPreview(10)}
	km := newKeyMap(config.DefaultKeys())
	rows := plainRows(newApprovalDialog(&a, &km, mocha()).dialog(60, mocha()).rows(60, 100, 4, mocha()))
	assert.Equal(t, "  asked by Write(a.go) · "+scrollHint(&km), rows[6])
}

func TestApprovalRowsReachTheEndOfALongLine(t *testing.T) {
	a := event.ApprovalRequested{Title: "Bash", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: strings.Repeat("0123456789", 20) + "END"}}}
	km := newKeyMap(config.DefaultKeys())
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	var rows []string
	for range 20 {
		rows = plainRows(p.dialog(40, mocha()).rows(40, 100, 4, mocha()))
		if strings.Contains(strings.Join(rows, ""), "END") {
			break
		}
		d.scroll.off += d.scroll.visible
	}
	assert.Contains(t, rows, "  0123456789END")
	assert.NotContains(t, strings.Join(rows, "\n"), "more line")
}

func TestApprovalRowsReachTheEndWhenTheHintTakesARow(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(10)}
	km := newKeyMap(config.DefaultKeys())
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	var rows []string
	for range 20 {
		rows = plainRows(p.dialog(40, mocha()).rows(40, 10, 4, mocha()))
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
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	p.dialog(40, mocha()).rows(40, 100, 4, mocha())
	d.scroll.off = 30
	p.dialog(40, mocha()).rows(40, 100, 4, mocha())
	require.Equal(t, 30, d.scroll.off)

	for _, width := range []int{12, 80} {
		rows := plainRows(p.dialog(width, mocha()).rows(width, 100, 4, mocha()))
		assert.Equal(t, width, p.preview.width+2)
		assert.Equal(t, "  … 30 earlier lines", ansi.Strip(rows[2]), "width %d", width)
	}
}

func TestApprovalRowsKeepTheFieldWithATallDraftAndALongPreview(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: numberedPreview(30), Edits: true}
	km := newKeyMap(config.DefaultKeys())
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	d.move(2)
	d.paste("one\ntwo\nthree\nfour\nfive")
	rows := plainRows(p.dialog(40, mocha()).rows(40, 12, 3, mocha()))
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
		p := newApprovalDialog(&a, &km, mocha())
		p.dialog(40, mocha()).rows(40, 100, 4, mocha())
		return testing.AllocsPerRun(10, func() { p.dialog(40, mocha()).rows(40, 100, 4, mocha()) })
	}
	assert.LessOrEqual(t, allocs(5000), allocs(50)+2)
}

func BenchmarkApprovalRowsFirstFrame(b *testing.B) {
	km := newKeyMap(config.DefaultKeys())
	for _, n := range []int{100, 5000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			a := event.ApprovalRequested{Title: "Write a.go", Preview: numberedPreview(n)}
			p := newApprovalDialog(&a, &km, mocha())
			b.ReportAllocs()
			for b.Loop() {
				p.preview = approvalPreview{}
				p.dialog(40, mocha()).rows(40, 100, 10, mocha())
			}
		})
	}
}

func BenchmarkApprovalRowsFirstFrameHugeLine(b *testing.B) {
	km := newKeyMap(config.DefaultKeys())
	for _, size := range []int{4096, 4 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			a := event.ApprovalRequested{Title: "Bash", Preview: []tool.PreviewLine{{Kind: tool.Context, Text: strings.Repeat("x", size)}}}
			p := newApprovalDialog(&a, &km, mocha())
			b.ReportAllocs()
			for b.Loop() {
				p.preview = approvalPreview{}
				p.dialog(40, mocha()).rows(40, 100, 10, mocha())
			}
		})
	}
}

func TestApprovalRowsKeepBothRulesAndTheSelectionWhenTheOptionsDoNotFit(t *testing.T) {
	a := event.ApprovalRequested{
		Title:   "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
	km := newKeyMap(config.DefaultKeys())
	rows := plainRows(newApprovalDialog(&a, &km, mocha()).dialog(40, mocha()).rows(40, 4, 10, mocha()))
	assert.Equal(t, []string{rule40, "❭ 1. Yes", "  1 of 3", rule40}, rows)
}

func TestApprovalRowsPlaceAreasOnTheDrawnRows(t *testing.T) {
	a := event.ApprovalRequested{
		Title:   "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
	km := newKeyMap(config.DefaultKeys())
	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	f := d.options[2].field
	// "  3. No (esc)  " takes 15 cells.
	p.dialog(40, mocha()).rows(40, 100, 10, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 7, h: 1}}, d.areas)

	d.move(2)
	p.dialog(40, mocha()).rows(40, 4, 10, mocha())
	assert.Equal(t, []fieldArea{{c: f, x: 15, y: 1, h: 1}}, d.areas, "the selected option's row is all that fits")

	require.Equal(t, []string{rule40}, plainRows(p.dialog(40, mocha()).rows(40, 1, 10, mocha())))
	assert.Empty(t, d.areas, "a field that is not drawn is dropped")
}

func TestApprovalRowsKeepTheFieldWithATallDraft(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, maxRows := range []int{6, 7, 8, 12} {
		t.Run(fmt.Sprint(maxRows), func(t *testing.T) {
			p := newApprovalDialog(&a, &km, mocha())
			d := p.d
			d.move(2)
			d.paste("one\ntwo\nthree\nfour\nfive")
			rows := plainRows(p.dialog(40, mocha()).rows(40, maxRows, 3, mocha()))
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

	p := newApprovalDialog(&a, &km, mocha())
	d := p.d
	d.move(2)
	d.paste("one\ntwo")
	assert.Equal(t, []string{rule40, "❭ 3. No (esc)", rule40}, plainRows(p.dialog(40, mocha()).rows(40, 3, 3, mocha())), "too few rows for the options keeps the selected row between the rules")

	p = newApprovalDialog(&a, &km, mocha())
	d = p.d
	d.move(2)
	d.paste("one\ntwo\nthree\nfour\nfive")
	assert.Equal(t, []string{rule40, "❭ 3. No (esc)", "  five", "  3 of 3", rule40}, plainRows(p.dialog(40, mocha()).rows(40, 5, 3, mocha())), "the selected field keeps one row")
}

func TestApprovalRowsDropAnUnselectedFieldBeforeAnOption(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit a.go", Preview: []tool.PreviewLine{{Kind: tool.Added, Text: "x"}}, Edits: true}
	km := newKeyMap(config.DefaultKeys())
	for _, tt := range []struct{ width, maxPreview, maxRows int }{{60, 2, 4}, {30, 3, 5}} {
		t.Run(fmt.Sprint(tt.width), func(t *testing.T) {
			rule := strings.Repeat("─", tt.width)
			p := newApprovalDialog(&a, &km, mocha())
			d := p.d
			d.move(2)
			d.paste("one\ntwo\nthree")
			d.move(0)
			rows := plainRows(p.dialog(tt.width, mocha()).rows(tt.width, tt.maxRows, tt.maxPreview, mocha()))
			assert.LessOrEqual(t, len(rows), tt.maxRows)
			assert.Equal(t, rule, rows[0])
			assert.Contains(t, rows, "❭ 1. Yes", "%q", rows)
			for _, line := range []string{"one", "two", "three"} {
				assert.NotContains(t, rows, "  "+line)
			}

			d.move(2)
			rows = plainRows(p.dialog(tt.width, mocha()).rows(tt.width, tt.maxRows, tt.maxPreview, mocha()))
			at := slices.Index(rows, "❭ 3. No (esc)")
			require.GreaterOrEqual(t, at, 0, "%q", rows)
			assert.Equal(t, "  three", rows[at+1], "the selected field keeps one row")
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

	w := event.ApprovalRequested{Title: "Bash npm test", Agent: "workflow #2 auth", TaskID: "t3"}
	assert.Equal(t, "  workflow #2 auth t3 › Bash npm test", plainApprovalRows(w, 0, 10)[1])
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
