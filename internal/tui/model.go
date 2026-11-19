package tui

import (
	"context"
	"errors"
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
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/transcript"
)

const (
	// chromeRows is the rows below the transcript besides the input: the
	// blank row, the composer's two shaded rows and the status line.
	chromeRows = 4
	// busyRows is what the working row adds: the row and the blank row
	// below it.
	busyRows = 2
	flashFor = 2 * time.Second
)

// geometry is the row heights and field areas arrange last computed, which
// View draws and the mouse hit-tests.
type geometry struct {
	transcript, menu, composerTop int
	fields                        []fieldArea
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
	home          string
	commands      statusCommands
	commandOut    map[string]string
	geo           geometry
	clip          clipboard
	imageDir      string
	// getenv and execProcess are os.Getenv and tea.ExecProcess, replaced in tests.
	getenv      func(string) string
	execProcess func(*exec.Cmd, tea.ExecCallback) tea.Cmd

	tr    transcript.Transcript
	folds folds
	cache renderCache
	lines []line
	// offset is the first visible transcript line while not following.
	offset int
	follow bool
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
	busy   bool
	// activity is the Controller's latest event.Activity text.
	activity string
	// queued is the input queued for the running turn; pending is its rows.
	queued       []string
	pending      []string
	pendingCache pendingCache
	// flashText is the status line's notice until flashMsg{flashSeq}.
	flashText string
	flashSeq  int
	// exitArmed is set by a cancel at idle with an empty draft; the next
	// cancel quits.
	exitArmed bool
	// turnStart is when the running turn's TurnStart was applied.
	turnStart time.Time
	// waitStart is when the root went idle with background work running,
	// zero otherwise.
	waitStart time.Time
	pickVerb  func() verb
	// verb is the running or last turn's.
	verb verb
	// approval is the call awaiting an answer, nil when none; dialog is its
	// prompt. tasks is the /tasks dialog, nil when closed; an approval closes
	// it. prompt is the shown dialog's rows, drawn in place of the composer.
	approval *event.ApprovalRequested
	// queued asks wait behind approval, in arrival order.
	approvalQueue []event.ApprovalRequested
	// login is the pending login question, whose prompt is dialog too.
	login *event.LoginRequested
	// question is the pending AskUserQuestion dialog, whose current page is
	// dialog too; plan is the pending ExitPlanMode dialog.
	question *questionDialog
	dialog   *dialog
	tasks    *taskDialog
	plan     *planDialog
	prompt   []string
	menu     menu
	// seen is the draft and cursor syncInput last reconciled.
	seen struct {
		value     string
		line, col int
	}
}

func newModel(ctx context.Context, c Controller, b *bridge, opts Options) *model {
	th, km := opts.Themes.dark, newKeyMap(opts.Keys)
	m := &model{
		ctx: ctx, c: c, bridge: b, version: opts.Version,
		th: th, themes: opts.Themes, keys: km, composer: newComposer(th, km.newline), spinner: newSpinner(th),
		layout: statusLayout{
			left: opts.StatusLine.Left, right: opts.StatusLine.Right,
			separator: opts.StatusLine.Separator, segments: opts.StatusLine.Segments,
		},
		commandOut: map[string]string{},
		clip:       systemClipboard{},
		imageDir:   opts.ImageDir,
		folds:      newFolds(),
		follow:     true,
		geo:        geometry{working: -1},
		status:     c.Status(),
		pickVerb:   randomVerb,
		verb:       fallbackVerb,
	}
	m.composer.setHistory(c.History())
	m.home, _ = os.UserHomeDir()
	m.getenv, m.execProcess = os.Getenv, tea.ExecProcess
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.bridge.wait(), tea.RequestBackgroundColor)
}

