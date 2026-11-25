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
	dirCase = promptCase{
		kind: promptDir, open: event.DirRequested{Token: 5, Dir: "/w/other"}, end: event.DirEnded{Token: 5},
		title: "Add this folder to the workspace?", answers: func(c *fakeController) int { return len(c.dirAnswers) },
	}
	loginCase = promptCase{
		kind: promptLogin, open: event.LoginRequested{Token: 4, Message: "Login code"}, end: event.LoginEnded{Token: 4},
		title: "Login code", answers: func(c *fakeController) int { return len(c.loginAnswers) },
	}
)

// promptOf is the first open prompt of type P, nil when none is open.
func promptOf[P prompt](m *model) P {
	for _, h := range m.prompts {
		if p, ok := h.prompt.(P); ok {
			return p
		}
	}
	var none P
	return none
}

func approvalOf(m *model) *approvalDialog { return promptOf[*approvalDialog](m) }
func questionOf(m *model) *questionDialog { return promptOf[*questionDialog](m) }
func planOf(m *model) *planDialog         { return promptOf[*planDialog](m) }
func loginOf(m *model) *loginDialog       { return promptOf[*loginDialog](m) }
func dirOf(m *model) *dirDialog           { return promptOf[*dirDialog](m) }
func tasksOf(m *model) *taskDialog        { return promptOf[*taskDialog](m) }
func usageOf(m *model) *usageDialog       { return promptOf[*usageDialog](m) }

// queuedOf is the approvals behind the shown one.
func queuedOf(m *model) []event.ApprovalRequested {
	var out []event.ApprovalRequested
	for _, h := range m.prompts {
		if a, ok := h.prompt.(*approvalDialog); ok {
			out = append(out, *a.req)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out[1:]
}

// frontKind is the kind of the front prompt; it requires one to be open.
func frontKind(m *model) promptKind {
	return m.front().kind
}

func TestFrontIsTheOpenPromptOfTheHighestKind(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	assert.Nil(t, m.front())
	assert.Nil(t, m.frontDialog())

	for _, pc := range []promptCase{approvalCase, questionCase, planCase, loginCase, dirCase} {
		m, _ := newTestModel(t, 70, 30)
		send(m, pc.open)
		assert.Equal(t, pc.kind, frontKind(m))
		assert.NotNil(t, m.frontDialog())
	}
}

func TestFrontIsTasksWhileOnlyTheTasksDialogIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")
		require.NotNil(t, tasksOf(m))
		assert.Equal(t, promptTasks, frontKind(m))
		assert.Same(t, tasksOf(m).d, m.frontDialog())
		press(m, escape)
	})
}

func TestOpenOrdersByKindThenArrival(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	send(m,
		event.LoginRequested{Token: 1, Message: "Login code"},
		event.ApprovalRequested{Token: 2, Title: "Edit a.go"},
		event.PlanRequested{Token: 3},
		event.ApprovalRequested{Token: 4, Title: "Edit b.go"},
		event.ApprovalRequested{Token: 5, Title: "Edit c.go"},
		event.DirRequested{Token: 7, Dir: "/w/other"},
	)
	m.open(promptTasks, &taskDialog{d: &dialog{}, km: &m.keys})
	m.open(promptQuestion, newQuestionDialog(event.QuestionRequested{Token: 6, Questions: []tool.Question{choice("one", false, "A")}}, &m.keys, m.th))

	var kinds []promptKind
	for _, h := range m.prompts {
		kinds = append(kinds, h.kind)
	}
	assert.Equal(t, []promptKind{promptApproval, promptApproval, promptApproval, promptQuestion, promptPlan, promptLogin, promptDir, promptTasks}, kinds)
	var tokens []uint64
	for _, h := range m.prompts {
		if a, ok := h.prompt.(*approvalDialog); ok {
			tokens = append(tokens, a.req.Token)
		}
	}
	assert.Equal(t, []uint64{2, 4, 5}, tokens)
}

func TestACoveredPromptStaysOpenAndReturns(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, loginCase.open, approvalCase.open)
	require.Len(t, m.prompts, 2)
	assert.NotNil(t, loginOf(m), "the login stays open under the approval")

	send(m, approvalCase.end)

	assert.Equal(t, promptLogin, frontKind(m))
	press(m, enter)
	assert.Equal(t, 1, loginCase.answers(c))
}

