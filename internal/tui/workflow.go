package tui

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

const (
	// barCells is the width of a workflow's progress bar.
	barCells = 16
	// descriptionFloor is the columns of a workflow's description its footer row
	// keeps before the bar goes.
	descriptionFloor = 12
)

// progressBar is the workflow's calls as barCells cells, stacked done, failed,
// running and queued; the ends are proportional to the counts.
func progressBar(s app.WorkflowStatus, th theme) string {
	n := workflowCalls(s)
	if n == 0 {
		return fg(th.dim).Render(strings.Repeat("▱", barCells))
	}
	var b strings.Builder
	cum, drawn := 0, 0
	for _, seg := range []struct {
		n     int
		glyph string
		c     color.Color
	}{{s.Done, "▰", th.success}, {s.Failed, "▰", th.error}, {s.Running, "▰", th.accent}, {s.Queued, "▱", th.dim}} {
		cum += seg.n
		end := cum * barCells / n
		if end > drawn {
			b.WriteString(fg(seg.c).Render(strings.Repeat(seg.glyph, end-drawn)))
			drawn = end
		}
	}
	return b.String()
}

// workflowCalls is all the workflow's calls, whatever their state.
func workflowCalls(s app.WorkflowStatus) int {
	return s.Done + s.Failed + s.Running + s.Queued
}

// failedSuffix is " · 1 failed", empty when none did.
func failedSuffix(failed int) string {
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d failed", failed)
}

// workflowCounts is "5/9", done of all calls, and " · 1 failed" when any call
// ended without completing.
func workflowCounts(s app.WorkflowStatus) string {
	return fmt.Sprintf("%d/%d", s.Done, workflowCalls(s)) + failedSuffix(s.Failed)
}

// workflowRow is a running workflow's agent row. The description shrinks to
// descriptionFloor first, then the bar is dropped, then the row is truncated.
func (m *model) workflowRow(w app.WorkflowStatus) string {
	cursor := m.focus.on && w.ID == m.focus.id
	gutter := "  "
	if cursor {
		gutter = cursorMark
	}
	paint := func(s string) string {
		if !cursor || s == "" {
			return s
		}
		return fg(m.th.accent).Render(s)
	}
	rest := []string{workflowCounts(w)}
	if w.Phase != "" {
		rest = append(rest, oneLine(w.Phase))
	}
	rest = append(rest, formatDuration(time.Since(w.Started)))
	head := paint(gutter + "○ workflow › ")
	desc := oneLine(w.Description)
	need := min(lipgloss.Width(desc), descriptionFloor)
	var row string
	for _, withBar := range []bool{true, false} {
		tail := "  " + paint(strings.Join(rest, " · "))
		if withBar {
			tail = "  " + progressBar(w, m.th) + tail
		}
		room := m.width - lipgloss.Width(head) - lipgloss.Width(tail)
		if room >= need || !withBar {
			row = head + paint(cut(desc, max(room, need))) + tail
			break
		}
	}
	return ansi.Truncate(row, m.width, "")
}

// workflowDialog is the progress dialog of one workflow run, drawn in the
// composer's slot: its agents grouped by phase, or while watching one agent
// only the key hint, its transcript showing in the rows above.
type workflowDialog struct {
	m  *model
	d  *dialog
	id string
	// stale is set when an event may have changed the run; the next dialog call
	// reads it again.
	stale  bool
	run    app.WorkflowStatus
	agents []app.WorkflowAgent
	// order is the index in agents of each option; options' drawn rows are
	// subslices of rows.
	order []int
	rows  []string
	head  [2]string
	cache map[int]drawnAgent
	// width, th and at are what the rows were drawn for; at is the displayed
	// second, which only running rows and the header depend on.
	width int
	th    theme
	at    time.Time
	// clock is whether the header's elapsed time is still counting.
	clock bool

	watching bool
	// prev is the view shown before watching began.
	prev string
	// hint is the dialog shown while watching, and watchWhat the watched
	// agent's agentWhat.
	hint      *dialog
	watchWhat string
	// watchID is the watched agent's session.
	watchID string
}

