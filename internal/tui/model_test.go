package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type fakeController struct {
	status     app.Status
	running    bool
	interrupts int
	submitted  []string
	submitErr  error
	subscriber func(event.Event)
}

func (f *fakeController) Subscribe(fn func(event.Event)) func() {
	f.subscriber = fn
	return func() { f.subscriber = nil }
}

func (f *fakeController) Submit(_ context.Context, input string) (*app.Work, error) {
	f.submitted = append(f.submitted, input)
	return nil, f.submitErr
}

func (f *fakeController) Interrupt() bool {
	f.interrupts++
	return f.running
}

func (f *fakeController) Status() app.Status { return f.status }

func newTestModel(t *testing.T, width, height int) (*model, *fakeController) {
	t.Helper()
	c := &fakeController{status: app.Status{Model: "mock/model-1", Mode: policy.Plan}}
	m := newModel(t.Context(), c, newBridge(), "v1.2.3")
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

func typeText(m *model, s string) {
	m.Update(tea.PasteMsg{Content: s})
}

var (
	enter  = tea.KeyPressMsg{Code: tea.KeyEnter}
	ctrlC  = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	escape = tea.KeyPressMsg{Code: tea.KeyEscape}
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
	for _, l := range m.lines[len(header(m.version, m.th)):] {
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
	for _, l := range m.lines[len(header(m.version, m.th)):] {
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
	assert.Equal(t, 2, m.composer.Height())
	assert.Equal(t, "  b", screen(m)[len(screen(m))-3])

	typeText(m, strings.Repeat("\nline", 20))
	assert.Equal(t, 10, m.composer.Height())
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
}

func TestLightBackgroundSwitchesTheme(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)

	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	assert.Equal(t, mocha(), m.th)

	m.Update(tea.BackgroundColorMsg{Color: color.White})
	assert.Equal(t, latte(), m.th)
	assert.Equal(t, latte().userMessageBg, m.composer.Styles().Focused.Text.GetBackground())
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

// submitText types text, presses Enter and runs the submit command,
// returning its result for the test to deliver.
func submitText(t *testing.T, m *model, text string) tea.Msg {
	t.Helper()
	typeText(m, text)
	cmd := press(m, enter)
	require.NotNil(t, cmd)
	return cmd()
}

func TestEnterKeepsTextUntilAcceptedThenClears(t *testing.T) {
	m, c := newTestModel(t, 30, 20)
	typeText(m, "hello")

	cmd := press(m, enter)

	require.NotNil(t, cmd)
	assert.Equal(t, "hello", m.composer.Value())
	assert.Empty(t, c.submitted, "Submit runs in the command, not in Update")
	assert.Nil(t, press(m, enter), "a second Enter while in flight submits nothing")
	msg := cmd()
	assert.Equal(t, []string{"hello"}, c.submitted)
	assert.Equal(t, "hello", m.composer.Value())

	m.Update(msg)
	assert.Empty(t, m.composer.Value())
}

func TestTextTypedWhileSubmittingIsKept(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	msg := submitText(t, m, "hello")
	typeText(m, " again")

	m.Update(msg)

	assert.Equal(t, "hello again", m.composer.Value())
}

func TestBlankEnterDoesNothing(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	typeText(m, "  ")

	assert.Nil(t, press(m, enter))
	assert.Equal(t, "  ", m.composer.Value())
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
			assert.Equal(t, "a\nb", m.composer.Value())
			assert.Empty(t, c.submitted)
		})
	}
}

func TestBusyRefusalKeepsDraftAndHintsOnWorkingRow(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	send(m, event.TurnStart{Prompt: "first"}, start("a"), ok("a", "1 line"), start("b"), ok("b", "2 lines"))
	before := body(m)
	c.submitErr = app.ErrBusy

	m.Update(submitText(t, m, "more"))

	assert.Equal(t, "more", m.composer.Value())
	assert.Equal(t, before, body(m), "no block is added and the live group stays live")
	assert.Contains(t, screen(m), "⠋ Working… 0s · esc to interrupt")
	assert.Nil(t, press(m, tea.KeyPressMsg{Code: tea.KeyLeft}))
	assert.Contains(t, screen(m), "⠋ Working… 0s")
	assert.NotContains(t, screen(m), "⠋ Working… 0s · esc to interrupt", "a key press clears the hint")

	m.Update(submitText(t, m, "!"))
	require.Contains(t, screen(m), "⠋ Working… 0s · esc to interrupt")
	send(m, event.TurnEnd{})
	assert.Empty(t, m.hint, "the turn ending clears the hint")
}

func TestBusyRefusalWithoutTurnOnlyKeepsDraft(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = app.ErrBusy

	m.Update(submitText(t, m, "more"))

	assert.Equal(t, "more", m.composer.Value())
	assert.Empty(t, m.hint)
	assert.Empty(t, body(m))
}

func TestCommandDuringTurnIsSubmitted(t *testing.T) {
	m, c := newTestModel(t, 30, 20)
	send(m, event.TurnStart{Prompt: "first"})

	submitText(t, m, "/status")

	assert.Equal(t, []string{"/status"}, c.submitted)
}

func TestSubmitErrors(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.submitErr = fmt.Errorf("%w: /nope", app.ErrUnknownCommand)

	_, cmd := m.Update(submitText(t, m, "/nope"))
	assert.Nil(t, cmd)
	assert.Equal(t, "/nope", m.composer.Value(), "the draft is kept so a typo can be fixed")
	assert.Equal(t, []string{"", "κ unknown command: /nope"}, body(m))

	m.composer.Reset()
	c.submitErr = app.ErrQuit
	_, cmd = m.Update(submitText(t, m, "/quit"))
	require.NotNil(t, cmd)
	assert.Equal(t, tea.QuitMsg{}, cmd())
}

func TestCtrlCInterruptsWhenBusyAndQuitsWhenIdle(t *testing.T) {
	for _, msg := range []tea.Msg{ctrlC, interruptMsg{}} {
		t.Run(fmt.Sprintf("%T", msg), func(t *testing.T) {
			m, c := newTestModel(t, 30, 20)

			c.running = true
			_, cmd := m.Update(msg)
			assert.Nil(t, cmd)
			assert.Equal(t, 1, c.interrupts)

			c.running = false
			_, cmd = m.Update(msg)
			require.NotNil(t, cmd)
			assert.Equal(t, tea.QuitMsg{}, cmd())
		})
	}
}

func TestEscInterruptsAndNeverQuits(t *testing.T) {
	m, c := newTestModel(t, 30, 20)
	c.running = true
	assert.Nil(t, press(m, escape))

	c.running = false
	assert.Nil(t, press(m, escape))
	assert.Equal(t, 2, c.interrupts)
}

func TestInitWaitsForEvents(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	m.bridge.push(event.Notice{Text: "early"})

	_, cmd := m.Update(m.bridge.wait()())

	require.NotNil(t, cmd, "the wait is re-armed")
	assert.Equal(t, []string{"", "κ early"}, body(m))
}
