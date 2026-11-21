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
	d    *dialog
	km   *keyMap
	list []tool.TaskInfo
	open string
	info tool.TaskInfo
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
	t := &taskDialog{d: &dialog{}, km: &m.keys, list: list}
	t.setOptions(m.width)
	m.open(promptTasks, t)
	return nil
}

// key goes to the dialog: dismiss leaves the task for the list and the list
// for the composer. Enter on a workflow opens its dialog over the list, on a
// subagent's task shows its session. Other keys it has no use for are swallowed.
func (t *taskDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if key.Matches(msg, m.keys.dismiss) {
		if t.open == "" {
			return nil, false
		}
		return nil, t.taskList(m, t.open)
	}
	if t.open == "" && key.Matches(msg, m.keys.stopAgent) {
		info := t.list[t.d.sel]
		if info.Agent != "" && info.Status == tool.TaskRunning {
			if err := m.c.StopTask(info.ID); err != nil {
				m.showError(err)
			}
			return nil, t.taskList(m, info.ID)
		}
		return nil, true
	}
	picked, cmd := t.d.key(msg, &m.keys)
	if !picked {
		return cmd, true
	}
	switch {
	case t.open == "":
		info := t.list[t.d.sel]
		if m.openWorkflow(info.ID) {
			return nil, true
		}
		if _, ok := m.agent(info.ID); ok && info.Agent != "" {
			return m.show(info.ID), false
		}
		return nil, t.openTask(m, info.ID)
	case t.stoppable() && t.d.sel == 0:
		if err := m.c.StopTask(t.open); err != nil {
			m.showError(err)
		}
		return nil, t.openTask(m, t.open)
	}
	return nil, t.taskList(m, t.open)
}

// seen ends the dialog with a session switch. A change of tasks has it read what
// it shows again, keeping the selected task.
func (t *taskDialog) seen(m *model, msg tea.Msg) bool {
	switch msg.(type) {
	case event.SessionChanged:
		return false
	case event.TasksChanged:
		if t.open != "" {
			return t.openTask(m, t.open)
		}
		return t.taskList(m, t.list[t.d.sel].ID)
	}
	return true
}

// openTask shows task id's output, or the list when it cannot be read. A
// task newly opened starts on its first option. It reports whether the dialog
// stays open.
func (t *taskDialog) openTask(m *model, id string) bool {
	info, err := m.c.TaskOutput(id)
	if err != nil {
		m.showError(err)
		return t.taskList(m, id)
	}
	if id != t.open {
		t.d.sel, t.d.scroll = 0, scroller{}
	}
	t.open, t.info, t.out = id, info, nil
	t.setOptions(m.width)
	return true
}

// taskList shows the list with task id selected. It reports whether the dialog
// stays open: with no tasks it does not.
func (t *taskDialog) taskList(m *model, id string) bool {
	list := m.c.Tasks()
	if len(list) == 0 {
		return false
	}
	sel := slices.IndexFunc(list, func(i tool.TaskInfo) bool { return i.ID == id })
	t.list, t.open, t.d.sel = list, "", max(0, sel)
	t.d.scroll = scroller{}
	t.setOptions(m.width)
	return true
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

// dialog is the task list or, for an open task, its output, which fills what
// the other rows leave, from its end until it is scrolled.
func (t *taskDialog) dialog(width int, th theme) *dialog {
	t.setOptions(width)
	t.d.note = ""
	if t.open == "" {
		if info := t.list[t.d.sel]; info.Agent != "" && info.Status == tool.TaskRunning {
			t.d.note = binding(t.km.stopAgent, "stop agent")
		}
	}
	t.d.body, t.d.tail, t.d.hint = nil, t.open != "", scrollHint(t.km)
	if t.open != "" {
		t.d.body = t.wrapped(width-2, th)
	}
	return t.d
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