// drawnAgent is an agent's row as drawn for the agent's state at one second.
type drawnAgent struct {
	a    app.WorkflowAgent
	at   time.Time
	text string
}

// openWorkflow opens the progress dialog of run id on the agent last selected
// in it; false when the Controller does not know the run.
func (m *model) openWorkflow(id string) bool {
	run, agents, ok := m.c.Workflow(id)
	if !ok {
		return false
	}
	w := &workflowDialog{
		m: m, d: &dialog{}, id: id, run: run, agents: agents,
		cache: map[int]drawnAgent{}, hint: &dialog{},
	}
	w.d.head = w.head[:]
	m.open(promptWorkflow, w)
	return true
}

func (w *workflowDialog) dialog(width int, th theme) *dialog {
	if w.watching {
		w.hint.note = cut("watching "+w.watchWhat, width-2)
		return w.hint
	}
	w.sync(width, th)
	w.d.hints = w.foot()
	return w.d
}

// read takes a new snapshot of the run.
func (w *workflowDialog) read() {
	w.stale = false
	run, agents, ok := w.m.c.Workflow(w.id)
	if !ok {
		return
	}
	w.run, w.agents = run, agents
}

// findAgent is the index of call n in agents, -1 when there is none.
func findAgent(agents []app.WorkflowAgent, n int) int {
	return slices.IndexFunc(agents, func(a app.WorkflowAgent) bool { return a.N == n })
}

// sync brings the rows up to date with the snapshot, width, theme and second.
// A frame where none changed does no work.
func (w *workflowDialog) sync(width int, th theme) {
	at := time.Now().Truncate(time.Second)
	resized := width != w.width || th != w.th
	if !resized && !w.stale && (!w.clock || at.Equal(w.at)) {
		return
	}
	if resized {
		clear(w.cache)
		w.width, w.th = width, th
	}
	if w.stale {
		w.read()
	}
	w.at = at
	w.clock = w.run.Ended.IsZero()
	w.build()
}

// build draws the header and the agents' rows that changed, and groups them:
// agents with no phase, then each phase in the order of its first agent.
func (w *workflowDialog) build() {
	type group struct {
		phase string
		idx   []int
		done  int
	}
	var phases []*group
	byPhase := map[string]*group{}
	var plain []int
	for i, a := range w.agents {
		running := a.State == tool.TaskRunning
		if c, ok := w.cache[a.N]; !ok || c.a != a || (running && c.at != w.at) {
			w.cache[a.N] = drawnAgent{a: a, at: w.at, text: w.drawAgent(a)}
		}
		if a.Phase == "" {
			plain = append(plain, i)
			continue
		}
		g := byPhase[a.Phase]
		if g == nil {
			g = &group{phase: a.Phase}
			byPhase[a.Phase], phases = g, append(phases, g)
		}
		g.idx = append(g.idx, i)
		if a.State == tool.TaskCompleted {
			g.done++
		}
	}

	w.rows, w.order = w.rows[:0], w.order[:0]
	var starts []int
	add := func(i int, heading string) {
		starts = append(starts, len(w.rows))
		if heading != "" {
			w.rows = append(w.rows, heading)
		}
		w.rows = append(w.rows, w.cache[w.agents[i].N].text)
		w.order = append(w.order, i)
	}
	for _, i := range plain {
		add(i, "")
	}
	for _, g := range phases {
		for j, i := range g.idx {
			heading := ""
			if j == 0 {
				heading = w.heading(g.phase, g.done, len(g.idx))
			}
			add(i, heading)
		}
	}
	d := w.d
	d.options = slices.Grow(d.options[:0], len(w.order))[:len(w.order)]
	for j := range d.options {
		end := len(w.rows)
		if j+1 < len(starts) {
			end = starts[j+1]
		}
		d.options[j] = option{drawn: w.rows[starts[j]:end:end]}
	}
	d.sel = 0
	sel := w.m.workflowSel[w.id]
	if j := slices.IndexFunc(w.order, func(i int) bool { return w.agents[i].N == sel }); j >= 0 {
		d.sel = j
	}
	if len(w.order) > 0 {
		w.m.workflowSel[w.id] = w.agents[w.order[d.sel]].N
	}
	w.header()
}