func (m *model) statusInput() statusInput {
	return statusInput{Status: m.status, notice: m.flashText, home: m.home, commands: m.commandOut}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	held := m.holding()
	cmd := m.update(msg)
	cmd = tea.Batch(cmd, m.syncInput(held))
	m.arrange()
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
	case tea.BackgroundColorMsg:
		if !msg.IsDark() {
			m.th = m.themes.light
			m.clearSelection()
			m.composer.style(m.th)
			m.spinner.Style = fg(m.th.accent)
			m.relayout()
		}
	case eventsMsg:
		return m.applyBatch(msg)
	case spinner.TickMsg:
		if _, shown := m.workingText(); !shown {
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
		m.editorDone(msg)
	case flashMsg:
		if msg.seq == m.flashSeq {
			m.flashText, m.exitArmed = "", false
		}
	case tea.KeyPressMsg:
		return m.key(msg)
	case clipboardTextMsg:
		if msg.field != nil {
			if m.dialog != nil && m.dialog.field() == msg.field {
				return m.dialog.paste(msg.text)
			}
			return nil
		}
		if m.approval == nil && m.question == nil && m.plan == nil && m.tasks == nil {
			return m.paste(msg.text)
		}
		return nil
	case tea.PasteMsg:
		switch {
		case m.dialog != nil:
			return m.dialog.paste(msg.Content)
		case m.tasks != nil:
			return nil
		}
		return m.paste(msg.Content)
	case imagePastedMsg:
		return m.composer.insertImage(msg.path)
	case pasteFailedMsg:
		return m.flash("paste failed")
	case candidatesMsg:
		m.loaded(msg)
	}
	return nil
}

// syncInput reconciles what is derived from the draft after any message that
// may have changed it. The menu follows once a composer hold ends, as well as
// on a change, so it never acts on a token from before the hold.
func (m *model) syncInput(wasHeld bool) tea.Cmd {
	value, line, col := m.composer.value(), m.composer.ta.Line(), m.composer.ta.Column()
	changed := value != m.seen.value || line != m.seen.line || col != m.seen.col
	if value != m.seen.value {
		m.composer.spans = m.c.Spans(value)
	}
	m.seen.value, m.seen.line, m.seen.col = value, line, col
	if m.holding() || (!changed && !wasHeld) {
		return nil
	}
	return m.followMenu()
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	m.clearSelection()
	km := &m.keys
	if key.Matches(msg, km.cancel) {
		return m.cancel()
	}
	if m.exitArmed {
		m.exitArmed, m.flashText = false, ""
	}
	if m.approval != nil {
		return m.approvalKey(msg)
	}
	if m.question != nil {
		return m.questionKey(msg)
	}
	if m.plan != nil {
		return m.planKey(msg)
	}
	if m.login != nil {
		return m.loginKey(msg)
	}
	if m.tasks != nil {
		return m.tasksKey(msg)
	}
	// Unconsumed, the key reaches the composer, which may bind it too.
	if key.Matches(msg, km.background) && m.c.Background() {
		return nil
	}
	if m.menuHeight() > 0 && !m.holding() {
		if cmd, ok := m.menuKey(msg); ok {
			return cmd
		}
	}
	switch {
	case key.Matches(msg, km.interrupt):
		m.c.Interrupt()
		return nil
	case key.Matches(msg, km.quit) && m.composer.empty():
		return tea.Quit
	case key.Matches(msg, km.pageUp):
		m.scroll(-m.transcriptHeight())
		return nil
	case key.Matches(msg, km.pageDown):
		m.scroll(m.transcriptHeight())
		return nil
	case key.Matches(msg, km.submit):
		return m.submit()
	case key.Matches(msg, km.sendNow):
		return m.sendNow()
	case key.Matches(msg, km.toggleMode):
		m.c.ToggleMode()
		return nil
	case key.Matches(msg, km.queueEdit) && len(m.queued) > 0 && m.composer.atTop():
		m.editQueue()
		return nil
	case key.Matches(msg, km.historyPrev) && m.composer.historyPrev():
		return nil
	case key.Matches(msg, km.historyNext) && m.composer.historyNext():
		return nil
	case key.Matches(msg, km.expandAll):
		m.holdTop(m.folds.toggleAll)
		return nil
	case key.Matches(msg, km.paste):
		return readClipboard(m.ctx, m.clip, m.imageDir)
	}
	return m.composer.update(msg)
}

// approvalKey goes to the prompt, which answers when an option is picked;
// interrupt rejects without a reason. Keys the prompt has no use for are
// swallowed. The prompt stays until its ApprovalEnded arrives.
func (m *model) approvalKey(msg tea.KeyPressMsg) tea.Cmd {
	token := m.approval.Token
	switch {
	case key.Matches(msg, m.keys.interrupt):
		m.c.Answer(token, policy.Decision{})
		return nil
	case key.Matches(msg, m.keys.paste) && m.dialog.field() != nil:
		return readClipboardText(m.clip, m.dialog.field())
	}
	picked, cmd := m.dialog.key(msg, &m.keys)
	if picked {
		opts := approvalOptions(m.approval, &m.keys)
		m.c.Answer(token, policy.Decision{Answer: opts[m.dialog.sel].answer, Reason: m.dialog.text()})
	}
	return cmd
}

