package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
	"github.com/keyute/kopi/internal/tool"
)

type fakeController struct {
	// calls logs Submit, SendNow and Interrupt in the order they were made.
	calls []string
	// sessions logs the session argument of every Submit, SendNow, TakeQueued
	// and Interrupt, in call order.
	sessions   []string
	agents     []app.AgentStatus
	status     app.Status
	running    bool
	interrupts int
	submitted  []string
	images     [][]string
	submitErr  error
	sentNow    []string
	nowImages  [][]string
	sendNowErr error
	// queued is what TakeQueued hands out once; takes counts the calls.
	queued     []event.Input
	takes      int
	toggles    int
	history    []string
	histories  int
	subscriber func(event.Event)
	answers    []answer
	// loginAnswers logs AnswerLogin.
	loginAnswers []loginAnswer
	// questionAnswers logs AnswerQuestion.
	questionAnswers []questionAnswer
	// planAnswers logs AnswerPlan, refreshes RefreshPlan; planFile is PlanFile.
	planAnswers []planAnswer
	refreshes   []uint64
	planFile    string
	commands    []app.Command
	// skills is what Spans highlights as skill commands; nil means none.
	skills   *skill.Set
	args     map[string][]app.Candidate
	files    []app.Candidate
	filesErr error
	// foreground is what Background returns; backgrounds counts the calls.
	foreground  bool
	backgrounds int
	// tasks is what Tasks returns; TaskOutput adds the output in outputs.
	tasks   []tool.TaskInfo
	outputs map[string]string
	// stops logs StopTask, which marks the task stopped.
	stops []string
}

type answer struct {
	token uint64
	d     policy.Decision
}

type loginAnswer struct {
	token  uint64
	answer string
}

func (f *fakeController) Answer(token uint64, d policy.Decision) bool {
	f.answers = append(f.answers, answer{token, d})
	return true
}

func (f *fakeController) AnswerLogin(token uint64, answer string) bool {
	f.loginAnswers = append(f.loginAnswers, loginAnswer{token, answer})
	return true
}

type questionAnswer struct {
	token   uint64
	answers []tool.Answer
}

func (f *fakeController) AnswerQuestion(token uint64, answers []tool.Answer) bool {
	f.questionAnswers = append(f.questionAnswers, questionAnswer{token, answers})
	return true
}

type planAnswer struct {
	token  uint64
	answer tool.PlanAnswer
}

func (f *fakeController) AnswerPlan(token uint64, a tool.PlanAnswer) bool {
	f.planAnswers = append(f.planAnswers, planAnswer{token, a})
	return true
}

func (f *fakeController) RefreshPlan(token uint64) bool {
	f.refreshes = append(f.refreshes, token)
	return true
}

func (f *fakeController) PlanFile() string { return f.planFile }

func (f *fakeController) Subscribe(fn func(event.Event)) func() {
	f.subscriber = fn
	return func() { f.subscriber = nil }
}

func (f *fakeController) Submit(_ context.Context, session, text string, images ...string) (*app.Work, error) {
	f.sessions = append(f.sessions, session)
	f.calls = append(f.calls, "submit "+text)
	f.submitted = append(f.submitted, text)
	f.images = append(f.images, images)
	return nil, f.submitErr
}

func (f *fakeController) SendNow(_ context.Context, session, text string, images ...string) error {
	f.sessions = append(f.sessions, session)
	f.calls = append(f.calls, "send-now "+text)
	f.sentNow = append(f.sentNow, text)
	f.nowImages = append(f.nowImages, images)
	return f.sendNowErr
}

func (f *fakeController) TakeQueued(session string) []event.Input {
	f.sessions = append(f.sessions, session)
	f.takes++
	q := f.queued
	f.queued = nil
	return q
}

func (f *fakeController) ToggleMode() { f.toggles++ }

func (f *fakeController) History() []string {
	f.histories++
	return f.history
}

func (f *fakeController) Interrupt(session string) bool {
	f.sessions = append(f.sessions, session)
	f.calls = append(f.calls, "interrupt")
	f.interrupts++
	return f.running
}

func (f *fakeController) Status() app.Status { return f.status }

func (f *fakeController) Agents() []app.AgentStatus { return f.agents }

func (f *fakeController) Commands() []app.Command { return f.commands }

func (f *fakeController) Spans(text string) []app.Span { return app.Spans(text, f.skills) }

func (f *fakeController) Arguments(name string) ([]app.Candidate, error) { return f.args[name], nil }

func (f *fakeController) Files() ([]app.Candidate, error) { return f.files, f.filesErr }

func (f *fakeController) Background() bool {
	f.backgrounds++
	return f.foreground
}

func (f *fakeController) Tasks() []tool.TaskInfo { return slices.Clone(f.tasks) }

func (f *fakeController) TaskOutput(id string) (tool.TaskInfo, error) {
	for _, info := range f.tasks {
		if info.ID == id {
			info.Output = f.outputs[id]
			return info, nil
		}
	}
	return tool.TaskInfo{}, tool.ErrUnknownTask
}

func (f *fakeController) StopTask(id string) error {
	f.stops = append(f.stops, id)
	for i := range f.tasks {
		if f.tasks[i].ID == id && f.tasks[i].Status == tool.TaskRunning {
			f.tasks[i].Status, f.tasks[i].Ended = tool.TaskStopped, time.Now()
		}
	}
	return nil
}

// testOptions are the options config's defaults resolve to.
func testOptions() Options {
	return Options{Version: "v1.2.3", Keys: config.DefaultKeys(), StatusLine: config.DefaultStatusLine(), AgentLine: config.DefaultAgentLine(), Themes: testThemes}
}

var testThemes = Themes{dark: mocha(), light: latte()}

func newTestModel(t *testing.T, width, height int) (*model, *fakeController) {
	t.Helper()
	return newTestModelWith(t, width, height, testOptions())
}

func newTestModelWith(t *testing.T, width, height int, opts Options) (*model, *fakeController) {
	t.Helper()
	c := &fakeController{status: app.Status{Model: "mock/model-1", Mode: policy.Plan}}
	m := newModel(t.Context(), c, newBridge(), opts)
	m.clip = &fakeClipboard{}
	m.pickVerb = func() verb { return fallbackVerb }
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m, c
}

func send(m *model, evs ...event.Event) {
	m.Update(eventsMsg(evs))
}

