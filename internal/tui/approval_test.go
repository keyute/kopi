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
		"Edit a.go",
		"  keep",
		"- old",
		"+ new",
		"",
		"  1. Yes",
		"❯ 2. Yes, allow all edits this session",
		"  3. No (esc)",
		"",
	}, plainApprovalRows(a, 1, 10))
}

func TestApprovalRowsWithoutEditsOfferNoAsTwo(t *testing.T) {
	a := event.ApprovalRequested{CallID: "c1", Title: "Write a.go"}
	assert.Equal(t, []string{"Write a.go", "", "❯ 1. Yes", "  2. No (esc)", ""}, plainApprovalRows(a, 0, 10))
}

func TestApprovalRowsCapThePreview(t *testing.T) {
	a := event.ApprovalRequested{Title: "Write a.go"}
	for range 10 {
		a.Preview = append(a.Preview, tool.PreviewLine{Kind: tool.Added, Text: "x"})
	}
	rows := plainApprovalRows(a, 0, 4)
	assert.Equal(t, []string{"Write a.go", "+ x", "+ x", "+ x", "  … 7 more lines", ""}, rows[:6])

	a.Preview = a.Preview[:4]
	assert.Equal(t, []string{"Write a.go", "+ x", "+ x", "+ x", "+ x", ""}, plainApprovalRows(a, 0, 4)[:6], "an exact fit has no marker")
}

func TestApprovalRowsNameTheRuleOnlyWhenSet(t *testing.T) {
	a := event.ApprovalRequested{Title: "Edit .git/config", Rule: "Edit(.git) (built-in)"}
	assert.Equal(t, "asked by Edit(.git) (built-in)", plainApprovalRows(a, 0, 10)[1])

	a.Rule = ""
	assert.NotContains(t, strings.Join(plainApprovalRows(a, 0, 10), "\n"), "asked by")
}