// header draws the two head rows: the title, then the bar, counts, how the run
// ended if it failed or was stopped, and the elapsed time.
func (w *workflowDialog) header() {
	run := w.run
	text := fg(w.th.text)
	w.head[0] = "  " + text.Bold(true).Render(cut("Workflow › "+oneLine(run.Description), w.width-2))
	parts := []string{text.Render(workflowCounts(run))}
	if run.State == tool.TaskFailed || run.State == tool.TaskStopped {
		_, word := taskTone(run.State).styles(w.th)
		parts = append(parts, word.Render(string(run.State)))
	}
	end := run.Ended
	if end.IsZero() {
		end = w.at
	}
	parts = append(parts, text.Render(formatDuration(max(0, end.Sub(run.Started)))))
	w.head[1] = ansi.Truncate("  "+progressBar(run, w.th)+"  "+strings.Join(parts, text.Render(" · ")), w.width, "")
}

func (w *workflowDialog) heading(phase string, done, total int) string {
	row := "  " + fg(w.th.text).Bold(true).Render(oneLine(phase)) + fg(w.th.muted).Render(fmt.Sprintf(" · %d/%d", done, total))
	return ansi.Truncate(row, w.width, "…")
}

// agentWhat is what agent a is: "reviewer › label", or "reviewer #3" without a label.
func agentWhat(a app.WorkflowAgent) string {
	name := cmp.Or(oneLine(a.Agent), "agent")
	if label := oneLine(a.Description); label != "" {
		return name + " › " + label
	}
	return name + " #" + strconv.Itoa(a.N)
}

// stateBullet is the bullet of a list row in the colour of state.
func stateBullet(state tool.TaskStatus, th theme) string {
	if state == app.WorkflowQueued {
		return fg(th.dim).Render("•")
	}
	bullet, _ := taskTone(state).styles(th)
	return bullet.Render("•")
}

// stateRow is a list row of width cells: lead, what the row is, cut to leave
// meta whole, and meta flush right in the muted colour.
func stateRow(lead, what, meta string, width int, th theme) string {
	text := fg(th.text)
	if meta == "" {
		return lead + text.Render(cut(what, width-lipgloss.Width(lead)))
	}
	room := width - lipgloss.Width(lead) - lipgloss.Width(meta)
	what = cut(what, room-2)
	gap := max(2, room-lipgloss.Width(what))
	return ansi.Truncate(lead+text.Render(what)+strings.Repeat(" ", gap)+fg(th.muted).Render(meta), width, "")
}

// drawAgent is agent a's row: its bullet in the colour of its state, what it
// is, and flush right its state, elapsed time and context.
func (w *workflowDialog) drawAgent(a app.WorkflowAgent) string {
	indent := 2
	if a.Phase != "" {
		indent = 4
	}
	what := agentWhat(a)
	var meta []string
	if a.State != tool.TaskRunning && a.State != tool.TaskCompleted {
		meta = append(meta, string(a.State))
	}
	if !a.Started.IsZero() {
		end := a.Ended
		if a.State == tool.TaskRunning {
			end = w.at
		}
		meta = append(meta, formatDuration(max(0, end.Sub(a.Started))))
	}
	if a.ContextTokens > 0 {
		meta = append(meta, compactCount(a.ContextTokens))
	}
	lead := strings.Repeat(" ", indent) + stateBullet(a.State, w.th) + " "
	return stateRow(lead, what, strings.Join(meta, " · "), w.width, w.th)
}

