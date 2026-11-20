package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// promptCase is one prompt kind: the events that open and end it, the text its
// dialog draws, and how many answers the controller got for it.
type promptCase struct {
	kind    promptKind
	open    event.Event
	end     event.Event
	title   string
	answers func(*fakeController) int
}

var (
	approvalCase = promptCase{
		kind: promptApproval, open: event.ApprovalRequested{Token: 1, Title: "Edit a.go"}, end: event.ApprovalEnded{Token: 1},
		title: "Edit a.go", answers: func(c *fakeController) int { return len(c.answers) },
	}
	questionCase = promptCase{
		kind: promptQuestion, open: event.QuestionRequested{Token: 2, Questions: []tool.Question{choice("one", false, "A", "B")}},
		end: event.QuestionEnded{Token: 2}, title: "Which one?", answers: func(c *fakeController) int { return len(c.questionAnswers) },
	}
	planCase = promptCase{
		kind: promptPlan, open: planRequest(3, "# Heading"), end: event.PlanEnded{Token: 3},
		title: planPath, answers: func(c *fakeController) int { return len(c.planAnswers) },
	}
	loginCase = promptCase{
		kind: promptLogin, open: event.LoginRequested{Token: 4, Message: "Login code"}, end: event.LoginEnded{Token: 4},
		title: "Login code", answers: func(c *fakeController) int { return len(c.loginAnswers) },
	}
)

func TestFrontIsTheOpenPromptOfTheHighestKind(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	assert.Equal(t, promptNone, m.front())
	assert.Nil(t, m.frontDialog())

	for _, pc := range []promptCase{approvalCase, questionCase, planCase, loginCase} {
		m, _ := newTestModel(t, 70, 30)
		send(m, pc.open)
		assert.Equal(t, pc.kind, m.front())
		assert.NotNil(t, m.frontDialog())
	}
}

func TestFrontIsTasksWhileOnlyTheTasksDialogIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")
		require.NotNil(t, m.tasks)
		assert.Equal(t, promptTasks, m.front())
		assert.Nil(t, m.frontDialog())
		press(m, escape)
	})
}

func TestTheHigherPromptIsInFrontWhateverTheArrivalOrder(t *testing.T) {
	for _, pair := range []struct{ high, low promptCase }{
		{approvalCase, loginCase},
		{questionCase, planCase},
		{questionCase, loginCase},
		{planCase, loginCase},
	} {
		for _, highFirst := range []bool{true, false} {
			m, c := newTestModel(t, 70, 30)
			if highFirst {
				send(m, pair.high.open, pair.low.open)
			} else {
				send(m, pair.low.open, pair.high.open)
			}
			text := strings.Join(screen(m), "\n")
			assert.Equal(t, pair.high.kind, m.front())
			assert.Contains(t, text, pair.high.title)
			assert.NotContains(t, text, pair.low.title)

			press(m, enter)
			assert.Equal(t, 1, pair.high.answers(c))
			assert.Zero(t, pair.low.answers(c))

			send(m, pair.high.end)
			text = strings.Join(screen(m), "\n")
			assert.Equal(t, pair.low.kind, m.front())
			assert.Contains(t, text, pair.low.title)
			assert.NotContains(t, text, pair.high.title)

			press(m, enter)
			assert.Equal(t, 1, pair.low.answers(c))
			assert.Equal(t, 1, pair.high.answers(c))
		}
	}
}

func TestTheFrontPromptStillAnswersAfterTheOneUnderItEnds(t *testing.T) {
	for _, tc := range []struct {
		front, under promptCase
		underFirst   []bool
	}{
		{questionCase, planCase, []bool{true}},
		{questionCase, loginCase, []bool{true}},
		{approvalCase, questionCase, []bool{true, false}},
		{approvalCase, planCase, []bool{true, false}},
		{approvalCase, loginCase, []bool{true, false}},
		{planCase, loginCase, []bool{true}},
	} {
		for _, underFirst := range tc.underFirst {
			m, c := newTestModel(t, 70, 30)
			if underFirst {
				send(m, tc.under.open, tc.front.open)
			} else {
				send(m, tc.front.open, tc.under.open)
			}
			send(m, tc.under.end)

			assert.Equal(t, tc.front.kind, m.front())
			assert.Contains(t, strings.Join(screen(m), "\n"), tc.front.title)
			press(m, enter)
			assert.Equal(t, 1, tc.front.answers(c))
			assert.Zero(t, tc.under.answers(c))
			if req, ok := tc.front.open.(event.ApprovalRequested); ok {
				first := approvalOptions(&req, &m.keys)[0].answer
				assert.Equal(t, []answer{{req.Token, policy.Decision{Answer: first}}}, c.answers)
			}
		}
	}
}

func TestMainTurnEndLeavesAChildApprovalAnswerableUnderAQuestion(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, questionCase.open, subagentAsk(5, "explore"))
	require.NotNil(t, m.question)
	require.Equal(t, promptApproval, m.front())

	send(m, event.TurnEnd{})

	assert.Nil(t, m.question)
	assert.Equal(t, promptApproval, m.front())
	assert.Contains(t, strings.Join(screen(m), "\n"), "Edit explore.go")
	press(m, enter)
	assert.Equal(t, []answer{{5, policy.Decision{Answer: policy.Allow}}}, c.answers)
	assert.Zero(t, questionCase.answers(c))
}