func press(m *model, k tea.KeyPressMsg) tea.Cmd {
	_, cmd := m.Update(k)
	return cmd
}

// typeText enters s as typing would, so it never collapses into a block.
func typeText(m *model, s string) {
	m.composer.update(tea.PasteMsg{Content: s})
	m.arrange()
}

// pasteText delivers s as a terminal paste.
func pasteText(m *model, s string) {
	m.Update(tea.PasteMsg{Content: s})
}

var (
	enter  = tea.KeyPressMsg{Code: tea.KeyEnter}
	ctrlC  = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	ctrlD  = tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	ctrlB  = tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	escape = tea.KeyPressMsg{Code: tea.KeyEscape}
	left   = tea.KeyPressMsg{Code: tea.KeyLeft}
)

// screen is the view's text, one string per row, with trailing blanks trimmed.
func screen(m *model) []string {
	rows := strings.Split(ansi.Strip(m.View().Content), "\n")
	for i, r := range rows {
		rows[i] = strings.TrimRight(r, " ")
	}
	return rows
}

// body is the transcript's text below the header.
func body(m *model) []string {
	var out []string
	for _, l := range m.view().lines[len(header(m.version, m.th)):] {
		out = append(out, strings.TrimRight(ansi.Strip(l.text), " "))
	}
	return out
}

var readAction = tool.Action{Verb: "Read", One: "file", Many: "files"}

func start(id string) event.ToolStart {
	return event.ToolStart{CallID: id, Name: "Read", Title: id + ".go", Action: readAction}
}

func ok(id, summary string) event.ToolEnd {
	return okOut(id, summary, "")
}

func okOut(id, summary, output string) event.ToolEnd {
	return event.ToolEnd{CallID: id, Name: "Read", Summary: summary, Output: output}
}

func TestPromptTurnRendersUserBoxThenLiveThenSealedGroup(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)

	send(m, event.TurnStart{Prompt: "read them"}, start("a"), ok("a", "3 lines"), start("b"), ok("b", "4 lines"))
	assert.Equal(t, []string{
		"", "", "❯ read them", "",
		"", "• Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 4 lines",
	}, body(m))

	send(m, event.TextDelta{Text: "Done."})
	assert.Equal(t, []string{
		"", "", "❯ read them", "",
		"", "▸ Read 2 files",
		"", "Done.",
	}, body(m))
}

func TestRowStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []event.Event
		want []string
	}{
		{"running", []event.Event{start("a")}, []string{"", "• a.go"}},
		{"ok", []event.Event{start("a"), ok("a", "3 lines")}, []string{"", "• a.go", "  ↳ 3 lines"}},
		{
			"failed",
			[]event.Event{start("a"), event.ToolEnd{CallID: "a", IsError: true, Output: "boom\nbang\n"}},
			[]string{"", "• a.go", "  boom", "  bang"},
		},
		{
			"unfinished",
			[]event.Event{start("a"), event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}}},
			[]string{"", "• a.go", "  ↳ interrupted", "", "κ Interrupted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			send(m, tc.evs...)
			assert.Equal(t, tc.want, body(m))
		})
	}
}

func TestTurnLine(t *testing.T) {
	for _, tc := range []struct {
		status  event.Status
		elapsed time.Duration
		want    []string
	}{
		{event.Completed, 41 * time.Second, []string{"", "κ Worked for 41s"}},
		{event.Completed, 0, nil},
		{event.Interrupted, 12 * time.Second, []string{"", "κ Interrupted after 12s"}},
		{event.Interrupted, 0, []string{"", "κ Interrupted"}},
		{event.Failed, 12 * time.Second, []string{"", "κ Failed after 12s"}},
		{event.Failed, 0, []string{"", "κ Failed"}},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.status, tc.elapsed), func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			send(m, event.TurnEnd{Outcome: event.Outcome{Status: tc.status}, Elapsed: tc.elapsed})
			assert.Equal(t, tc.want, body(m))
		})
	}
}

func TestFailedTurnShowsItsReason(t *testing.T) {
	failed := func(msg string) event.Outcome { return event.Outcome{Status: event.Failed, Message: msg} }
	for _, tc := range []struct {
		name string
		ev   event.TurnEnd
		want []string
	}{
		{"live", event.TurnEnd{Outcome: failed("401: token expired\nlog in again"), Elapsed: 2 * time.Second}, []string{"", "κ Failed after 2s", "  401: token expired", "  log in again"}},
		{"restored", event.TurnEnd{Outcome: failed("rate limited")}, []string{"", "κ Failed", "  rate limited"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			send(m, tc.ev)
			assert.Equal(t, tc.want, body(m))
		})
	}
}

func TestNoticeAndErrorContinueTwoIn(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)

	send(m, event.Notice{Text: "first\nsecond"}, event.Error{Err: errors.New("bad\nworse")})

	assert.Equal(t, []string{"", "κ first", "  second", "", "κ bad", "  worse"}, body(m))
}

func TestWrapsToWidthAndReflowsOnResize(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m,
		event.TextDelta{Text: "alpha beta gamma"},
		event.Notice{Text: "one two three"},
		start("a-very-long-name"),
	)
	assert.Equal(t, []string{"", "alpha beta gamma", "", "κ one two three", "", "• a-very-long-name.go"}, body(m))

	m.Update(tea.WindowSizeMsg{Width: 12, Height: 30})

	assert.Equal(t, []string{
		"", "alpha beta", "gamma",
		"", "κ one two", "  three",
		"", "• a-very-", "  long-", "  name.go",
	}, body(m))
	for _, l := range m.view().lines[len(header(m.version, m.th)):] {
		assert.LessOrEqual(t, lipgloss.Width(l.text), 12, ansi.Strip(l.text))
	}
}

func TestComposerIsShadedFullWidthWithPromptAtEdge(t *testing.T) {
	m, _ := newTestModel(t, 30, 30)
	typeText(m, "hello")

	rows := strings.Split(m.View().Content, "\n")
	composer := rows[len(rows)-4 : len(rows)-1]
	for _, r := range composer {
		assert.Equal(t, 30, lipgloss.Width(r))
	}
	assert.Equal(t, []string{"", "❯ hello", ""}, screen(m)[len(rows)-4:len(rows)-1])
}