func TestTheHigherPromptIsInFrontWhateverTheArrivalOrder(t *testing.T) {
	for _, pair := range []struct{ high, low promptCase }{
		{approvalCase, loginCase},
		{questionCase, planCase},
		{questionCase, loginCase},
		{planCase, loginCase},
		{approvalCase, dirCase},
		{planCase, dirCase},
		{loginCase, dirCase},
	} {
		for _, highFirst := range []bool{true, false} {
			m, c := newTestModel(t, 70, 30)
			if highFirst {
				send(m, pair.high.open, pair.low.open)
			} else {
				send(m, pair.low.open, pair.high.open)
			}
			text := strings.Join(screen(m), "\n")
			assert.Equal(t, pair.high.kind, frontKind(m))
			assert.Contains(t, text, pair.high.title)
			assert.NotContains(t, text, pair.low.title)

			press(m, enter)
			assert.Equal(t, 1, pair.high.answers(c))
			assert.Zero(t, pair.low.answers(c))

			send(m, pair.high.end)
			text = strings.Join(screen(m), "\n")
			assert.Equal(t, pair.low.kind, frontKind(m))
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

			assert.Equal(t, tc.front.kind, frontKind(m))
			assert.Contains(t, strings.Join(screen(m), "\n"), tc.front.title)
			press(m, enter)
			assert.Equal(t, 1, tc.front.answers(c))
			assert.Zero(t, tc.under.answers(c))
			if req, ok := tc.front.open.(event.ApprovalRequested); ok {
				first := approvalOptions(&req)[0].answer
				assert.Equal(t, []answer{{req.Token, policy.Decision{Answer: first}}}, c.answers)
			}
		}
	}
}

func TestMainTurnEndLeavesAChildApprovalAnswerableUnderAQuestion(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, questionCase.open, subagentAsk(5, "explore"))
	require.NotNil(t, questionOf(m))
	require.Equal(t, promptApproval, frontKind(m))

	send(m, questionCase.end, event.TurnEnd{})

	assert.Nil(t, questionOf(m))
	assert.Equal(t, promptApproval, frontKind(m))
	assert.Contains(t, strings.Join(screen(m), "\n"), "Edit explore.go")
	press(m, enter)
	assert.Equal(t, []answer{{5, policy.Decision{Answer: policy.Allow}}}, c.answers)
	assert.Zero(t, questionCase.answers(c))
}

func TestApprovalFieldTakesInputWhileALoginIsUnderIt(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: "clip"}
	send(m, editApproval(), loginCase.open)
	require.Equal(t, promptApproval, frontKind(m))

	press(m, digit('3'))
	pasteText(m, "pasted")
	assert.Equal(t, "pasted", approvalOf(m).d.text())
	assert.Empty(t, loginOf(m).d.text())

	m.Update(clipboardTextMsg{text: "y", field: loginOf(m).d.field()})
	assert.Equal(t, "pasted", approvalOf(m).d.text())
	assert.Empty(t, loginOf(m).d.text())
}

func TestClickOnApprovalFieldTypesIntoItWhileALoginIsUnderIt(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval(), loginCase.open)
	require.Len(t, m.geo.fields, 1)
	a := m.geo.fields[0]

	pressAt(m, a.x, a.y)
	releaseAt(m, a.x, a.y)
	keys(m, "hi")

	assert.Equal(t, 2, approvalOf(m).d.sel)
	assert.Equal(t, "hi", approvalOf(m).d.text())
	assert.Empty(t, loginOf(m).d.text())
}

func TestSessionChangeLeavesALoginUnderAQuestionAnswerable(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, loginCase.open, questionCase.open)
	send(m, questionCase.end, event.SessionChanged{})

	assert.Equal(t, promptLogin, frontKind(m))
	assert.Contains(t, strings.Join(screen(m), "\n"), loginCase.title)
	press(m, enter)
	assert.Equal(t, 1, loginCase.answers(c))
	assert.Zero(t, questionCase.answers(c))
}

func TestPlanAndLoginRequestsCoverTheTasksDialog(t *testing.T) {
	for _, pc := range []promptCase{planCase, loginCase} {
		synctest.Test(t, func(t *testing.T) {
			m, _ := tasksModel(t)
			submitText(m, "/tasks")
			require.NotNil(t, tasksOf(m))

			send(m, pc.open)

			assert.NotNil(t, tasksOf(m))
			assert.Equal(t, pc.kind, frontKind(m))

			send(m, pc.end)

			assert.Equal(t, promptTasks, frontKind(m))
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
	require.True(t, approvalOf(m).d.scroll.cut())

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
	questionOf(m).current().warn = unansweredNote

	press(m, pgDown)

	assert.Empty(t, questionOf(m).current().warn)
}

func TestPageKeysScrollACutApprovalFromTheNoRowAndKeepTheDraft(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	send(m, longApproval(50))
	approvalOf(m).d.move(2)
	approvalOf(m).d.paste("one\ntwo\nthree")

	press(m, pgDown)

	assert.Equal(t, "one\ntwo\nthree", approvalOf(m).d.text())
	assert.Positive(t, approvalOf(m).d.scroll.off)
	assert.Empty(t, c.answers)
}

func TestPageKeysScrollTheTranscriptWhenTheApprovalPreviewFits(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, editApproval())
	require.False(t, approvalOf(m).d.scroll.cut())
	require.True(t, m.view().follow)

	press(m, pgUp)
	assert.False(t, m.view().follow)
	assert.Zero(t, approvalOf(m).d.scroll.off)
	press(m, pgDown)
	assert.True(t, m.view().follow)
}

func TestTheNextApprovalStartsAtTheTopOfItsPreview(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	send(m, longApproval(50))
	press(m, pgDown)
	require.Positive(t, approvalOf(m).d.scroll.off)

	ev := longApproval(50)
	ev.Token = 2
	send(m, ev)
	send(m, event.ApprovalEnded{Token: 1})
	assert.Zero(t, approvalOf(m).d.scroll.off)
}

func TestPageKeysScrollTheTranscriptFromALoginPrompt(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, event.LoginRequested{Token: 1, Message: "Code"})
	typeKeys(m, "abc")
	require.True(t, m.view().follow)

	press(m, pgUp)

	assert.False(t, m.view().follow)
	assert.Equal(t, "abc", loginOf(m).d.text())
}

func TestCancelDismissesTheFrontPromptAsEscDoes(t *testing.T) {
	for _, pc := range []promptCase{approvalCase, questionCase, planCase} {
		m, c := newTestModel(t, 70, 30)
		c.running = true
		send(m, event.TurnStart{}, pc.open)

		press(m, ctrlC)

		assert.Equal(t, 1, pc.answers(c))
		assert.Zero(t, c.interrupts)
	}
}

func TestCancelInATaskOutputReturnsToTheList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")
		press(m, enter)
		require.Equal(t, "t1", tasksOf(m).open)

		press(m, ctrlC)

		require.NotNil(t, tasksOf(m))
		assert.Empty(t, tasksOf(m).open)
		assert.Equal(t, promptTasks, m.front().kind)
	})
}