func TestApprovalFieldTakesInputWhileALoginIsUnderIt(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: "clip"}
	send(m, editApproval(), loginCase.open)
	require.Equal(t, promptApproval, m.front())

	press(m, digit('3'))
	pasteText(m, "pasted")
	assert.Equal(t, "pasted", m.approvalDialog.text())
	assert.Empty(t, m.loginDialog.text())

	m.Update(clipboardTextMsg{text: "y", field: m.loginDialog.field()})
	assert.Equal(t, "pasted", m.approvalDialog.text())
	assert.Empty(t, m.loginDialog.text())
}

func TestClickOnApprovalFieldTypesIntoItWhileALoginIsUnderIt(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval(), loginCase.open)
	require.Len(t, m.geo.fields, 1)
	a := m.geo.fields[0]

	pressAt(m, a.x, a.y)
	releaseAt(m, a.x, a.y)
	keys(m, "hi")

	assert.Equal(t, 2, m.approvalDialog.sel)
	assert.Equal(t, "hi", m.approvalDialog.text())
	assert.Empty(t, m.loginDialog.text())
}

func TestSessionChangeLeavesALoginUnderAQuestionAnswerable(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, loginCase.open, questionCase.open)
	send(m, event.SessionChanged{})

	assert.Equal(t, promptLogin, m.front())
	assert.Contains(t, strings.Join(screen(m), "\n"), loginCase.title)
	press(m, enter)
	assert.Equal(t, 1, loginCase.answers(c))
	assert.Zero(t, questionCase.answers(c))
}

func TestPlanAndLoginRequestsCloseTheTasksDialog(t *testing.T) {
	for _, pc := range []promptCase{planCase, loginCase} {
		synctest.Test(t, func(t *testing.T) {
			m, _ := tasksModel(t)
			submitText(m, "/tasks")
			require.NotNil(t, m.tasks)

			send(m, pc.open)

			assert.Nil(t, m.tasks)
			assert.Equal(t, pc.kind, m.front())
		})
	}
}

func longApproval(n int) event.ApprovalRequested {
	ev := editApproval()
	for i := range n {
		ev.Preview = append(ev.Preview, tool.PreviewLine{Kind: tool.Added, Text: fmt.Sprintf("line %d", i)})
	}
	return ev
}

var pgUp = tea.KeyPressMsg{Code: tea.KeyPgUp}

func TestPageKeysScrollACutApprovalNotTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, longApproval(50))
	top := m.top()
	require.True(t, m.approvalDialog.scroll.cut())

	press(m, pgDown)
	assert.Contains(t, strings.Join(screen(m), "\n"), "earlier lines")
	assert.Equal(t, top, m.top())

	press(m, pgUp)
	assert.NotContains(t, strings.Join(screen(m), "\n"), "earlier lines")
	assert.Equal(t, top, m.top())
}

func TestPageKeyClearsAQuestionNotice(t *testing.T) {
	m, _ := newTestModel(t, 90, 20)
	send(m, event.QuestionRequested{Token: 7, Questions: []tool.Question{choice("Pick", false, "a", "b")}})
	m.question.notice = unansweredNote

	press(m, pgDown)

	assert.Empty(t, m.question.notice)
}

func TestPageKeysScrollACutApprovalFromTheNoRowAndKeepTheDraft(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	send(m, longApproval(50))
	m.approvalDialog.move(2)
	m.approvalDialog.paste("one\ntwo\nthree")

	press(m, pgDown)

	assert.Equal(t, "one\ntwo\nthree", m.approvalDialog.text())
	assert.Positive(t, m.approvalDialog.scroll.off)
	assert.Empty(t, c.answers)
}

func TestPageKeysScrollTheTranscriptWhenTheApprovalPreviewFits(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, editApproval())
	require.False(t, m.approvalDialog.scroll.cut())
	require.True(t, m.view().follow)

	press(m, pgUp)
	assert.False(t, m.view().follow)
	assert.Zero(t, m.approvalDialog.scroll.off)
	press(m, pgDown)
	assert.True(t, m.view().follow)
}

func TestTheNextApprovalStartsAtTheTopOfItsPreview(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, longApproval(50))
	press(m, pgDown)
	require.Positive(t, m.approvalDialog.scroll.off)

	ev := longApproval(50)
	ev.Token = 2
	send(m, ev)
	send(m, event.ApprovalEnded{Token: 1})
	assert.Zero(t, m.approvalDialog.scroll.off)
}

func TestPageKeysScrollTheTranscriptFromALoginPrompt(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, event.LoginRequested{Token: 1, Message: "Code"})
	typeKeys(m, "abc")
	require.True(t, m.view().follow)

	press(m, pgUp)

	assert.False(t, m.view().follow)
	assert.Equal(t, "abc", m.loginDialog.text())
}
