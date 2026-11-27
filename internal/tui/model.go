package tui

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

const (
	// chromeRows is the rows below the transcript besides the input: the
	// blank row, the composer's two shaded rows and the status line, which
	// a prompt takes back.
	chromeRows = 4
	// busyRows is what the working row adds: the row and the blank row
	// below it.
	busyRows = 2
	flashFor = 2 * time.Second
	// barEvery is under Ghostty's removal of a progress bar 15 s after its last
	// report.
	barEvery = 10 * time.Second
)

// geometry is the row heights and field areas arrange last computed, which
// View draws and the mouse hit-tests.
type geometry struct {
	transcript, menu, composerTop int
	// agents is the agent rows drawn below the status line.
	agents int
	fields []fieldArea
	// working is the working row's index in model.below, -1 without one.
	working int
}

// fieldArea is where an editable field is drawn: its first text row y and
// column x on screen, and its h rows.
type fieldArea struct {
	c       *composer
	x, y, h int
}

// interruptMsg is an external SIGINT.
type interruptMsg struct{}

// flashMsg ends the flash numbered seq; a later flash outlives it.
type flashMsg struct{ seq int }

// hintMsg wakes the screen once an activity has lasted hintDelay.
type hintMsg struct{}

// barMsg is a tick of the progress bar's keepalive.
type barMsg struct{}

type model struct {
	// ctx is Run's context, passed to Submit.
	ctx     context.Context
	c       Controller
	bridge  *bridge
	version string
	// editing is set while an editor runs in the terminal, whose SIGINT is not
	// a cancel; the signal goroutine reads it.
	editing atomic.Bool

	th            theme
	themes        Themes
	width, height int
	keys          keyMap
	composer      composer
	spinner       spinner.Model
	layout        statusLayout
	subagentLine  subagentLayout
	// usesActivity and usesElapsed report whether subagentLine shows those segments.
	usesActivity, usesElapsed bool
	home                      string
	commands                  statusCommands
	commandOut                map[string]string
	added, removed            int
	geo                       geometry
	clip                      clipboard
	imageDir                  string
	// getenv, execProcess and raw are os.Getenv, tea.ExecProcess and tea.Raw,
	// replaced in tests.
	getenv      func(string) string
	execProcess func(*exec.Cmd, tea.ExecCallback) tea.Cmd
	raw         func(any) tea.Cmd
	terminal    config.Terminal
	// title is the session's name, "" when it has none.
	title string
	// away is set while the terminal reports it is not focused. unseen is the
	// done or error state a run left while it was, until focus returns or work
	// starts; ended is how the main session's last turn ended as a program state,
	// "" before one or when it was interrupted.
	// mainEnded records that a main turn ended in the update in progress.
	away      bool
	unseen    tea.ProgramState
	ended     tea.ProgramState
	mainEnded bool
	// barBeating is set while a barMsg is due, so one chain runs at a time;
	// barBeat is the bar's value, flipped on each so Bubble Tea writes it again.
	barBeating bool
	barBeat    int
	// readsStatus is set once the terminal answers the OSC 7501 query. records
	// holds the child records the terminal has, by id, with their titles.
	readsStatus    bool
	records        map[string]string
	recordsWriting bool

	// views holds each session's state by id, main's under app.Main; shown is
	// the id of the one drawn and acted on.
	views map[string]*view
	shown string
	// sel is the selection in the space selIn names, drawn in reverse video.
	sel   selection
	selIn region
	// below is the rows under the transcript as drawn.
	below  []line
	drag   drag
	clicks clicks
	// ticking is set while an autoScrollMsg is due, so one chain runs at a
	// time.
	ticking bool

	status app.Status
	// agents is the Controller's sessions as of the last refresh.
	agents []app.AgentStatus
	// workflows is the running workflows as of the last refresh.
	workflows []app.WorkflowStatus
	// workflowSel is the number of the agent last selected in each workflow's
	// dialog.
	workflowSel map[string]int
	// focus is the agent rows' key focus.
	focus agentFocus
	// agentFrom is the index of the first entry the agent rows' window shows.
	agentFrom int
	// activity is the Controller's latest event.Activity text.
	activity string
	// activitySince is when activity last went from empty to text.
	activitySince time.Time
	// pending is the shown view's queued rows.
	pending      []string
	pendingCache pendingCache
	// flashText is the notice until flashMsg{flashSeq}.
	flashText string
	flashSeq  int
	// exitArmed is set by a cancel at idle with an empty draft; the next
	// cancel quits.
	exitArmed bool
	pickVerb  func() verb
	// prompts are the open prompts, the front one first; prompt is the front
	// one's rows, drawn in place of the composer.
	prompts []held
	prompt  []string
	menu    menu
	// seen is the draft, shown session and cursor syncInput last reconciled.
	seen struct {
		value     string
		shown     string
		line, col int
	}
}