// dropApprovals removes the asks drop selects from the shown one and the
// queue. When the shown one goes, the first remaining ask is shown next.
func (m *model) dropApprovals(drop func(event.ApprovalRequested) bool) {
	if m.approval == nil {
		return
	}
	shownGone := drop(*m.approval)
	var keep []event.ApprovalRequested
	if !shownGone {
		keep = append(keep, *m.approval)
	}
	for _, a := range m.approvalQueue {
		if !drop(a) {
			keep = append(keep, a)
		}
	}
	if !shownGone {
		m.approvalQueue = keep[1:]
		return
	}
	m.approval, m.dialog, m.approvalQueue = nil, nil, nil
	switch {
	case len(keep) > 0:
		next := keep[0]
		m.approval, m.dialog, m.approvalQueue = &next, newApprovalDialog(&next, &m.keys, m.th), keep[1:]
	case m.question != nil:
		m.dialog = m.question.current()
	case m.plan != nil:
		m.dialog = m.plan.d
	}
}

// endQuestion drops the pending questions and, unless an approval is shown
// over them, their dialog.
func (m *model) endQuestion() {
	if m.question == nil {
		return
	}
	m.question = nil
	if m.approval == nil {
		m.dialog = nil
	}
}

// loginKey goes to the login prompt like approvalKey: a pick answers with the
// chosen option's ID or the typed text, and interrupt cancels the login. The
// prompt stays until its LoginEnded arrives.
func (m *model) loginKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.interrupt):
		m.c.Interrupt()
		return nil
	case key.Matches(msg, m.keys.paste) && m.dialog.field() != nil:
		return readClipboardText(m.clip, m.dialog.field())
	}
	picked, cmd := m.dialog.key(msg, &m.keys)
	if picked {
		answer := m.dialog.text()
		if f := m.dialog.field(); f != nil && f.secret {
			answer = strings.NewReplacer("\r", "", "\n", "").Replace(answer)
		}
		if len(m.login.Options) > 0 {
			answer = m.login.Options[m.dialog.sel].ID
		}
		m.c.AnswerLogin(m.login.Token, answer)
		m.dialog.clear()
	}
	return cmd
}

// endLogin drops the pending question and its prompt.
func (m *model) endLogin() {
	if m.login != nil {
		m.dialog.clear()
		m.login, m.dialog = nil, nil
	}
}

// cancel is the cancel key and SIGINT: it interrupts a running turn, else
// clears the draft, else takes two in a row to quit.
func (m *model) cancel() tea.Cmd {
	switch {
	case m.c.Interrupt():
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

// flash shows text on the status line for flashFor.
func (m *model) flash(text string) tea.Cmd {
	m.flashSeq++
	m.flashText = text
	seq := m.flashSeq
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashMsg{seq} })
}

func (m *model) submit() tea.Cmd {
	if strings.TrimSpace(m.composer.value()) == "" {
		return nil
	}
	text, images := m.composer.expanded()
	_, err := m.c.Submit(m.ctx, text, images...)
	return m.submitted(err)
}

// sendNow is submit through SendNow; an empty draft sends just the queue.
func (m *model) sendNow() tea.Cmd {
	text, images := m.composer.expanded()
	return m.submitted(m.c.SendNow(m.ctx, text, images...))
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
	case errors.Is(err, app.ErrOpenPlan):
		m.composer.reset()
		return m.openEditor(m.c.PlanFile())
	case err == nil:
		m.composer.reset()
	case errors.Is(err, app.ErrBusy):
		return m.flash("busy opening a session")
	case errors.Is(err, app.ErrLoggingIn):
		return m.flash("a login is under way")
	default:
		m.tr.Apply(event.Error{Err: err})
		m.relayout()
	}
	return nil
}

// editQueue moves the queued input into the composer, before the draft.
func (m *model) editQueue() {
	m.composer.restore(m.c.TakeQueued())
	// QueueChanged follows; clearing now keeps a second press from taking again.
	m.queued = nil
}