func TestCancelCancelsALoginAndLeavesTheTasksList(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, loginCase.open)
	press(m, ctrlC)
	assert.Equal(t, 1, c.interrupts, "as esc, it ends the login")

	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")
		press(m, ctrlC)
		assert.Nil(t, m.front())
	})
}

func TestClipboardTextIsDroppedWhileAPromptIsInFront(t *testing.T) {
	for _, pc := range []promptCase{approvalCase, questionCase, planCase, loginCase, dirCase} {
		m, _ := newTestModel(t, 70, 30)
		send(m, pc.open)

		m.Update(clipboardTextMsg{text: "stray"})

		assert.Empty(t, m.composer.value())
	}

	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")

		m.Update(clipboardTextMsg{text: "stray"})

		assert.Empty(t, m.composer.value())
		assert.NotNil(t, tasksOf(m))
	})
}

func TestPastedImageGoesToTheDraftWhileAPromptIsInFront(t *testing.T) {
	for _, pc := range []promptCase{approvalCase, questionCase, planCase, loginCase, dirCase} {
		m, _ := newTestModel(t, 70, 30)
		send(m, pc.open)

		m.Update(imagePastedMsg{path: writeImage(t, t.TempDir(), "a.png")})

		_, images := m.composer.expanded()
		assert.Len(t, images, 1)
		require.NotNil(t, m.front())
		assert.Equal(t, pc.kind, m.front().kind)
	}
}

func TestPageKeysScrollTheTranscriptFromAnUncutQuestion(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, questionCase.open)
	require.False(t, questionOf(m).current().scroll.cut())
	require.True(t, m.view().follow)

	press(m, pgUp)

	assert.False(t, m.view().follow)
}

func TestPageKeysScrollTheTranscriptFromAQuestionWhoseSelectedOptionHasNoPreview(t *testing.T) {
	m, _ := newTestModel(t, 80, 60)
	notices(m, 100)
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	send(m, event.QuestionRequested{Token: 2, Questions: previewQuestion(strings.Join(lines, "\n\n"))})
	require.True(t, questionOf(m).current().scroll.cut())
	press(m, down)
	press(m, down)
	require.False(t, questionOf(m).current().scroll.cut())
	require.True(t, m.view().follow)

	press(m, pgUp)

	assert.False(t, m.view().follow)
}

func TestAPromptStaysThroughTurnEndAndSessionChangeUntilItsEnd(t *testing.T) {
	for _, pc := range []promptCase{approvalCase, questionCase, planCase, loginCase, dirCase} {
		m, _ := newTestModel(t, 70, 30)
		send(m, pc.open)

		send(m, event.TurnEnd{}, event.SessionChanged{})
		assert.Equal(t, pc.kind, frontKind(m))

		send(m, pc.end)
		assert.Nil(t, m.front())
	}
}

func TestWorkflowIsRankedBetweenLoginAndTasks(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	m.open(promptTasks, &taskDialog{d: &dialog{}, km: &m.keys})
	m.open(promptWorkflow, &workflowDialog{m: m, d: &dialog{}})
	send(m, event.LoginRequested{Token: 1, Message: "Login code"})

	var kinds []promptKind
	for _, h := range m.prompts {
		kinds = append(kinds, h.kind)
	}
	assert.Equal(t, []promptKind{promptLogin, promptWorkflow, promptTasks}, kinds)
	assert.False(t, promptWorkflow.waits(), "it does not wait on the human")
}