func newModel(ctx context.Context, c Controller, b *bridge, opts Options) *model {
	th, km := opts.Themes.dark, newKeyMap(opts.Keys)
	subagentLine := newSubagentLayout(opts.SubagentLine)
	var agentNames []string
	for _, f := range slices.Concat(subagentLine.left, subagentLine.right) {
		agentNames = append(agentNames, f.Segments()...)
	}
	m := &model{
		ctx: ctx, c: c, bridge: b, version: opts.Version,
		th: th, themes: opts.Themes, keys: km, composer: newComposer(th, km.newline), spinner: newSpinner(th),
		layout:       newStatusLayout(opts.StatusLine),
		subagentLine: subagentLine,
		usesActivity: slices.Contains(agentNames, "activity"),
		usesElapsed:  slices.Contains(agentNames, "elapsed"),
		commandOut:   map[string]string{},
		clip:         systemClipboard{},
		imageDir:     opts.ImageDir,
		views:        map[string]*view{app.Main: newView()},
		shown:        app.Main,
		geo:          geometry{working: -1},
		status:       c.Status(),
		agents:       c.Agents(),
		workflows:    c.Workflows(),
		workflowSel:  map[string]int{},
		pickVerb:     randomVerb,
		terminal:     opts.Terminal,
	}
	m.composer.setHistory(c.History())
	m.home, _ = os.UserHomeDir()
	m.getenv, m.execProcess, m.raw = os.Getenv, tea.ExecProcess, tea.Raw
	return m
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.bridge.wait(), tea.RequestBackgroundColor, m.raw(ansi.SetModeLightDark)}
	if m.terminal.Title {
		cmds = append(cmds, tea.RequestProgramStatusSupport)
	}
	return tea.Batch(cmds...)
}