// applyBatch applies events in order, refreshes the status when one of them
// can change it, and starts the spinner when the working row appears.
func (m *model) applyBatch(evs eventsMsg) tea.Cmd {
	_, wasShown := m.workingText()
	refresh := false
	pureThinking := len(evs) > 0
	for _, ev := range evs {
		if _, ok := ev.(event.ThinkingDelta); !ok {
			pureThinking = false
		}
		switch ev.(type) {
		case event.SessionChanged, event.ModeChanged, event.ToolEnd, event.TurnEnd, event.TasksChanged:
			refresh = true
		}
		if m.commands != nil {
			m.commands.Observe(ev)
		}
		m.apply(ev)
	}
	if refresh {
		m.status = m.c.Status()
	}
	m.syncWaiting()
	if !pureThinking {
		m.relayout()
	}
	cmds := []tea.Cmd{m.bridge.wait()}
	if _, shown := m.workingText(); shown && !wasShown {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

func (m *model) apply(ev event.Event) {
	switch ev := ev.(type) {
	case event.StatusCommandResult:
		if ev.Err != nil {
			delete(m.commandOut, ev.Name)
		} else {
			m.commandOut[ev.Name] = cleanStatusOutput(ev.Output)
		}
		return
	case event.SessionChanged:
		m.tr = transcript.Transcript{}
		if len(ev.History) > 0 {
			m.tr.Restore(ev.History)
		}
		m.folds = newFolds()
		m.cache.reset()
		m.clearSelection()
		m.follow = true
		m.queued = nil
		m.dropApprovals(func(event.ApprovalRequested) bool { return true })
		m.endQuestion()
		m.endPlan()
		m.tasks = nil
		return
	case event.TasksChanged:
		m.refreshTasks()
		return
	case event.QueueChanged:
		m.queued = ev.Pending
		return
	case event.Activity:
		m.activity = ev.Text
		return
	case event.ApprovalRequested:
		if m.approval != nil {
			m.approvalQueue = append(m.approvalQueue, ev)
			return
		}
		m.approval, m.dialog, m.tasks = &ev, newApprovalDialog(&ev, &m.keys, m.th), nil
		return
	case event.QuestionRequested:
		m.question, m.tasks = newQuestionDialog(ev, &m.keys, m.th), nil
		if m.approval == nil {
			m.dialog = m.question.current()
		}
		return
	case event.QuestionEnded:
		if m.question != nil && m.question.token == ev.Token {
			m.endQuestion()
		}
		return
	case event.PlanRequested:
		// A repeat after an edit keeps the dialog and what was typed in it.
		if m.plan == nil || m.plan.token != ev.Token {
			m.plan, m.tasks = newPlanDialog(ev, &m.keys, m.th), nil
			if m.approval == nil {
				m.dialog = m.plan.d
			}
		}
	case event.PlanEnded:
		if m.plan != nil && m.plan.token == ev.Token {
			m.endPlan()
		}
		return
	case event.LoginRequested:
		m.login, m.dialog, m.tasks = &ev, newLoginDialog(ev, &m.keys, m.th), nil
		return
	case event.LoginEnded:
		if m.login != nil && m.login.Token == ev.Token {
			m.endLogin()
		}
		return
	case event.ApprovalEnded:
		m.dropApprovals(func(a event.ApprovalRequested) bool { return a.Token == ev.Token })
		return
	case event.TurnStart:
		m.busy = true
		m.turnStart = time.Now()
		m.verb = m.pickVerb()
	case event.TurnEnd:
		m.tr.Apply(ev)
		// Only a turn seen starting picked a verb; the Turn block is last.
		if m.busy {
			blocks := m.tr.Blocks()
			m.folds.verbs[blocks[len(blocks)-1].Key()] = m.verb
		}
		m.busy = false
		m.dropApprovals(func(a event.ApprovalRequested) bool { return a.Agent == "" })
		m.endQuestion()
		m.endPlan()
		m.composer.restore(ev.Unsent)
		return
	}
	m.tr.Apply(ev)
}

// holdTop applies a fold change without moving the first visible line, so
// what was clicked stays on its row; following stops unless the view is
// still at the bottom.
func (m *model) holdTop(change func()) {
	top := m.top()
	m.clearSelection()
	change()
	m.relayout()
	m.offset = min(top, m.maxOffset())
	m.follow = m.offset == m.maxOffset()
}

// relayout rebuilds the transcript's lines: the header, then each block
// that draws anything with one blank line above it. A selection, which holds
// line indices, is dropped once any line up to its end changes.
func (m *model) relayout() {
	if m.width == 0 {
		return
	}
	blocks := m.tr.Blocks()
	m.folds.settle(blocks)
	old := m.lines
	m.lines = header(m.version, m.th)
	for _, b := range blocks {
		ls := m.cache.lines(b, m.folds.of(b), m.width, m.th)
		if len(ls) == 0 {
			continue
		}
		m.lines = append(m.lines, line{})
		m.lines = append(m.lines, ls...)
	}
	if m.selIn != regionTranscript {
		return
	}
	n := max(m.sel.anchor.line, m.sel.head.line) + 1
	sameText := func(a, b line) bool { return a.text == b.text }
	if !slices.EqualFunc(old[:min(n, len(old))], m.lines[:min(n, len(m.lines))], sameText) {
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
	switch {
	case m.approval != nil:
		// The prompt takes the composer's frame rows too.
		m.prompt = approvalRows(m.dialog, m.approval, m.width, max(2, m.height/3), free+2, m.th)
		input = len(m.prompt) - 2
	case m.question != nil:
		m.prompt = m.question.rows(m.width, max(2, m.height/3), free+2, m.th)
		input = len(m.prompt) - 2
	case m.plan != nil:
		m.prompt = planRows(m.dialog, m.width, max(2, m.height/3), free+2, m.th)
		input = len(m.prompt) - 2
	case m.login != nil:
		m.prompt = loginRows(m.dialog, m.width, max(2, m.height/3), free+2, m.th)
		input = len(m.prompt) - 2
	case m.tasks != nil:
		m.prompt = m.tasks.rows(m.width, max(2, m.height/3), free+2, m.th)
		input = len(m.prompt) - 2
	default:
		m.geo.menu = max(0, min(m.menu.want(), free-input))
	}
	free -= input + m.geo.menu
	m.pending = m.pendingCache.rows(m.queued, &m.keys, m.width, max(2, m.height/3), free, m.th)
	m.geo.transcript = max(0, free-len(m.pending))

	rows := append([]string{""}, m.pending...)
	if shown {
		m.geo.working = len(rows)
		rows = append(rows, workingRow(&m.spinner, working, m.workingHint(), m.th), "")
	}
	// The +1 is the shaded top row that the composer and the prompt both draw.
	m.geo.composerTop = m.geo.transcript + len(rows) + 1
	switch {
	case m.approval != nil || m.login != nil || m.question != nil || m.plan != nil:
		rows = append(rows, m.prompt...)
		if len(m.prompt) > 0 {
			// The prompt's first row is the composer's top shaded row; with
			// no rows, the areas are an earlier render's.
			for _, fa := range m.dialog.areas {
				// A secret is never selectable, so it has no area.
				if !fa.c.secret {
					fa.y += m.geo.composerTop - 1
					m.geo.fields = append(m.geo.fields, fa)
				}
			}
		}
	case m.tasks != nil:
		rows = append(rows, m.prompt...)
	default:
		rows = append(rows, m.composer.view(m.width, m.th)...)
		rows = append(rows, m.menuRows()...)
		m.geo.fields = []fieldArea{{c: &m.composer, y: m.geo.composerTop, h: m.composer.height()}}
	}
	rows = append(rows, statusLine(m.statusInput(), m.layout, m.width, m.th))
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
	return max(0, len(m.lines)-m.transcriptHeight())
}

// top is the first visible transcript line. It is derived rather than stored
// while following, so the composer growing keeps the bottom in view.
func (m *model) top() int {
	if m.follow {
		return m.maxOffset()
	}
	return min(m.offset, m.maxOffset())
}

// scroll moves the view by n lines; reaching the bottom resumes following.
func (m *model) scroll(n int) {
	m.offset = min(max(0, m.top()+n), m.maxOffset())
	m.follow = m.offset == m.maxOffset()
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
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion}
	if m.width == 0 || m.height == 0 {
		return v
	}
	h, top := m.transcriptHeight(), m.top()
	rows := make([]string, 0, h+len(m.below))
	for i := top; i < top+h; i++ {
		if i < len(m.lines) {
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