func TestComposerGrowsToAThirdOfTheHeight(t *testing.T) {
	m, _ := newTestModel(t, 30, 30)

	typeText(m, "a\nb")
	assert.Equal(t, 2, m.composer.height())
	assert.Equal(t, "  b", screen(m)[len(screen(m))-3])

	typeText(m, strings.Repeat("\nline", 20))
	assert.Equal(t, 10, m.composer.height())
	assert.Len(t, screen(m), 30)
}

func TestStatusRefreshesOnEventsThatChangeIt(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	last := func() string { return screen(m)[len(screen(m))-1] }
	assert.Equal(t, "  model-1"+strings.Repeat(" ", 25)+"plan", last())

	c.status.Branch = "main"
	send(m, event.Notice{Text: "no refresh"})
	assert.Equal(t, "  model-1"+strings.Repeat(" ", 25)+"plan", last())

	send(m, event.TurnEnd{})
	assert.Equal(t, "  model-1 · main"+strings.Repeat(" ", 18)+"plan", last())

	c.status.Mode = policy.Execute
	send(m, event.ModeChanged{Mode: policy.Execute})
	assert.Equal(t, "  model-1 · main"+strings.Repeat(" ", 15)+"execute", last())

	c.status.Shells = 2
	send(m, event.TasksChanged{})
	assert.Equal(t, "  model-1 · main"+strings.Repeat(" ", 4)+"2 shells · execute", last())
}

func TestBackgroundKeyIsConsumedOnlyWhenACommandMoves(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	typeText(m, "ab")

	c.foreground = true
	assert.Nil(t, press(m, ctrlB))
	assert.Equal(t, 1, c.backgrounds)
	assert.Equal(t, 2, m.composer.ta.Column(), "a moved command takes the key")

	c.foreground = false
	press(m, ctrlB)
	assert.Equal(t, 2, c.backgrounds)
	assert.Equal(t, 1, m.composer.ta.Column(), "otherwise the composer moves the cursor left")
	assert.Equal(t, "ab", m.composer.value())
}

func TestBackgroundKeyWaitsForAnApproval(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.foreground = true
	send(m, editApproval())

	press(m, ctrlB)

	assert.Zero(t, c.backgrounds)
	assert.Empty(t, c.answers)
}

func TestLightBackgroundSwitchesTheme(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)

	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	assert.Equal(t, mocha(), m.th)

	m.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, latte(), m.th)
	assert.Equal(t, latte().userMessageBg, m.composer.ta.Styles().Focused.Text.GetBackground())
}

func TestSessionChangedRestoresHistory(t *testing.T) {
	m, _ := newTestModel(t, 40, 40)
	send(m, event.Notice{Text: "old"})

	send(m, event.SessionChanged{SessionID: "s2", Resumed: true, History: []event.Event{
		event.TurnStart{Prompt: "earlier"},
		start("a"), okOut("a", "3 lines", "a1"), start("b"), ok("b", "1 line"),
		event.TextDelta{Text: "reply"},
		start("c"),
		event.ToolEnd{CallID: "c", IsError: true, Output: "nope"},
		event.TurnEnd{Elapsed: 5 * time.Second},
	}})
	send(m, start("d"), ok("d", "2 lines"), start("e"), ok("e", "4 lines"))

	assert.Equal(t, []string{
		"", "", "❯ earlier", "",
		"", "▸ Read 2 files",
		"", "reply",
		"", "• c.go", "  nope",
		"", "κ Worked for 5s",
		"", "• Read 2 files", "  ↳ d.go · 2 lines", "  ↳ e.go · 4 lines",
	}, body(m))
	assert.Equal(t, "        ▄ ▄", screen(m)[0], "the header stays first")
}

func TestSessionChangedClearsTranscript(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	send(m, event.Notice{Text: "old"})

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.Empty(t, body(m))
}

func TestFollowsTailUntilScrolledUp(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	for i := range 20 {
		send(m, event.Notice{Text: fmt.Sprintf("n%d", i)})
	}
	assert.Contains(t, screen(m), "κ n19")

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.NotContains(t, screen(m), "κ n19")
	top := screen(m)[0]

	send(m, event.Notice{Text: "n20"})
	assert.Equal(t, top, screen(m)[0], "scrolled up stays put")
	assert.NotContains(t, screen(m), "κ n20")

	press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	send(m, event.Notice{Text: "n21"})
	assert.Contains(t, screen(m), "κ n21", "back at the bottom follows again")

	press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	assert.NotContains(t, screen(m), "κ n21")
	for range 5 {
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	assert.Contains(t, screen(m), "κ n21")
}

func TestHeaderShowsVersion(t *testing.T) {
	m, _ := newTestModel(t, 30, 30)
	assert.Contains(t, screen(m), "  kopi v1.2.3")
	assert.Equal(t, "        ▄ ▄", screen(m)[0])
}

// submitText types text and presses Enter, returning the key's command.
func submitText(m *model, text string) tea.Cmd {
	typeText(m, text)
	return press(m, enter)
}

func TestEnterSubmitsInlineAndClearsOnlyOnAcceptance(t *testing.T) {
	m, c := newTestModel(t, 30, 20)

	assert.Nil(t, submitText(m, "hello"))
	assert.Equal(t, []string{"hello"}, c.submitted)
	assert.Empty(t, m.composer.value())

	c.submitErr = errors.New("boom")
	submitText(m, "again")
	assert.Equal(t, []string{"hello", "again"}, c.submitted)
	assert.Equal(t, "again", m.composer.value())
}

func TestSubmitSendsExpandedTextAndImages(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	img := writeImage(t, t.TempDir(), "a.png")
	n := imageCount.Load() + 1
	typeText(m, "look ")
	pasteText(m, img)
	pasteText(m, linesOf(11))

	assert.Nil(t, press(m, enter))

	assert.Equal(t, []string{fmt.Sprintf("look [image #%d]", n) + linesOf(11)}, c.submitted)
	assert.Equal(t, [][]string{{img}}, c.images)
	assert.Empty(t, m.composer.value())
	assert.Empty(t, m.composer.blocks)
}

func TestBlankEnterDoesNothing(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	typeText(m, "  ")

	assert.Nil(t, press(m, enter))
	assert.Equal(t, "  ", m.composer.value())
}

func TestNewlineKeysInsertNewline(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyEnter, Mod: tea.ModAlt},
		{Code: tea.KeyEnter, Mod: tea.ModShift},
		{Code: 'j', Mod: tea.ModCtrl},
	} {
		t.Run(k.String(), func(t *testing.T) {
			m, c := newTestModel(t, 30, 20)
			typeText(m, "a")
			assert.Nil(t, press(m, k))
			typeText(m, "b")
			assert.Equal(t, "a\nb", m.composer.value())
			assert.Empty(t, c.submitted)
		})
	}
}