func (m *model) statusInput() statusInput {
	in := statusInput{Status: m.status, notice: m.flashText, home: m.home, commands: m.commandOut, added: m.added, removed: m.removed}
	a, ok := m.agent(m.shown)
	if ok {
		in.Model, in.ContextTokens, in.ContextWindow = a.Model, a.ContextTokens, a.ContextWindow
		in.Effort, in.ToolCalls, in.Cost = a.Effort, a.ToolCalls, a.Cost
	}
	return in
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	held, was := m.holding(), m.attention()
	cmd := m.update(msg)
	cmd = tea.Batch(cmd, m.syncInput(held), m.attend(msg, was), m.syncRecords(), m.beat(), m.usageWake())
	m.syncFocus()
	m.arrange()
	m.syncFocus()
	return m, cmd
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.deselect()
		m.drag = drag{}
		m.composer.resize(m.width, m.height)
		m.relayout()
	case tea.ProgramStatusSupportMsg:
		m.readsStatus = true
	case recordsWrittenMsg:
		m.records = msg
		m.recordsWriting = false
	case uv.DarkColorSchemeEvent, uv.LightColorSchemeEvent:
		// The report's value is not used: terminals disagree on what it means
		// (Ghostty with a single theme always reports light), so the background decides.
		return tea.RequestBackgroundColor
	case tea.BackgroundColorMsg:
		// A malformed reply parses to a nil colour, which IsDark reports as dark.
		if msg.Color == nil {
			return nil
		}
		th := m.themes.light
		if msg.IsDark() {
			th = m.themes.dark
		}
		if th != m.th {
			m.th = th
			m.clearSelection()
			m.composer.style(m.th)
			m.spinner.Style = fg(m.th.accent)
			m.relayout()
		}
	case eventsMsg:
		return m.applyBatch(msg)
	case spinner.TickMsg:
		if !m.tickWanted() {
			return nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	case tea.MouseMsg:
		return m.mouse(msg)
	case autoScrollMsg:
		return m.autoScroll()
	case interruptMsg:
		return m.cancel()
	case editorDoneMsg:
		return m.editorDone(msg)
	case hintMsg:
		if m.tickWanted() {
			return m.spinner.Tick
		}
	case barMsg:
		m.barBeating = false
		m.barBeat = 1 - m.barBeat
	case flashMsg:
		if msg.seq == m.flashSeq {
			m.flashText, m.exitArmed = "", false
		}
	case tea.KeyPressMsg:
		return m.key(msg)
	case clipboardTextMsg:
		if msg.field != nil {
			if d := m.frontDialog(); d != nil && d.field() == msg.field {
				return d.paste(msg.text)
			}
			return nil
		}
		if m.front() == nil {
			return m.paste(msg.text)
		}
		return nil
	case tea.PasteMsg:
		if d := m.frontDialog(); d != nil {
			return d.paste(msg.Content)
		}
		return m.paste(msg.Content)
	case imagePastedMsg:
		m.focus = agentFocus{}
		return m.composer.insertImage(msg.path)
	case pasteFailedMsg:
		return m.flash("paste failed")
	case candidatesMsg:
		return m.loaded(msg)
	}
	return nil
}

// syncInput reconciles what is derived from the draft after any message that
// may have changed it. The menu follows once a composer hold ends, as well as
// on a change, so it never acts on a token from before the hold.
func (m *model) syncInput(wasHeld bool) tea.Cmd {
	value := m.composer.value()
	if value != m.seen.value || m.shown != m.seen.shown || m.composer.stale() {
		m.composer.setSpans(m.c.Spans(m.shown, value), m.th)
	}
	value = m.composer.value()
	line, col := m.composer.ta.Line(), m.composer.ta.Column()
	changed := value != m.seen.value || line != m.seen.line || col != m.seen.col
	m.seen.value, m.seen.shown, m.seen.line, m.seen.col = value, m.shown, line, col
	if m.holding() || (!changed && !wasHeld) {
		return nil
	}
	return m.followMenu()
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	m.clearSelection()
	km := &m.keys
	if m.front() == nil && key.Matches(msg, km.cancel) {
		m.focus = agentFocus{}
		return m.cancel()
	}
	if m.exitArmed {
		m.exitArmed, m.flashText = false, ""
	}
	switch {
	case key.Matches(msg, km.pageUp):
		m.pageFront(-1)
		return nil
	case key.Matches(msg, km.pageDown):
		m.pageFront(1)
		return nil
	}
	if front := m.front(); front != nil {
		// A key may open or close prompts, so it goes to a copy.
		h := *front
		d := h.dialog(m.width, m.th)
		d.warn = ""
		if key.Matches(msg, km.paste) {
			if f := d.field(); f != nil {
				return readClipboardText(m.clip, f)
			}
			return nil
		}
		cmd, keep := h.key(m, msg)
		if !keep {
			m.prompts = slices.DeleteFunc(m.prompts, func(x held) bool { return x.prompt == h.prompt })
		}
		return cmd
	}
	if m.focus.on {
		if cmd, ok := m.agentKey(msg); ok {
			return cmd
		}
	}
	// Unconsumed, the key reaches the composer, which may bind it too.
	if m.shown == app.Main && key.Matches(msg, km.background) && m.c.Background() {
		return nil
	}
	pendingNav := m.menu.open && m.menu.pending && m.navKey(msg)
	if (m.menuHeight() > 0 || pendingNav) && !m.holding() {
		if cmd, ok := m.menuKey(msg); ok {
			return cmd
		}
	}
	switch {
	case key.Matches(msg, km.interrupt):
		m.c.Interrupt(m.shown)
		return nil
	case key.Matches(msg, km.quit) && m.composer.empty():
		return tea.Quit
	case key.Matches(msg, km.submit):
		return m.submit()
	case key.Matches(msg, km.sendNow):
		return m.sendNow()
	case key.Matches(msg, km.toggleMode):
		m.c.ToggleMode()
		return nil
	case key.Matches(msg, km.queueEdit) && slices.ContainsFunc(m.view().queued, isPrompt) && m.composer.atTop():
		m.editQueue()
		return nil
	case key.Matches(msg, km.historyPrev) && m.composer.historyPrev():
		return nil
	case key.Matches(msg, km.historyNext) && m.composer.historyNext():
		return nil
	case key.Matches(msg, km.historyNext) && m.menuHeight() == 0 && m.composer.atBottom() && m.focusRows():
		return nil
	case key.Matches(msg, km.expandAll):
		m.holdTop(m.view().folds.toggleAll)
		return nil
	case key.Matches(msg, km.paste):
		return readClipboard(m.ctx, m.clip, m.imageDir)
	}
	return m.composer.update(msg)
}

