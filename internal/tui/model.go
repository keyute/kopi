package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/transcript"
)

const (
	// chromeRows is the rows below the transcript besides the input: the
	// blank row, the composer's two shaded rows and the status line.
	chromeRows = 4
	// busyRows is what a running turn adds: the working row and the blank
	// row below it.
	busyRows = 2
	flashFor = 2 * time.Second
)

// submitDoneMsg is the result of submitting or sending now the draft text.
type submitDoneMsg struct {
	text string
	err  error
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

	th            theme
	width, height int
	keys          keyMap
	composer      composer
	spinner       spinner.Model
	layout        statusLayout
	clip          clipboard
	imageDir      string

	tr    transcript.Transcript
	folds folds
	cache renderCache
	lines []line
	// offset is the first visible transcript line while not following.
	offset int
	follow bool
	// sel is the transcript selection, drawn in reverse video.
	sel    selection
	drag   drag
	clicks clicks
	// ticking is set while an autoScrollMsg is due, so one chain runs at a
	// time.
	ticking bool

	status app.Status
	busy   bool
	// submitting is set while a Submit or SendNow runs; submit and send now
	// do nothing meanwhile.
	submitting bool
	// unsent is input handed back while submitting, restored once the
	// result has settled the draft.
	unsent []event.Input
	// queued is the input queued for the running turn; pending is its rows.
	queued  []string
	pending []string
	// flashText is the status line's notice until flashMsg{flashSeq}.
	flashText string
	flashSeq  int
	// exitArmed is set by a cancel at idle with an empty draft; the next
	// cancel quits.
	exitArmed bool
	now       func() time.Time
	// turnStart is when the running turn's TurnStart was applied.
	turnStart time.Time
	pickVerb  func() verb
	// verb is the running or last turn's.
	verb verb
}