func TestEnterWhileBusyQueuesAndClears(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	send(m, event.TurnStart{Prompt: "first"})

	submitText(m, "more")

	assert.Equal(t, []string{"more"}, c.submitted)
	assert.Empty(t, m.composer.value())
	s := screen(m)
	assert.Equal(t, "⠋ Working… 0s", s[len(s)-6], "no hint on the working row")
}

func TestBusyRefusalKeepsDraftAndFlashes(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = app.ErrBusy

	cmd := submitText(m, "more")

	assert.NotNil(t, cmd, "the flash expires")
	assert.Equal(t, "more", m.composer.value())
	assert.Contains(t, statusText(m), "busy opening a session")
	assert.Empty(t, body(m))
}

func TestBadImageRefusalKeepsDraft(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = fmt.Errorf("%w: content is text/plain", app.ErrBadImage)

	submitText(m, "look")

	assert.Equal(t, "look", m.composer.value())
	assert.Equal(t, []string{"", "κ not a supported image: content is text/plain"}, body(m))
}

func TestCommandDuringTurnIsSubmitted(t *testing.T) {
	m, c := newTestModel(t, 30, 20)
	send(m, event.TurnStart{Prompt: "first"})

	submitText(m, "/status")

	assert.Equal(t, []string{"/status"}, c.submitted)
}

func TestSubmitErrors(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = fmt.Errorf("%w: /nope", app.ErrUnknownCommand)

	assert.Nil(t, submitText(m, "/nope"))
	assert.Equal(t, "/nope", m.composer.value(), "the draft is kept so a typo can be fixed")
	assert.Equal(t, []string{"", "κ unknown command: /nope"}, body(m))

	m.composer.reset()
	c.submitErr = app.ErrQuit
	cmd := submitText(m, "/quit")
	require.NotNil(t, cmd)
	assert.Equal(t, tea.QuitMsg{}, cmd())
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// statusText is the status line's text.
func statusText(m *model) string {
	s := screen(m)
	return s[len(s)-1-m.geo.agents]
}

func TestCancelInterruptsThenClearsThenTakesTwoToQuit(t *testing.T) {
	for _, msg := range []tea.Msg{ctrlC, interruptMsg{}} {
		t.Run(fmt.Sprintf("%T", msg), func(t *testing.T) {
			// isQuit runs the flash's tea.Tick; the bubble's fake clock fires it at once.
			synctest.Test(t, func(t *testing.T) {
				cancel := func(m *model) tea.Cmd { _, cmd := m.Update(msg); return cmd }
				m, c := newTestModel(t, 40, 20)
				typeText(m, "draft")

				c.running = true
				assert.Nil(t, cancel(m))
				assert.Equal(t, 1, c.interrupts)
				assert.Equal(t, "draft", m.composer.value(), "interrupting keeps the draft")

				c.running = false
				assert.Nil(t, cancel(m))
				assert.Empty(t, m.composer.value(), "at idle the draft is cleared")

				cmd := cancel(m)
				assert.False(t, isQuit(cmd))
				assert.True(t, strings.HasSuffix(statusText(m), "ctrl+c again to exit · plan"), statusText(m))
				assert.True(t, isQuit(cancel(m)))

				m.Update(flashMsg{m.flashSeq})
				assert.False(t, strings.Contains(statusText(m), "again to exit"), "the hint expires")
				assert.False(t, isQuit(cancel(m)), "the next press arms again")
				assert.Contains(t, statusText(m), "ctrl+c again to exit")

				press(m, left)
				assert.NotContains(t, statusText(m), "again to exit", "another key disarms")
				assert.False(t, isQuit(cancel(m)))
			})
		})
	}
}

func TestStaleFlashMsgKeepsNewerFlash(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	m.flash("first")
	stale := m.flashSeq
	m.flash("second")

	m.Update(flashMsg{stale})

	assert.Contains(t, statusText(m), "second")
}

func TestCtrlDQuitsOnlyWhenEmpty(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	typeText(m, "ab")
	press(m, tea.KeyPressMsg{Code: tea.KeyHome})

	assert.False(t, isQuit(press(m, ctrlD)))
	assert.Equal(t, "b", m.composer.value(), "with text it deletes forward")

	m.composer.reset()
	assert.True(t, isQuit(press(m, ctrlD)))
}

func TestEscInterruptsAndNeverQuits(t *testing.T) {
	m, c := newTestModel(t, 30, 20)
	c.running = true
	assert.Nil(t, press(m, escape))

	c.running = false
	typeText(m, "draft")
	assert.Nil(t, press(m, escape))
	assert.Equal(t, 2, c.interrupts)
	assert.Equal(t, "draft", m.composer.value(), "esc at idle keeps the draft")
}

func TestReboundKeys(t *testing.T) {
	opts := testOptions()
	opts.Keys = map[string][]string{"submit": {"ctrl+s"}}
	m, c := newTestModelWith(t, 30, 20, opts)
	typeText(m, "hi")

	press(m, enter)
	assert.Empty(t, c.submitted, "enter no longer submits")
	press(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.Equal(t, []string{"hi"}, c.submitted)

	assert.False(t, isQuit(press(m, ctrlC)), "cancel is unbound")
	assert.False(t, isQuit(press(m, ctrlC)))
	assert.Zero(t, c.interrupts)
}

func TestThinkingBatchKeepsLayoutAndRearmsBridge(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	send(m, event.TurnStart{Prompt: "go"}, event.Notice{Text: "visible"})
	oldLines := &m.view().lines[0]
	oldView := m.View().Content
	m.bridge.push(event.ThinkingDelta{Text: "thinking"})
	_, cmd := m.Update(m.bridge.wait()())
	require.NotNil(t, cmd)
	assert.Same(t, oldLines, &m.view().lines[0])
	assert.Equal(t, oldView, m.View().Content)
	assert.True(t, m.view().busy)
	m.bridge.push(event.TextDelta{Text: "answer"})
	m.Update(cmd())
	assert.Contains(t, body(m), "answer", "the bridge wait was rearmed")
	assert.NotSame(t, oldLines, &m.view().lines[0])
}

func TestMixedThinkingBatchRebuildsVisibleLayout(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	_, cmd := m.Update(eventsMsg{event.ThinkingDelta{Text: "thinking"}, event.TurnStart{Prompt: "go"}, event.TextDelta{Text: "answer"}})
	require.NotNil(t, cmd)
	assert.True(t, m.view().busy)
	assert.Contains(t, body(m), "answer")
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	assert.Len(t, batch, 2, "mixed turn start still starts the spinner and rearms the bridge")
}

func TestInitWaitsForEvents(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	m.bridge.push(event.Notice{Text: "early"})

	_, cmd := m.Update(m.bridge.wait()())

	require.NotNil(t, cmd, "the wait is re-armed")
	assert.Equal(t, []string{"", "κ early"}, body(m))
}

var (
	up        = tea.KeyPressMsg{Code: tea.KeyUp}
	down      = tea.KeyPressMsg{Code: tea.KeyDown}
	ctrlEnter = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}
	shiftTab  = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
)

func TestSendNowSendsExpandedTextAndImages(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	img := writeImage(t, t.TempDir(), "a.png")
	n := imageCount.Load() + 1
	typeText(m, "look ")
	pasteText(m, img)
	pasteText(m, linesOf(11))

	assert.Nil(t, press(m, ctrlEnter))

	assert.Equal(t, []string{fmt.Sprintf("look [image #%d]", n) + linesOf(11)}, c.sentNow)
	assert.Equal(t, [][]string{{img}}, c.nowImages)
	assert.Empty(t, c.submitted)
	assert.Empty(t, m.composer.value())
}

func TestSendNowWithEmptyDraftSendsTheQueue(t *testing.T) {
	m, c := newTestModel(t, 40, 20)

	assert.Nil(t, press(m, ctrlEnter))

	assert.Equal(t, []string{""}, c.sentNow)
}

func TestSendNowRefusalKeepsDraft(t *testing.T) {
	m, c := newTestModel(t, 40, 20)
	c.sendNowErr = errors.New("boom")
	typeText(m, "draft")

	press(m, ctrlEnter)

	assert.Equal(t, "draft", m.composer.value())
	assert.Equal(t, []string{"", "κ boom"}, body(m))
}

// queueTwo starts a turn with a and b queued.
func queueTwo(m *model, c *fakeController) {
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{"a", "b"}})
	c.queued = []event.Input{{Text: "a"}, {Text: "b"}}
}