// selected is the selected agent, false without agents.
func (w *workflowDialog) selected() (app.WorkflowAgent, bool) {
	if len(w.order) == 0 {
		return app.WorkflowAgent{}, false
	}
	return w.agents[w.order[w.d.sel]], true
}

func (w *workflowDialog) foot() []string {
	km := &w.m.keys
	parts := []string{binding(km.submit, "view")}
	if a, ok := w.selected(); ok && (a.State == tool.TaskRunning || a.State == app.WorkflowQueued) {
		parts = append(parts, binding(km.stopAgent, "stop agent"))
	}
	return append(parts, binding(km.dismiss, "close"))
}

// key goes to the list: dismiss closes it, the stop key stops the selected
// agent while it is queued or running, and Enter shows the agent. Watching, only
// dismiss, which returns to the list, and the expand-all key act.
func (w *workflowDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if w.watching {
		switch {
		case key.Matches(msg, m.keys.dismiss):
			return w.leave(m), true
		case key.Matches(msg, m.keys.expandAll):
			m.holdTop(m.view().folds.toggleAll)
		}
		return nil, true
	}
	a, ok := w.selected()
	switch {
	case key.Matches(msg, m.keys.dismiss):
		return nil, false
	case !ok:
		return nil, true
	case key.Matches(msg, m.keys.stopAgent):
		if a.State == tool.TaskRunning || a.State == app.WorkflowQueued {
			m.c.StopWorkflowAgent(w.id, a.N)
		}
		return nil, true
	}
	picked, cmd := w.d.key(msg, &m.keys)
	a, _ = w.selected()
	m.workflowSel[w.id] = a.N
	if picked {
		return w.watchSelected(m), true
	}
	return cmd, true
}

// watchSelected shows the selected agent's transcript, read again for its
// current state: the live view of a running agent, a restored one of an ended
// agent. An agent that never opened a session flashes why.
func (w *workflowDialog) watchSelected(m *model) tea.Cmd {
	w.read()
	w.stale = true
	i := findAgent(w.agents, m.workflowSel[w.id])
	if i < 0 {
		return nil
	}
	a := w.agents[i]
	switch {
	case a.ID == "" && a.State == tool.TaskStopped:
		return m.flash("stopped before it started")
	case a.ID == "":
		return m.flash(cmp.Or(a.Reason, "not started yet"))
	case a.State == tool.TaskRunning:
		m.viewOf(a.ID)
	default:
		events, err := m.c.WorkflowHistory(w.id, a.N)
		if err != nil {
			m.showError(err)
			return nil
		}
		v := newView()
		v.tr.Restore(events)
		v.closed = true
		m.views[a.ID] = v
	}
	w.watching, w.prev = true, m.shown
	w.watchWhat = agentWhat(a)
	w.watchID = a.ID
	w.hint.hints = []string{binding(m.keys.dismiss, "go back")}
	return m.show(a.ID)
}

// leave ends watching: the view shown before comes back, and the watched one
// goes if its session had closed.
func (w *workflowDialog) leave(m *model) tea.Cmd {
	id := w.watchID
	w.watching, w.stale, w.watchID = false, true, ""
	cmd := m.show(w.prev)
	if v := m.views[id]; v != nil && v.closed {
		delete(m.views, id)
	}
	return cmd
}

// seen has a change of session close the dialog. A change of the run has it
// read the run again, and a plan, which the model shows in the main view, ends
// watching.
func (w *workflowDialog) seen(m *model, msg tea.Msg) bool {
	switch msg.(type) {
	case event.SessionChanged:
		return false
	case event.TasksChanged, event.ToolEnd, event.TurnEnd:
		w.stale = true
	case event.PlanRequested:
		if w.watching {
			w.leave(m)
		}
	}
	return true
}