// cancel is the cancel key and SIGINT: it interrupts a running turn, else
// clears the draft, else takes two in a row to quit.
func (m *model) cancel() tea.Cmd {
	switch {
	case m.c.Interrupt(m.shown):
		return nil
	case !m.composer.empty():
		m.composer.reset()
		return nil
	case m.exitArmed:
		return tea.Quit
	}
	m.exitArmed = true
	k := "interrupt"
	if ks := m.keys.cancel.Keys(); len(ks) > 0 {
		k = ks[0]
	}
	return m.flash(k + " again to exit")
}

// barWanted reports whether the progress bar shows: the option is on and the
// session is working.
func (m *model) barWanted() bool {
	return m.terminal.Progress && m.attention().State == tea.ProgramStateWorking
}

// beat starts the progress bar's keepalive when the bar shows and none is due;
// a due tick that finds the bar gone is not followed by another.
func (m *model) beat() tea.Cmd {
	if !m.barWanted() || m.barBeating {
		return nil
	}
	m.barBeating = true
	return tea.Tick(barEvery, func(time.Time) tea.Msg { return barMsg{} })
}

// flash shows text as the notice for flashFor: on the status line, or with the
// key hints of the prompt in front.
func (m *model) flash(text string) tea.Cmd {
	m.flashSeq++
	m.flashText = text
	seq := m.flashSeq
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashMsg{seq} })
}

// draft is the composer's text with pastes expanded. Only a draft that shows as a
// shell command is sent as one: a "!" that a collapsed paste brings in is not.
func (m *model) draft() (string, []string) {
	text, images := m.composer.expanded()
	if !m.composer.bang {
		text = app.AsPrompt(text)
	}
	return text, images
}

func (m *model) submit() tea.Cmd {
	if strings.TrimSpace(m.composer.value()) == "" {
		return nil
	}
	text, images := m.draft()
	_, err := m.c.Submit(m.ctx, m.shown, text, images...)
	return m.submitted(err)
}

// sendNow is submit through SendNow; an empty draft sends just the queue.
func (m *model) sendNow() tea.Cmd {
	text, images := m.draft()
	return m.submitted(m.c.SendNow(m.ctx, m.shown, text, images...))
}