func TestQueueEditMovesQueueBeforeDraft(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	queueTwo(m, c)
	typeText(m, "draft")

	assert.Nil(t, press(m, up))

	assert.Equal(t, 1, c.takes)
	assert.Equal(t, "a\n\nb\n\ndraft", m.composer.value())
	assert.NotContains(t, strings.Join(screen(m), "\n"), "queued", "the area empties at once")
	press(m, up)
	assert.Equal(t, 1, c.takes, "a second press does not take again")
}

func TestQueueEditAfterSubmitRestoresBeforeNewDraft(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	submitText(m, "x")
	c.queued = []event.Input{{Text: "x"}}
	send(m, event.QueueChanged{Pending: []string{"x"}})
	typeText(m, "draft")

	press(m, up)

	assert.Equal(t, "x\n\ndraft", m.composer.value())
	assert.Equal(t, 1, c.takes)
}

func TestQueueEditRegistersImages(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	m.composer.insertImage("/tmp/a.png")
	mk := fmt.Sprintf("[image #%d]", imageCount.Load())
	m.composer.reset()
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{"see " + mk}})
	c.queued = []event.Input{{Text: "see " + mk, Images: []string{"/tmp/a.png"}}}

	press(m, up)

	text, imgs := m.composer.expanded()
	assert.Equal(t, "see "+mk, text)
	assert.Equal(t, []string{"/tmp/a.png"}, imgs)
}

func TestUpWithNothingPendingBrowsesHistory(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.history = []string{"old"}
	m.composer.setHistory(c.History())
	send(m, event.TurnStart{Prompt: "go"})

	press(m, up)

	assert.Zero(t, c.takes)
	assert.Equal(t, "old", m.composer.value())
}

func TestUpInsideMultiLineDraftMovesCursor(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	queueTwo(m, c)
	c.history = []string{"old"}
	m.composer.setHistory(c.History())
	typeText(m, "one\ntwo\nthree")

	press(m, up)

	assert.Zero(t, c.takes)
	assert.Equal(t, "one\ntwo\nthree", m.composer.value())
	assert.Equal(t, 1, m.composer.ta.Line())
}

func TestUnsentInputReturnsBeforeDraft(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	typeText(m, "draft")

	send(m, event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "a"}, {Text: "b"}}})

	assert.Equal(t, "a\n\nb\n\ndraft", m.composer.value())
}

// submitThenStop types text and presses Enter in a running turn, then
// delivers the stopped turn's TurnEnd handing back unsent.
func submitThenStop(m *model, text string, unsent ...event.Input) {
	send(m, event.TurnStart{Prompt: "go"})
	submitText(m, text)
	send(m, event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}, Unsent: unsent})
}

func TestUnsentInputAfterAcceptedSubmitIsAllThatIsLeft(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)

	submitThenStop(m, "/new", event.Input{Text: "fix the test"})

	assert.Equal(t, "fix the test", m.composer.value())
}

func TestUnsentInputAfterRefusedSubmitGoesBeforeIt(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.submitErr = errors.New("boom")

	submitThenStop(m, "/new", event.Input{Text: "fix the test"})

	assert.Equal(t, "fix the test\n\n/new", m.composer.value())
}

func TestUnsentInputAfterBusyRefusalGoesBeforeIt(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.submitErr = app.ErrBusy

	submitThenStop(m, "/new", event.Input{Text: "fix the test"})

	assert.Equal(t, "fix the test\n\n/new", m.composer.value())
}

func TestOwnTextHandedBackAfterAcceptedSubmitAppearsOnce(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)

	submitThenStop(m, "x", event.Input{Text: "x"})

	assert.Equal(t, "x", m.composer.value())
}

