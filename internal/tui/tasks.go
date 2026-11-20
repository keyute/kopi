package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// taskDialog is the /tasks dialog, drawn in the composer's slot: the
// background tasks, or one task's output while open names it.
type taskDialog struct {
	d *dialog
	// scrolled is set when the human moves the open output off its end.
	scrolled bool
	list     []tool.TaskInfo
	open     string
	info     tool.TaskInfo
	// out is info's output as body rows for outWidth and outTheme, kept
	// across renders.
	out      []string
	outWidth int
	outTheme theme
}

// showTasks opens the dialog on the task list; with no tasks it only
// flashes.
func (m *model) showTasks() tea.Cmd {
	list := m.c.Tasks()
	if len(list) == 0 {
		return m.flash("no background tasks")
	}
	m.tasks = &taskDialog{d: &dialog{}, list: list}
	m.tasks.setOptions(m.width)
	return nil
}

// tasksKey goes to the dialog: interrupt leaves the task for the list and
// the list for the composer. Enter on a subagent's task shows its session.
// Other keys it has no use for are swallowed.
func (m *model) tasksKey(msg tea.KeyPressMsg) tea.Cmd {
	t := m.tasks
	if key.Matches(msg, m.keys.interrupt) {
		if t.open == "" {
			m.tasks = nil
		} else {
			m.taskList(t.open)
		}
		return nil
	}
	if t.open == "" && key.Matches(msg, m.keys.stopAgent) {
		info := t.list[t.d.sel]
		if info.Agent != "" && info.Status == tool.TaskRunning {
			if err := m.c.StopTask(info.ID); err != nil {
				m.showError(err)
			}
			m.taskList(info.ID)
		}
		return nil
	}
	picked, cmd := t.d.key(msg, &m.keys)
	if !picked {
		return cmd
	}
	switch {
	case t.open == "":
		info := t.list[t.d.sel]
		if _, ok := m.agent(info.ID); ok && info.Agent != "" {
			m.tasks = nil
			return m.show(info.ID)
		}
		m.openTask(info.ID)
	case t.stoppable() && t.d.sel == 0:
		if err := m.c.StopTask(t.open); err != nil {
			m.showError(err)
		}
		m.openTask(t.open)
	default:
		m.taskList(t.open)
	}
	return nil
}

// refreshTasks re-reads what the dialog shows, keeping the selected task.
func (m *model) refreshTasks() {
	if m.tasks == nil {
		return
	}
	if m.tasks.open != "" {
		m.openTask(m.tasks.open)
		return
	}
	m.taskList(m.tasks.list[m.tasks.d.sel].ID)
}

// openTask shows task id's output, or the list when it cannot be read. A
// task newly opened starts on its first option.
func (m *model) openTask(id string) {
	t := m.tasks
	info, err := m.c.TaskOutput(id)
	if err != nil {
		m.showError(err)
		m.taskList(id)
		return
	}
	if id != t.open {
		t.d.sel, t.d.scroll, t.scrolled = 0, scroller{}, false
	}
	t.open, t.info, t.out = id, info, nil
	t.setOptions(m.width)
}

// taskList shows the list with task id selected, closing the dialog when
// there are no tasks.
func (m *model) taskList(id string) {
	t := m.tasks
	list := m.c.Tasks()
	if len(list) == 0 {
		m.tasks = nil
		return
	}
	sel := slices.IndexFunc(list, func(i tool.TaskInfo) bool { return i.ID == id })
	t.list, t.open, t.d.sel = list, "", max(0, sel)
	t.d.scroll, t.scrolled = scroller{}, false
	t.setOptions(m.width)
}

func (m *model) showError(err error) {
	m.view().tr.Apply(event.Error{Err: err})
	m.relayout()
}

func (t *taskDialog) stoppable() bool { return t.info.Status == tool.TaskRunning }

// setOptions sets the title and options for width, the labels cut to one row.
func (t *taskDialog) setOptions(width int) {
	d := t.d
	d.options = nil
	label := func(text string) {
		n := len(d.options) + 1
		d.options = append(d.options, option{label: cut(text, width-lipgloss.Width(fmt.Sprintf("  %d. ", n)))})
	}
	if t.open == "" {
		d.title = "Background tasks"
		for _, info := range t.list {
			label(taskLine(info))
		}
	} else {
		d.title = cut(taskLine(t.info), width-2)
		if t.stoppable() {
			label("Stop")
		}
		label("Back")
	}
	d.sel = min(d.sel, len(d.options)-1)
}

// rows draws the dialog within maxRows; an open task's output fills what
// the other rows leave, at most maxBody rows, from its end until it is
// scrolled.
func (t *taskDialog) rows(width, maxBody, maxRows int, km *keyMap, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	t.setOptions(width)
	t.d.note = ""
	if t.open == "" {
		if info := t.list[t.d.sel]; info.Agent != "" && info.Status == tool.TaskRunning {
			t.d.note = binding(km.stopAgent, "stop agent")
		}
	}
	t.d.body = nil
	if t.open != "" {
		t.d.body = t.wrapped(width-2, th)
		// The view follows the end until the human scrolls up.
		if !t.scrolled {
			t.d.scroll.off = len(t.d.body)
		}
	}
	rows := t.d.scrollRows(width, maxRows, 1, maxBody, scrollHint(km), th)
	t.scrolled = t.d.scroll.below > 0
	if t.open == "" && len(rows) > maxRows {
		head := len(rows) - len(t.d.options) - 1
		room := maxRows - head - 1
		if room > 0 {
			start := max(0, t.d.sel-room+1)
			rows = append(rows[:head], rows[head+start:head+start+room]...)
			return append(rows, rows[0])
		}
	}
	return rows[max(0, len(rows)-maxRows):]
}

// wrapped is the output as body rows for width and th.
func (t *taskDialog) wrapped(width int, th theme) []string {
	if t.out == nil || t.outWidth != width || t.outTheme != th {
		t.out = hang("  ", fg(th.muted), strings.TrimRight(t.info.Output, "\n"), fg(th.muted), width)
		t.outWidth, t.outTheme = width, th
	}
	return t.out
}

// taskLine is "t1 · running · 1m 2s · <command's first line>", an agent's
// command shown as "<agent> › <description>": the time is since the start
// while running, else the whole run.
func taskLine(info tool.TaskInfo) string {
	end := info.Ended
	if info.Status == tool.TaskRunning {
		end = time.Now()
	}
	command, _, _ := strings.Cut(info.Command, "\n")
	what := oneLine(command)
	if info.Agent != "" {
		what = oneLine(info.Agent) + " › " + what
	}
	return oneLine(info.ID) + " · " + string(info.Status) + " · " + formatDuration(end.Sub(info.Started)) + " · " + what
}

func cut(s string, width int) string { return ansi.Truncate(s, max(1, width), "…") }