// submitted clears the draft once the Controller accepts it, and keeps it on
// any refusal so it can be fixed or sent again.
func (m *model) submitted(err error) tea.Cmd {
	// The Controller records refused commands too.
	m.composer.setHistory(m.c.History())
	switch {
	case errors.Is(err, app.ErrQuit):
		return tea.Quit
	case errors.Is(err, app.ErrShowTasks):
		m.composer.reset()
		return m.showTasks()
	case errors.Is(err, app.ErrShowUsage):
		m.composer.reset()
		return m.showUsage()
	case errors.Is(err, app.ErrOpenPlan):
		m.composer.reset()
		return m.openEditor(m.c.PlanFile())
	case err == nil:
		m.composer.reset()
	case errors.Is(err, app.ErrBusy):
		return m.flash("busy switching the session or model")
	case errors.Is(err, app.ErrLoggingIn):
		return m.flash("a login is under way")
	default:
		m.view().tr.Apply(event.Error{Err: err})
		m.relayout()
	}
	return nil
}

// editQueue moves the queued input into the composer, before the draft.
func (m *model) editQueue() {
	m.composer.restore(m.c.TakeQueued(m.shown))
	// QueueChanged follows; clearing now keeps a second press from taking again.
	m.view().queued = slices.DeleteFunc(m.view().queued, isPrompt)
}

func isPrompt(q event.Queued) bool { return q.Command == nil }

// applyBatch applies events in order, refreshes the status when one of them
// can change it, and starts the spinner when the working row appears.
func (m *model) applyBatch(evs eventsMsg) tea.Cmd {
	wasShown, wasSince := m.tickWanted(), m.activitySince
	refresh, redraw, respan := false, false, false
	dropped := 0
	pureThinking := len(evs) > 0
	for _, ev := range evs {
		_, inner := unwrap(ev)
		if _, ok := inner.(event.ThinkingDelta); !ok {
			pureThinking = false
		}
		switch inner.(type) {
		case event.SessionChanged, event.ModeChanged, event.ModelChanged, event.ToolEnd, event.TurnEnd, event.TasksChanged, event.LimitsChanged, event.ToolsChanged, event.EffortChanged, event.SideQuestionEnded:
			refresh = true
		}
		switch inner.(type) {
		case event.SessionChanged, event.EffortChanged, event.ModelChanged:
			respan = true
		}
		if u, ok := inner.(event.Unsent); ok {
			dropped += u.Dropped
		}
		if m.commands != nil {
			m.commands.Observe(ev)
		}
		if m.apply(ev) == m.view() {
			redraw = true
		}
	}
	if refresh {
		m.status, m.agents, m.workflows = m.c.Status(), m.c.Agents(), m.c.Workflows()
	}
	if respan {
		m.composer.setSpans(m.c.Spans(m.shown, m.composer.value()), m.th)
	}
	if redraw && !pureThinking {
		m.relayout()
	}
	cmds := []tea.Cmd{m.bridge.wait()}
	if dropped > 0 {
		cmds = append(cmds, m.flash(droppedText(dropped)))
	}
	if m.tickWanted() && !wasShown {
		cmds = append(cmds, m.spinner.Tick)
	}
	if m.activity != "" && !m.activitySince.Equal(wasSince) {
		cmds = append(cmds, tea.Tick(hintDelay, func(time.Time) tea.Msg { return hintMsg{} }))
	}
	return tea.Batch(cmds...)
}

func droppedText(n int) string {
	return "dropped " + countText(n, "queued command")
}

// unwrap is the session an event belongs to and the event itself; a bare
// event is the main session's.
func unwrap(ev event.Event) (string, event.Event) {
	if s, ok := ev.(event.Session); ok {
		return s.ID, s.Event
	}
	return app.Main, ev
}