func TestEnterThenCancelSubmitsBeforeInterrupting(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.running = true
	send(m, event.TurnStart{Prompt: "go"})

	submitText(m, "x")
	assert.Empty(t, m.composer.value())
	press(m, ctrlC)

	assert.Equal(t, []string{"submit x", "interrupt"}, c.calls)
	send(m, event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "x"}}})
	assert.Equal(t, "x", m.composer.value())
}

func TestAcceptedSubmitReloadsHistory(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	require.Equal(t, 1, c.histories, "loaded at start")
	c.history = []string{"hello"}

	submitText(m, "hello")

	assert.Equal(t, 2, c.histories)
	press(m, up)
	assert.Equal(t, "hello", m.composer.value())
}

func TestToggleModeKey(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	assert.Nil(t, press(m, shiftTab))
	assert.Equal(t, 1, c.toggles)

	opts := testOptions()
	opts.Keys["toggle-mode"] = []string{"ctrl+t"}
	m, c = newTestModelWith(t, 60, 30, opts)
	press(m, shiftTab)
	press(m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Equal(t, 1, c.toggles)
}

func editApproval() event.ApprovalRequested {
	return event.ApprovalRequested{
		Token: 1, CallID: "c1", Title: "Edit a.go",
		Preview: []tool.PreviewLine{{Kind: tool.Removed, Text: "old"}, {Kind: tool.Added, Text: "new"}},
		Edits:   true,
	}
}

func digit(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

func TestApprovalCapturesKeys(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())

	press(m, digit('x'))
	pasteText(m, "pasted")
	press(m, shiftTab)

	assert.Empty(t, m.composer.value())
	assert.Zero(t, c.toggles)
	assert.Empty(t, c.answers)
	assert.Contains(t, screen(m), "❭ 1. Yes")
}

func TestApprovalEnterAnswersTheSelectedOption(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())

	press(m, up)
	press(m, down)
	press(m, down)
	press(m, down)
	press(m, up)
	press(m, enter)

	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.AllowEdits}}}, c.answers)
	assert.Contains(t, screen(m), "❭ 2. Yes, allow all edits this session", "the prompt stays until it ends")
}

func TestApprovalFollowsReboundMenuKeys(t *testing.T) {
	opts := testOptions()
	opts.Keys["menu-up"] = []string{"ctrl+p"}
	opts.Keys["menu-down"] = []string{"ctrl+n"}
	m, c := newTestModelWith(t, 60, 30, opts)
	send(m, editApproval())

	press(m, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	press(m, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	press(m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	assert.Contains(t, screen(m), "❭ 2. Yes, allow all edits this session")

	press(m, down)
	press(m, up)
	press(m, up)
	assert.Contains(t, screen(m), "❭ 2. Yes, allow all edits this session", "the arrow keys are unbound")
	press(m, enter)
	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.AllowEdits}}}, c.answers)
}

func TestApprovalDigitAnswersDirectly(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())

	press(m, digit('4'))
	press(m, digit('1'))
	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.Allow}}}, c.answers)

	press(m, digit('3'))
	assert.Contains(t, screen(m), "❭ 3. No (esc)  What should change?", "the No digit moves to its field")
	press(m, digit('1'))
	assert.Contains(t, screen(m), "❭ 3. No (esc)  1", "digits in the field are text")
	assert.Len(t, c.answers, 1)
}

func TestApprovalEscRejects(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())
	press(m, digit('3'))
	keys(m, "draft")

	press(m, escape)

	assert.Equal(t, []answer{{1, policy.Decision{}}}, c.answers, "esc drops the draft")
	assert.Zero(t, c.interrupts, "the controller interrupts on a rejection")
}

func TestApprovalNoSendsTheReason(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())
	press(m, down)
	press(m, down)
	keys(m, "use b.go")
	assert.Contains(t, screen(m), "❭ 3. No (esc)  use b.go")

	press(m, enter)

	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.Reject, Reason: "use b.go"}}}, c.answers)
}

func TestApprovalPasteReachesTheField(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: " from\r\nclipboard"}
	send(m, editApproval())
	press(m, digit('3'))
	big := strings.Repeat("line\r\n", 20)
	pasteText(m, big)
	assert.Contains(t, screen(m), "❭ 3. No (esc)  [paste #1 +20 lines]")

	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)
	m.Update(cmd())
	press(m, enter)

	want := strings.ReplaceAll(big, "\r\n", "\n") + " from\nclipboard"
	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.Reject, Reason: want}}}, c.answers)
	assert.Empty(t, m.composer.value())
}

func TestApprovalClipboardCompletionAfterTransitionIsDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   event.Event
	}{
		{"approval ended", event.ApprovalEnded{Token: 1}},
		{"turn ended", event.TurnEnd{}},
		{"session changed", event.SessionChanged{SessionID: "s2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			m.clip = &fakeClipboard{text: "stale clipboard"}
			typeText(m, "composer draft")
			send(m, editApproval())
			press(m, digit('3'))
			keys(m, "feedback")
			oldField := m.dialog.field()
			cmd := press(m, ctrlV)
			require.NotNil(t, cmd)

			send(m, tc.ev)
			if m.approval != nil {
				press(m, digit('3'))
				keys(m, "replacement feedback")
			}
			m.Update(cmd())

			assert.Equal(t, "composer draft", m.composer.value())
			assert.Equal(t, "feedback", oldField.value())
			if m.approval != nil {
				assert.Equal(t, "replacement feedback", m.dialog.text())
			}
		})
	}
}

func TestApprovalClipboardCompletionAfterPromotionIsDiscarded(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: "stale clipboard"}
	send(m, editApproval(), subagentAsk(2, "explore"))
	press(m, digit('3'))
	keys(m, "feedback")
	oldField := m.dialog.field()
	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)

	send(m, event.ApprovalEnded{Token: 1})
	require.Equal(t, uint64(2), m.approval.Token)
	press(m, digit('3'))
	m.Update(cmd())

	assert.Equal(t, "feedback", oldField.value())
	assert.Empty(t, m.dialog.text(), "the promoted ask's reason field is unchanged")
}

func subagentAsk(token uint64, agent string) event.ApprovalRequested {
	return event.ApprovalRequested{Token: token, Title: "Edit " + agent + ".go", Agent: agent, TaskID: fmt.Sprintf("t%d", token)}
}