func newModel(ctx context.Context, c Controller, b *bridge, opts Options) *model {
	th, km := mocha(), newKeyMap(opts.Keys)
	m := &model{
		ctx: ctx, c: c, bridge: b, version: opts.Version,
		th: th, keys: km, composer: newComposer(th, km.newline), spinner: newSpinner(th),
		layout:   statusLayout{left: opts.StatusLeft, right: opts.StatusRight},
		clip:     systemClipboard{},
		imageDir: opts.ImageDir,
		folds:    newFolds(),
		follow:   true,
		status:   c.Status(),
		now:      time.Now,
		pickVerb: randomVerb,
		verb:     fallbackVerb,
	}
	m.composer.setHistory(c.History())
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.bridge.wait(), tea.RequestBackgroundColor)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sel = selection{}
		m.composer.resize(m.width, m.height)
		m.relayout()
	case tea.BackgroundColorMsg:
		if !msg.IsDark() {
			m.th = latte()
			m.sel = selection{}
			m.composer.style(m.th)
			m.spinner.Style = fg(m.th.accent)
			m.relayout()
		}
	case eventsMsg:
		return m, m.applyBatch(msg)
	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case autoScrollMsg:
		return m, m.autoScroll()
	case submitDoneMsg:
		defer m.refreshPending()
		return m, m.submitDone(msg)
	case interruptMsg:
		defer m.refreshPending()
		return m, m.cancel()
	case flashMsg:
		if msg.seq == m.flashSeq {
			m.flashText, m.exitArmed = "", false
		}
	case tea.KeyPressMsg:
		defer m.refreshPending()
		return m, m.key(msg)
	case tea.PasteMsg:
		defer m.refreshPending()
		return m, m.paste(msg.Content)
	case imagePastedMsg:
		defer m.refreshPending()
		return m, m.composer.insertImage(msg.path)
	case pasteFailedMsg:
		return m, m.flash("paste failed")
	}
	return m, nil
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	m.sel = selection{}
	km := &m.keys
	if key.Matches(msg, km.cancel) {
		return m.cancel()
	}
	if m.exitArmed {
		m.exitArmed, m.flashText = false, ""
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

// submit runs Submit off the update loop: /resume can block for seconds.
// The draft stays in the composer until the Controller accepts it.
func (m *model) submit() tea.Cmd {
	if strings.TrimSpace(m.composer.value()) == "" {
		return nil
	}
	return m.send(func(c Controller, ctx context.Context, text string, images []string) error {
		_, err := c.Submit(ctx, text, images...)
		return err
	})
}

// sendNow is submit through SendNow; an empty draft sends just the queue.
func (m *model) sendNow() tea.Cmd {
	return m.send(func(c Controller, ctx context.Context, text string, images []string) error {
		return c.SendNow(ctx, text, images...)
	})
}

func (m *model) send(call func(c Controller, ctx context.Context, text string, images []string) error) tea.Cmd {
	if m.submitting {
		return nil
	}
	m.submitting = true
	raw := m.composer.value()
	text, images := m.composer.expanded()
	ctx, c := m.ctx, m.c
	return func() tea.Msg {
		return submitDoneMsg{text: raw, err: call(c, ctx, text, images)}
	}
}

// submitDone clears the draft only if it is still what was submitted, and
// keeps it on any refusal so it can be fixed or sent again. Input handed
// back meanwhile then goes before what remains.
func (m *model) submitDone(msg submitDoneMsg) tea.Cmd {
	m.submitting = false
	// The Controller records refused commands too.
	m.composer.setHistory(m.c.History())
	var cmd tea.Cmd
	switch {
	case errors.Is(msg.err, app.ErrQuit):
		return tea.Quit
	case msg.err == nil:
		if m.composer.value() == msg.text {
			m.composer.reset()
		}
	case errors.Is(msg.err, app.ErrBusy):
		cmd = m.flash("busy switching sessions")
	default:
		m.tr.Apply(event.Error{Err: msg.err})
		m.relayout()
	}
	m.composer.restore(m.unsent)
	m.unsent = nil
	return cmd
}

// editQueue moves the queued input into the composer, before the draft.
func (m *model) editQueue() {
	m.composer.restore(m.c.TakeQueued())
	// QueueChanged follows; clearing now keeps a second press from taking again.
	m.queued = nil
	m.refreshPending()
}

// applyBatch applies events in order, refreshes the status when one of them
// can change it, and starts the spinner when a turn starts.
func (m *model) applyBatch(evs eventsMsg) tea.Cmd {
	wasBusy, refresh := m.busy, false
	pureThinking := len(evs) > 0
	for _, ev := range evs {
		if _, ok := ev.(event.ThinkingDelta); !ok {
			pureThinking = false
		}
		switch ev.(type) {
		case event.SessionChanged, event.ModeChanged, event.ToolEnd, event.TurnEnd:
			refresh = true
		}
		m.apply(ev)
	}
	if refresh {
		m.status = m.c.Status()
	}
	if !pureThinking {
		m.relayout()
	}
	cmds := []tea.Cmd{m.bridge.wait()}
	if m.busy && !wasBusy {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

func (m *model) apply(ev event.Event) {
	switch ev := ev.(type) {
	case event.SessionChanged:
		m.tr = transcript.Transcript{}
		if len(ev.History) > 0 {
			m.tr.Restore(ev.History)
		}
		m.folds = newFolds()
		m.cache.reset()
		m.sel = selection{}
		m.follow = true
		m.queued = nil
		return
	case event.QueueChanged:
		m.queued = ev.Pending
		return
	case event.TurnStart:
		m.busy = true
		m.turnStart = m.now()
		m.verb = m.pickVerb()
	case event.TurnEnd:
		m.tr.Apply(ev)
		// Only a turn seen starting picked a verb; the Turn block is last.
		if m.busy {
			blocks := m.tr.Blocks()
			m.folds.verbs[blocks[len(blocks)-1].Key()] = m.verb
		}
		m.busy = false
		// Restoring now would change the draft an in-flight submit compares
		// against.
		if m.submitting {
			m.unsent = append(m.unsent, ev.Unsent...)
		} else {
			m.composer.restore(ev.Unsent)
		}
		return
	}
	m.tr.Apply(ev)
}

// holdTop applies a fold change without moving the first visible line, so
// what was clicked stays on its row; following stops unless the view is
// still at the bottom.
func (m *model) holdTop(change func()) {
	top := m.top()
	m.sel = selection{}
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
	m.refreshPending()
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
	if m.drag.region != regionTranscript && m.sel.empty() {
		return
	}
	n := max(m.sel.anchor.line, m.sel.head.line) + 1
	sameText := func(a, b line) bool { return a.text == b.text }
	if !slices.EqualFunc(old[:min(n, len(old))], m.lines[:min(n, len(m.lines))], sameText) {
		m.sel = selection{}
		if m.drag.region == regionTranscript {
			m.drag = drag{}
		}
	}
}

// refreshPending updates queue geometry without rebuilding transcript lines.
func (m *model) refreshPending() {
	if m.width <= 0 {
		m.pending = nil
		return
	}
	budget := m.height - m.composer.height() - chromeRows
	if m.busy {
		budget -= busyRows
	}
	m.pending = pendingRows(m.queued, &m.keys, m.width, max(2, m.height/3), budget, m.th)
}

func (m *model) transcriptHeight() int {
	h := m.height - m.composer.height() - chromeRows - len(m.pending)
	if m.busy {
		h -= busyRows
	}
	return max(0, h)
}

// composerTop is the screen row of the composer's first text line, below
// the transcript, the blank row, the pending rows, the busy rows and the
// shaded row.
func (m *model) composerTop() int {
	top := m.transcriptHeight() + 2 + len(m.pending)
	if m.busy {
		top += busyRows
	}
	return top
}

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

// selected is line i with the part of it the selection covers reversed.
func (m *model) selected(i int) string {
	text := m.lines[i].text
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
	rows := make([]string, 0, m.height)
	for i := top; i < top+h; i++ {
		if i < len(m.lines) {
			rows = append(rows, m.selected(i))
		} else {
			rows = append(rows, "")
		}
	}
	rows = append(rows, "")
	rows = append(rows, m.pending...)
	if m.busy {
		rows = append(rows, workingRow(&m.spinner, m.verb, formatDuration(m.now().Sub(m.turnStart)), m.th), "")
	}
	rows = append(rows, m.composer.view(m.width, m.th)...)
	rows = append(rows, statusLine(m.status, m.layout, m.flashText, m.width, m.th))
	v.SetContent(strings.Join(rows, "\n"))
	return v
}