// apply routes an event: the global ones act on the model, the rest on their
// session's view, which it returns; nil when no view changed.
func (m *model) apply(ev event.Event) *view {
	id, ev := unwrap(ev)
	m.sweep(ev)
	switch ev := ev.(type) {
	case event.StatusCommandResult:
		if ev.Err != nil {
			delete(m.commandOut, ev.Name)
		} else {
			m.commandOut[ev.Name] = cleanStatusOutput(ev.Output)
		}
		return nil
	case event.Changes:
		m.added, m.removed = ev.Added, ev.Removed
		return nil
	case event.SessionNamed:
		m.title = ev.Name
		return nil
	case event.SessionChanged:
		old := m.views[app.Main]
		unsent := old.unsent
		// A turn running across the switch keeps its working row.
		main := newView()
		main.busy, main.turnStart, main.verb = old.busy, old.turnStart, old.verb
		delete(m.views, app.Main)
		for _, a := range m.agents {
			if v := m.views[a.ID]; v != nil {
				unsent = append(unsent, v.unsent...)
				delete(m.views, a.ID)
			}
		}
		// m.agents is the controller's list now: a switch has already dropped its
		// subagents when an earlier batch refreshed it.
		for _, id := range slices.Sorted(maps.Keys(m.views)) {
			unsent = append(unsent, m.views[id].unsent...)
		}
		m.views, m.shown = map[string]*view{app.Main: main}, app.Main
		m.title = ev.Name
		m.unseen, m.ended, m.mainEnded = "", "", false
		m.composer.restore(unsent)
		if len(ev.History) > 0 {
			m.view().tr.Restore(ev.History)
		}
		m.clearSelection()
		m.focus = agentFocus{}
		clear(m.workflowSel)
		return m.view()
	case event.SessionClosed:
		// Only a shown view stays, for the human to read until they leave it.
		if v := m.views[ev.ID]; v != nil {
			if ev.ID == m.shown {
				v.closed = true
			} else {
				delete(m.views, ev.ID)
			}
		}
		return nil
	case event.TasksChanged, event.LimitsChanged, event.ToolsChanged, event.EffortChanged, event.ModelChanged:
		return nil
	case event.ModeChanged:
		return nil
	case event.Activity:
		if m.activity == "" && ev.Text != "" {
			m.activitySince = time.Now()
		}
		m.activity = ev.Text
		return nil
	case event.ApprovalRequested:
		m.open(promptApproval, newApprovalDialog(&ev, &m.keys, m.th))
		return nil
	case event.QuestionRequested:
		m.open(promptQuestion, newQuestionDialog(ev, &m.keys, m.th))
		return nil
	case event.PlanRequested:
		// A repeat after an edit keeps the dialog and what was typed in it.
		if !m.owned(ev) {
			m.open(promptPlan, newPlanDialog(ev, &m.keys, m.th))
			// The plan is the body of a row in the main transcript and the dialog takes every key.
			if m.shown != app.Main {
				m.show(app.Main)
			}
		}
	case event.LoginRequested:
		m.open(promptLogin, newLoginDialog(ev, &m.keys, m.th))
		return nil
	case event.DirRequested:
		m.open(promptDir, newDirDialog(ev, &m.keys))
		return nil
	case event.SideQuestionAsked:
		m.open(promptBtw, newBtwDialog(ev, &m.keys))
		return nil
	case event.QuestionEnded, event.PlanEnded, event.LoginEnded, event.DirEnded, event.ApprovalEnded, event.SideQuestionEnded:
		return nil
	}
	v := m.viewOf(id)
	if v.closed {
		return nil
	}
	switch ev := ev.(type) {
	case event.QueueChanged:
		v.queued = ev.Pending
		return v
	case event.Unsent:
		if id == m.shown {
			m.composer.restore(ev.Inputs)
		} else {
			v.unsent = append(v.unsent, ev.Inputs...)
		}
		return nil
	case event.TurnStart:
		if id == app.Main {
			m.unseen = ""
		}
		v.busy = true
		v.turnStart = time.Now()
		v.verb = m.pickVerb()
	case event.TurnEnd:
		v.tr.Apply(ev)
		// Only a turn seen starting picked a verb; the Turn block is last.
		if v.busy {
			blocks := v.tr.Blocks()
			v.folds.verbs[blocks[len(blocks)-1].Key()] = v.verb
		}
		v.busy = false
		if id == app.Main {
			m.ended, m.mainEnded = "", true
			switch ev.Outcome.Status {
			case event.Completed:
				m.ended = tea.ProgramStateDone
			case event.Failed:
				m.ended = tea.ProgramStateError
			}
		}
		if id == m.shown {
			m.composer.restore(ev.Unsent)
		} else {
			v.unsent = append(v.unsent, ev.Unsent...)
		}
		return v
	}
	v.tr.Apply(ev)
	return v
}