func TestApprovalQueuePromotesInOrder(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval(), subagentAsk(2, "explore"), subagentAsk(3, "review"))
	assert.Equal(t, uint64(1), m.approval.Token)
	assert.Contains(t, strings.Join(screen(m), "\n"), "Edit a.go")

	send(m, event.ApprovalEnded{Token: 1})
	assert.Equal(t, uint64(2), m.approval.Token)
	assert.Contains(t, strings.Join(screen(m), "\n"), "explore t2 › Edit explore.go")
	press(m, enter)
	assert.Equal(t, []answer{{2, policy.Decision{Answer: policy.Allow}}}, c.answers, "answers use the shown ask's token")

	send(m, event.ApprovalEnded{Token: 2})
	assert.Equal(t, uint64(3), m.approval.Token)
	send(m, event.ApprovalEnded{Token: 3})
	assert.Nil(t, m.approval)
	assert.Nil(t, m.dialog)
}

func TestApprovalQueueEndOfAQueuedAskRemovesIt(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval(), subagentAsk(2, "explore"), subagentAsk(3, "review"))

	send(m, event.ApprovalEnded{Token: 2})
	assert.Equal(t, uint64(1), m.approval.Token)
	send(m, event.ApprovalEnded{Token: 1})
	assert.Equal(t, uint64(3), m.approval.Token)
}

func TestApprovalQueueRootTurnEndKeepsSubagentAsks(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval(), subagentAsk(2, "explore"))
	send(m, event.TurnEnd{})
	assert.Equal(t, uint64(2), m.approval.Token, "the root's ask is dropped and the subagent's is shown")
	assert.Contains(t, strings.Join(screen(m), "\n"), "explore t2 › Edit explore.go")

	send(m, event.TurnEnd{})
	assert.Equal(t, uint64(2), m.approval.Token)

	m, _ = newTestModel(t, 60, 30)
	send(m, subagentAsk(2, "explore"), editApproval())
	send(m, event.TurnEnd{})
	assert.Equal(t, uint64(2), m.approval.Token)
	send(m, event.ApprovalEnded{Token: 2})
	assert.Nil(t, m.approval, "the queued root ask was dropped")
}

func TestApprovalQueueSessionChangeClearsAll(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval(), subagentAsk(2, "explore"))
	send(m, event.SessionChanged{SessionID: "s2"})
	assert.Nil(t, m.approval)
	assert.Nil(t, m.dialog)
	assert.Empty(t, m.approvalQueue)
}

func TestApprovalClipboardCompletionAfterLeavingFieldIsDiscarded(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: "stale clipboard"}
	send(m, editApproval())
	press(m, digit('3'))
	keys(m, "feedback")
	field := m.dialog.field()
	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)

	press(m, up)
	m.Update(cmd())

	assert.Equal(t, "feedback", field.value())
	assert.Empty(t, m.composer.value())
}

func TestApprovalFieldGrowsWithinTheScreen(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval())
	press(m, digit('3'))
	before := len(m.prompt)
	for i, r := range "abcdefghijk" {
		if i > 0 {
			press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
		}
		press(m, digit(r))
	}

	s := screen(m)
	assert.Len(t, s, 30)
	assert.Greater(t, len(m.prompt), before)
	want := []string{"❭ 3. No (esc)"}
	for _, r := range "bcdefghijk" {
		want = append(want, "  "+string(r))
	}
	want = append(want, strings.Repeat("─", 60))
	assert.Equal(t, want, s[len(s)-len(want)-1:len(s)-1], "the field grows to a third of the screen, then scrolls")
}

func TestApprovalTallDraftKeepsOptionsOnASmallScreen(t *testing.T) {
	m, c := newTestModel(t, 60, 10)
	send(m, event.TurnStart{}, editApproval())
	press(m, digit('3'))
	keys(m, "a")
	press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	keys(m, "b")
	press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	keys(m, "c")
	for range 4 {
		press(m, up)
	}

	s := screen(m)
	assert.Len(t, s, 10)
	assert.Contains(t, s, "❭ 1. Yes")
	assert.Contains(t, s, "  2. Yes, allow all edits this session")
	assert.Contains(t, s, "  3. No (esc)")
	press(m, enter)
	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.Allow}}}, c.answers)
}

func TestApprovalPasteKeyLeavesAnImageOut(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.imageDir = t.TempDir()
	m.clip = &fakeClipboard{image: []byte("png bytes")}
	send(m, editApproval())
	press(m, digit('3'))

	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)
	m.Update(cmd())

	assert.Empty(t, m.dialog.options[2].field.value())
	assert.Empty(t, m.composer.value())
}

func TestApprovalUpLeavesTheFieldKeepingTheDraft(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, editApproval())
	press(m, digit('3'))
	keys(m, "draft")

	press(m, up)
	assert.Contains(t, screen(m), "❭ 2. Yes, allow all edits this session")
	assert.Contains(t, screen(m), "  3. No (esc)  draft")
	press(m, down)
	press(m, enter)

	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.Reject, Reason: "draft"}}}, c.answers)
}

func TestApprovalEndedRestoresTheComposerWithItsDraft(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	typeText(m, "draft")
	send(m, editApproval())
	assert.NotContains(t, screen(m), "❯ draft")

	send(m, event.ApprovalEnded{Token: 2})
	assert.Contains(t, screen(m), "❭ 1. Yes", "another call's end leaves the prompt")

	send(m, event.ApprovalEnded{Token: 1})
	assert.Contains(t, screen(m), "❯ draft")
	assert.NotContains(t, screen(m), "❭ 1. Yes")
	assert.Len(t, screen(m), 30)
}

func TestStaleApprovalEndedCannotDismissReusedCallID(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	first := editApproval()
	send(m, first, event.ApprovalEnded{Token: first.Token})
	second := first
	second.Token++
	second.Title = "Edit b.go"
	send(m, second, event.ApprovalEnded{Token: first.Token})

	require.NotNil(t, m.approval)
	assert.Equal(t, second, *m.approval)
	assert.Contains(t, screen(m), "  Edit b.go")
	press(m, enter)
	assert.Equal(t, []answer{{second.Token, policy.Decision{Answer: policy.Allow}}}, c.answers)
	send(m, event.ApprovalEnded{Token: second.Token})
	assert.Nil(t, m.approval)
}

