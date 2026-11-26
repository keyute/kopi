package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func btwOf(m *model) *btwDialog { return promptOf[*btwDialog](m) }

func asked(m *model, id int, q string) { send(m, event.SideQuestionAsked{ID: id, Question: q}) }

func TestBtwOpensOnAskedAndAnswersInPlace(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	rule := strings.Repeat("─", 72)

	asked(m, 1, "what was the name of that config file again?")

	assert.Equal(t, []string{
		rule,
		"  Side question",
		"",
		"  what was the name of that config file again?",
		"",
		"  Answering…",
		"",
		"  esc to cancel",
		rule,
	}, dialogRows(t, m))

	send(m, event.SideQuestionEnded{ID: 1, Answer: "`internal/config/ui.go`. The key map is in `Defaults()`."})

	assert.Equal(t, []string{
		rule,
		"  Side question",
		"",
		"  what was the name of that config file again?",
		"",
		"  internal/config/ui.go. The key map is in Defaults().",
		"",
		"  esc to close",
		rule,
	}, dialogRows(t, m))
}

func TestBtwShowsAnError(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	asked(m, 1, "why?")

	send(m, event.SideQuestionEnded{ID: 1, Err: errors.New("failed to answer: boom")})

	rows := dialogRows(t, m)
	assert.Contains(t, rows, "  failed to answer: boom")
	assert.Contains(t, rows, "  esc to close")
	assert.NotContains(t, strings.Join(rows, "\n"), "Answering")
}

func TestBtwIgnoresAnotherIDsEnd(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	asked(m, 2, "why?")
	before := dialogRows(t, m)

	send(m, event.SideQuestionEnded{ID: 1, Answer: "old"})

	assert.Equal(t, before, dialogRows(t, m))
	assert.False(t, btwOf(m).ended)
}

func TestBtwDismissWhileAnsweringStopsTheRequest(t *testing.T) {
	for name, k := range map[string]tea.KeyPressMsg{"esc": escape, "ctrl+c": ctrlC} {
		t.Run(name, func(t *testing.T) {
			m, c := newTestModel(t, 72, 30)
			asked(m, 7, "why?")

			press(m, k)

			assert.Equal(t, []int{7}, c.sideStops)
			assert.Nil(t, btwOf(m))
		})
	}
}

func TestBtwDismissAfterTheAnswerOnlyCloses(t *testing.T) {
	m, c := newTestModel(t, 72, 30)
	asked(m, 7, "why?")
	send(m, event.SideQuestionEnded{ID: 7, Answer: "because"})

	press(m, escape)

	assert.Empty(t, c.sideStops)
	assert.Nil(t, btwOf(m))
}

func TestBtwSwallowsOtherKeys(t *testing.T) {
	m, c := newTestModel(t, 72, 30)
	asked(m, 1, "why?")

	typeText(m, "x")
	press(m, enter)

	assert.NotNil(t, btwOf(m))
	assert.Empty(t, c.sideStops)
	assert.Empty(t, c.submitted)
}

func TestBtwClosesWithASessionChange(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	asked(m, 1, "why?")

	send(m, event.SessionChanged{})

	assert.Nil(t, btwOf(m))
}

func TestBtwNewerQuestionReplacesTheOlder(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	asked(m, 1, "first?")

	asked(m, 2, "second?")

	assert.Len(t, m.prompts, 1)
	assert.Equal(t, 2, btwOf(m).id)
}

func TestBtwYieldsToAnApproval(t *testing.T) {
	m, _ := newTestModel(t, 72, 30)
	asked(m, 1, "why?")

	send(m, event.ApprovalRequested{Token: 1, CallID: "c1", Title: "ls"})

	require.NotNil(t, approvalOf(m))
	assert.Equal(t, promptApproval, m.front().kind)
	assert.NotNil(t, btwOf(m), "covered, not closed")
	assert.False(t, promptBtw.waits())
	assert.Empty(t, promptBtw.programKind())

	send(m, event.ApprovalEnded{Token: 1})

	assert.Equal(t, promptBtw, m.front().kind)
}

func TestBtwScrollsALongAnswer(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	asked(m, 1, "list them")
	var lines []string
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("- item %d", i))
	}
	send(m, event.SideQuestionEnded{ID: 1, Answer: strings.Join(lines, "\n")})
	require.True(t, btwOf(m).d.scroll.cut())
	assert.Contains(t, strings.Join(dialogRows(t, m), "\n"), "to scroll")

	before := strings.Join(dialogRows(t, m), "\n")
	press(m, pgDown)

	assert.NotEqual(t, before, strings.Join(dialogRows(t, m), "\n"))
}

func TestBtwKeepsItsRowsAtTheSameWidth(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	asked(m, 1, "why?")
	send(m, event.SideQuestionEnded{ID: 1, Answer: "because"})
	b := btwOf(m)
	m.frontDialog()
	first := b.body

	m.frontDialog()

	assert.Same(t, &first[0], &b.body[0])
}

func TestStatusRefreshesOnASideAnswer(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	asked(m, 1, "why?")

	c.status.TotalCost = 1.5
	send(m, event.SideQuestionEnded{ID: 1, Answer: "because"})

	assert.InDelta(t, 1.5, m.status.TotalCost, 0)
}