// holdTop applies a fold change without moving the first visible line, so
// what was clicked stays on its row; following stops unless the view is
// still at the bottom.
func (m *model) holdTop(change func()) {
	v := m.view()
	top := m.top()
	m.clearSelection()
	change()
	m.relayout()
	v.offset = min(top, m.maxOffset())
	v.follow = v.offset == m.maxOffset()
}

// relayout rebuilds the transcript's lines: the header, then each block
// that draws anything with one blank line above it. A selection, which holds
// line indices, is dropped once any line up to its end changes.
func (m *model) relayout() {
	if m.width == 0 {
		return
	}
	v := m.view()
	blocks := v.tr.Blocks()
	v.folds.settle(blocks)
	spans := func(t string) []app.Span { return m.c.PromptSpans(m.shown, t) }
	old := v.lines
	v.lines = header(m.version, m.th)
	for _, b := range blocks {
		ls := v.cache.lines(b, v.folds.of(b), spans, m.width, m.th)
		if len(ls) == 0 {
			continue
		}
		v.lines = append(v.lines, line{})
		v.lines = append(v.lines, ls...)
	}
	if m.selIn != regionTranscript {
		return
	}
	n := max(m.sel.anchor.line, m.sel.head.line) + 1
	sameText := func(a, b line) bool { return a.text == b.text }
	if !slices.EqualFunc(old[:min(n, len(old))], v.lines[:min(n, len(v.lines))], sameText) {
		m.clearSelection()
	}
}

// arrange computes every row height below the transcript, and the prompt's
// and queue's rows, for the state Update leaves; it is the only place they are
// computed.
// A lower-row selection is dropped once the row count changes or a row it
// covers reads differently; a row that was and is the working row is skipped,
// so the spinner's ticks keep a selection crossing it.
func (m *model) arrange() {
	old, wasWorking := m.below, m.geo.working
	m.place()
	if m.drag.region == regionTranscript && m.transcriptHeight() == 0 {
		m.clearSelection()
	}
	if m.selIn != regionRows {
		return
	}
	if len(old) != len(m.below) {
		m.clearSelection()
		return
	}
	from, to := m.sel.ordered()
	for i := from.line; i <= to.line; i++ {
		if (i != wasWorking || i != m.geo.working) && ansi.Strip(old[i].text) != ansi.Strip(m.below[i].text) {
			m.clearSelection()
			return
		}
	}
}