func TestTurnEndAndSessionChangedClearTheApproval(t *testing.T) {
	for _, ev := range []event.Event{event.TurnEnd{}, event.SessionChanged{SessionID: "s2"}} {
		m, _ := newTestModel(t, 60, 30)
		send(m, editApproval())
		send(m, ev)
		assert.Nil(t, m.approval)
		assert.Contains(t, screen(m), "❯")
		assert.NotContains(t, screen(m), "❭ 1. Yes")
	}
}

func TestWorkingRowWaitsDuringApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		ask := editApproval()
		send(m, event.TurnStart{}, ask)
		assert.Contains(t, screen(m), "⠋ Waiting for you…")
		assert.NotContains(t, screen(m), "⠋ Working… 0s")

		send(m, event.ApprovalEnded{Token: ask.Token})
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "⠋ Waiting for you…")
	})
}

func TestCancelInterruptsDuringApproval(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.running = true
	send(m, event.TurnStart{}, editApproval())

	assert.Nil(t, press(m, ctrlC))

	assert.Equal(t, 1, c.interrupts)
	assert.Empty(t, c.answers)
}

func TestApprovalSurvivesTinyTerminalResizes(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	send(m, event.TurnStart{}, editApproval())
	press(m, down)
	prompt := append([]string(nil), m.prompt...)

	for height := 0; height <= 4; height++ {
		require.NotPanics(t, func() {
			m.Update(tea.WindowSizeMsg{Width: 60, Height: height})
			m.View()
		}, "height %d", height)
		assert.NotNil(t, m.approval)
		assert.Empty(t, m.prompt)
	}

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	assert.Equal(t, prompt, m.prompt)
	s := screen(m)
	assert.Len(t, s, 30)
	assert.Contains(t, s, "  Edit a.go")
	assert.Contains(t, s, "  - old")
	assert.Contains(t, s, "  + new")
	assert.Equal(t, []string{"  1. Yes", "❭ 2. Yes, allow all edits this session", "  3. No (esc)  What should change?", strings.Repeat("─", 60)}, s[len(s)-5:len(s)-1])
	assert.Empty(t, c.answers)
	press(m, enter)
	assert.Equal(t, []answer{{1, policy.Decision{Answer: policy.AllowEdits}}}, c.answers)
}

func TestApprovalWithLongPreviewFitsTheTerminal(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	ev := editApproval()
	for i := range 50 {
		ev.Preview = append(ev.Preview, tool.PreviewLine{Kind: tool.Added, Text: fmt.Sprintf("line %d", i)})
	}
	send(m, event.TurnStart{}, ev)

	s := screen(m)
	assert.Len(t, s, 20)
	assert.Equal(t, []string{"❭ 1. Yes", "  2. Yes, allow all edits this session", "  3. No (esc)  What should change?", strings.Repeat("─", 40)}, s[len(s)-5:len(s)-1])
	assert.Contains(t, s, "    … 47 more lines")
	assert.Contains(t, s, "  Edit a.go")
}

func TestRowsFillTheScreenStatusLast(t *testing.T) {
	shiftEnter := tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	for _, tc := range []struct {
		name          string
		width, height int
		setup         func(m *model, c *fakeController)
	}{
		{"idle", 60, 30, func(*model, *fakeController) {}},
		{"menu", 60, 30, func(m *model, _ *fakeController) { keys(m, "/") }},
		{"menu and busy", 60, 30, func(m *model, _ *fakeController) {
			send(m, event.TurnStart{Prompt: "go"})
			keys(m, "/")
		}},
		{"short with queue and menu", 40, 10, func(m *model, c *fakeController) {
			queueTwo(m, c)
			keys(m, "/")
		}},
		{"approval over menu", 60, 30, func(m *model, _ *fakeController) {
			keys(m, "/")
			send(m, editApproval())
		}},
		{"approval with a tall reason", 60, 30, func(m *model, _ *fakeController) {
			send(m, editApproval())
			press(m, digit('3'))
			for i, r := range "abcdef" {
				if i > 0 {
					press(m, shiftEnter)
				}
				press(m, digit(r))
			}
		}},
		{"approval after a paste", 60, 30, func(m *model, _ *fakeController) {
			send(m, editApproval())
			press(m, digit('3'))
			pasteText(m, "one\ntwo\nthree")
		}},
		{"task output on a short screen", 40, 10, func(m *model, c *fakeController) {
			c.tasks = []tool.TaskInfo{{ID: "t1", Command: "yes", Status: tool.TaskRunning}}
			c.outputs = map[string]string{"t1": strings.Repeat("y\n", 50)}
			c.submitErr = app.ErrShowTasks
			submitText(m, "/tasks")
			press(m, enter)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := newMenuModel(t, tc.width, tc.height)
			tc.setup(m, c)

			s := screen(m)
			require.Len(t, s, m.height)
			assert.Equal(t, m.height, m.transcriptHeight()+len(m.below))
			status := strings.TrimRight(ansi.Strip(statusLine(m.statusInput(), m.layout, m.width, m.th)), " ")
			assert.Equal(t, status, s[m.height-1])
			if m.approval == nil && m.tasks == nil {
				assert.True(t, strings.HasPrefix(s[m.composerTop()], "❯"), "row %d: %q", m.composerTop(), s[m.composerTop()])
			}
		})
	}
}

func TestCommandSegmentResultsRenderAndRecover(t *testing.T) {
	opts := testOptions()
	opts.StatusLine = config.StatusLine{Left: []string{"git"}, Right: []string{"mode"}, Separator: " ", Segments: map[string]config.StatusSegment{"git": {Command: "git-info"}}}
	m, _ := newTestModelWith(t, 40, 20, opts)
	assert.NotContains(t, statusText(m), "ok")
	send(m, event.StatusCommandResult{Name: "git", Output: "\x1b[32mok\x1b[0m\nsecond line"})
	assert.Equal(t, "  ok"+strings.Repeat(" ", 30)+"plan", strings.TrimRight(ansi.Strip(statusText(m)), " "))
	assert.Contains(t, statusLine(m.statusInput(), m.layout, m.width, m.th), "\x1b[32mok")
	send(m, event.StatusCommandResult{Name: "git", Err: errors.New("exit status 1")})
	assert.NotContains(t, statusText(m), "ok", "a failed run hides the segment")
	send(m, event.StatusCommandResult{Name: "git", Output: "back"})
	assert.Contains(t, statusText(m), "back")
	assert.Empty(t, body(m), "footer results are not transcript events")
}
