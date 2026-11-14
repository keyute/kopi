package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/transcript"
)

const (
	wheelStep = 3
	// chromeRows is the rows below the transcript besides the input: the
	// blank row, the composer's two shaded rows and the status line.
	chromeRows = 4
	busyHint   = "esc to interrupt"
)

// submitDoneMsg is the result of submitting text.
type submitDoneMsg struct {
	text string
	err  error
}

// interruptMsg is an external SIGINT.
type interruptMsg struct{}

type model struct {
	// ctx is Run's context, passed to Submit.
	ctx     context.Context
	c       Controller
	bridge  *bridge
	version string

	th            theme
	width, height int
	composer      textarea.Model
	spinner       spinner.Model

	tr    transcript.Transcript
	folds folds
	cache renderCache
	lines []line
	// offset is the first visible transcript line while not following.
	offset int
	follow bool

	status app.Status
	busy   bool
	// submitting is set while a Submit runs; Enter does nothing meanwhile.
	submitting bool
	// hint is drawn on the working row after a prompt was refused as busy.
	hint string
	now  func() time.Time
	// turnStart is when the running turn's TurnStart was applied.
	turnStart time.Time
}

func newModel(ctx context.Context, c Controller, b *bridge, version string) *model {
	th := mocha()
	return &model{
		ctx: ctx, c: c, bridge: b, version: version,
		th: th, composer: newComposer(th), spinner: newSpinner(th),
		folds:  newFolds(),
		follow: true,
		status: c.Status(),
		now:    time.Now,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.bridge.wait(), tea.RequestBackgroundColor)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		resizeComposer(&m.composer, m.width, m.height)
		m.relayout()
	case tea.BackgroundColorMsg:
		if !msg.IsDark() {
			m.th = latte()
			styleComposer(&m.composer, m.th)
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
	case tea.MouseClickMsg:
		m.click(msg.Mouse())
	case submitDoneMsg:
		return m, m.submitDone(msg)
	case interruptMsg:
		return m, m.interruptOrQuit()
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll(-wheelStep)
		case tea.MouseWheelDown:
			m.scroll(wheelStep)
		}
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.PasteMsg:
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	m.hint = ""
	switch msg.String() {
	case "ctrl+c":
		return m.interruptOrQuit()
	case "esc":
		m.c.Interrupt()
		return nil
	case "pgup":
		m.scroll(-m.transcriptHeight())
		return nil
	case "pgdown":
		m.scroll(m.transcriptHeight())
		return nil
	case "enter":
		return m.submit()
	case "ctrl+o":
		m.holdTop(m.folds.toggleAll)
		return nil
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	return cmd
}

func (m *model) interruptOrQuit() tea.Cmd {
	if m.c.Interrupt() {
		return nil
	}
	return tea.Quit
}

// submit runs Submit off the update loop: /resume can block for seconds.
// The draft stays in the composer until the Controller accepts it.
func (m *model) submit() tea.Cmd {
	text := m.composer.Value()
	if m.submitting || strings.TrimSpace(text) == "" {
		return nil
	}
	m.submitting = true
	ctx, c := m.ctx, m.c
	return func() tea.Msg {
		_, err := c.Submit(ctx, text)
		return submitDoneMsg{text: text, err: err}
	}
}

// submitDone clears the draft only if it is still what was submitted, and
// keeps it on any refusal so it can be fixed or sent again.
func (m *model) submitDone(msg submitDoneMsg) tea.Cmd {
	m.submitting = false
	switch {
	case errors.Is(msg.err, app.ErrQuit):
		return tea.Quit
	case msg.err == nil:
		if m.composer.Value() == msg.text {
			m.composer.Reset()
		}
	case errors.Is(msg.err, app.ErrBusy):
		// Without a running turn (a session switch) there is no working row
		// to carry the hint; the kept draft is enough.
		if m.busy {
			m.hint = busyHint
		}
	default:
		m.tr.Apply(event.Error{Err: msg.err})
		m.relayout()
	}
	return nil
}

// applyBatch applies events in order, refreshes the status when one of them
// can change it, and starts the spinner when a turn starts.
func (m *model) applyBatch(evs eventsMsg) tea.Cmd {
	wasBusy, refresh := m.busy, false
	for _, ev := range evs {
		switch ev.(type) {
		case event.SessionChanged, event.ModeChanged, event.ToolEnd, event.TurnEnd:
			refresh = true
		}
		m.apply(ev)
	}
	if refresh {
		m.status = m.c.Status()
	}
	m.relayout()
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
		m.follow = true
		return
	case event.TurnStart:
		m.busy = true
		m.turnStart = m.now()
	case event.TurnEnd:
		m.busy = false
		m.hint = ""
	}
	m.tr.Apply(ev)
}

// click toggles the fold a left click in the transcript lands on. Clicks
// are inert while everything is shown.
func (m *model) click(mo tea.Mouse) {
	if mo.Button != tea.MouseLeft || mo.Y < 0 || mo.Y >= m.transcriptHeight() {
		return
	}
	i := m.top() + mo.Y
	if i >= len(m.lines) {
		return
	}
	h := m.lines[i].hit
	if h.kind == hitNone || m.folds.all {
		return
	}
	m.holdTop(func() { m.folds.click(h) })
}

// holdTop applies a fold change without moving the first visible line, so
// what was clicked stays on its row; following stops unless the view is
// still at the bottom.
func (m *model) holdTop(change func()) {
	top := m.top()
	change()
	m.relayout()
	m.offset = min(top, m.maxOffset())
	m.follow = m.offset == m.maxOffset()
}

// relayout rebuilds the transcript's lines: the header, then each block
// that draws anything with one blank line above it.
func (m *model) relayout() {
	if m.width == 0 {
		return
	}
	blocks := m.tr.Blocks()
	m.folds.settle(blocks)
	m.lines = header(m.version, m.th)
	for _, b := range blocks {
		ls := m.cache.lines(b, m.folds.of(b), m.width, m.th)
		if len(ls) == 0 {
			continue
		}
		m.lines = append(m.lines, line{})
		m.lines = append(m.lines, ls...)
	}
}

func (m *model) transcriptHeight() int {
	h := m.height - m.composer.Height() - chromeRows
	if m.busy {
		h--
	}
	return max(0, h)
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

func (m *model) View() tea.View {
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeCellMotion}
	if m.width == 0 || m.height == 0 {
		return v
	}
	h, top := m.transcriptHeight(), m.top()
	rows := make([]string, 0, m.height)
	for i := top; i < top+h; i++ {
		if i < len(m.lines) {
			rows = append(rows, m.lines[i].text)
		} else {
			rows = append(rows, "")
		}
	}
	rows = append(rows, "")
	if m.busy {
		rows = append(rows, workingRow(&m.spinner, formatDuration(m.now().Sub(m.turnStart)), m.hint, m.th))
	}
	rows = append(rows, composerView(&m.composer, m.width, m.th)...)
	rows = append(rows, statusLine(m.status, m.width, m.th))
	v.SetContent(strings.Join(rows, "\n"))
	return v
}