// place computes the heights and field areas and builds the lower rows.
func (m *model) place() {
	m.geo, m.prompt, m.pending, m.below = geometry{working: -1}, nil, nil, nil
	if m.width <= 0 {
		return
	}
	free := m.height - chromeRows
	working, shown := m.workingText()
	if shown {
		free -= busyRows
	}
	input := m.composer.height()
	var d *dialog
	if front := m.front(); front != nil {
		free++
		d = front.dialog(m.width, m.th)
		d.scrollHint, d.notice = scrollHint(&m.keys), m.flashText
		m.prompt = d.rows(m.width, free+2, max(2, m.height/3), m.th)
		// The prompt takes the composer's frame rows too.
		input = len(m.prompt) - 2
	} else {
		m.geo.menu = max(0, min(m.menu.want(), free-input))
	}
	free -= input + m.geo.menu
	m.pendingCache.tail = nil
	m.pendingCache.backgroundable = m.front() == nil
	m.pendingCache.stoppable = m.pendingCache.backgroundable && m.shown == app.Main && !m.view().busy && (m.menuHeight() == 0 || m.holding())
	if m.queuedRunning() {
		m.pendingCache.tail = m.c.CommandOutput
	}
	m.pending = m.pendingCache.rows(m.view().queued, m.shown, func(t string) []app.Span { return m.c.PromptSpans(m.shown, t) }, &m.keys, m.width, max(2, m.height/3), free, m.th)
	m.geo.transcript = max(0, free-len(m.pending))
	// The rows take only what is left of the transcript.
	var agentRows []string
	if d == nil {
		agentRows = m.agentRows(m.geo.transcript)
		m.geo.agents = len(agentRows)
		m.geo.transcript -= m.geo.agents
	}

	rows := append([]string{""}, m.pending...)
	if shown {
		m.geo.working = len(rows)
		rows = append(rows, workingRow(&m.spinner, working, m.workingHint(), m.th), "")
	}
	// The +1 is the shaded top row that the composer and the prompt both draw.
	m.geo.composerTop = m.geo.transcript + len(rows) + 1
	if d != nil {
		rows = append(rows, m.prompt...)
		// The prompt's first row is the composer's top shaded row.
		for _, fa := range d.areas {
			// A secret is never selectable, so it has no area.
			if !fa.c.secret {
				fa.y += m.geo.composerTop - 1
				m.geo.fields = append(m.geo.fields, fa)
			}
		}
	} else {
		rows = append(rows, m.composer.view(m.width, m.th)...)
		rows = append(rows, m.menuRows()...)
		m.geo.fields = []fieldArea{{c: &m.composer, y: m.geo.composerTop, h: m.composer.height()}}
	}
	if d == nil {
		rows = append(rows, statusLine(m.statusInput(), m.layout, m.width, m.th))
		rows = append(rows, agentRows...)
	}
	m.below = make([]line, len(rows))
	for i, r := range rows {
		m.below[i] = line{text: r}
	}
}

func (m *model) transcriptHeight() int { return m.geo.transcript }

// composerTop is the screen row of the composer's first text line, below
// the transcript, the blank row, the pending rows, the busy rows and the
// shaded row.
func (m *model) composerTop() int { return m.geo.composerTop }

func (m *model) maxOffset() int {
	return max(0, len(m.view().lines)-m.transcriptHeight())
}

// top is the first visible transcript line. It is derived rather than stored
// while following, so the composer growing keeps the bottom in view.
func (m *model) top() int {
	v := m.view()
	if v.follow {
		return m.maxOffset()
	}
	return min(v.offset, m.maxOffset())
}

// scroll moves the view by n lines; reaching the bottom resumes following.
func (m *model) scroll(n int) {
	v := m.view()
	v.offset = min(max(0, m.top()+n), m.maxOffset())
	v.follow = v.offset == m.maxOffset()
}

// selected is line i of space r with the part of it the selection covers
// reversed.
func (m *model) selected(r region, i int) string {
	lines, _ := m.space(r)
	text := lines[i].text
	if m.selIn != r {
		return text
	}
	if from, to := m.sel.span(i, ansi.StringWidth(text)); from < to {
		return lipgloss.StyleRanges(text, lipgloss.NewRange(from, to, lipgloss.NewStyle().Reverse(true)))
	}
	return text
}

func (m *model) View() tea.View {
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion, ReportFocus: true}
	attention := m.attention()
	status := m.programStatus(attention)
	v.ProgramStatus = &status
	if m.terminal.Title {
		v.WindowTitle = m.terminalTitle(attention.State)
	}
	if m.terminal.Progress && attention.State == tea.ProgramStateWorking {
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, m.barBeat)
	}
	if m.width == 0 || m.height == 0 {
		return v
	}
	lines := m.view().lines
	h, top := m.transcriptHeight(), m.top()
	rows := make([]string, 0, h+len(m.below))
	for i := top; i < top+h; i++ {
		if i < len(lines) {
			rows = append(rows, m.selected(regionTranscript, i))
		} else {
			rows = append(rows, "")
		}
	}
	for i := range m.below {
		rows = append(rows, m.selected(regionRows, i))
	}
	v.SetContent(strings.Join(rows, "\n"))
	return v
}
