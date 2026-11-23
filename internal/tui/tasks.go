package tui

import (
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// taskDialog is the /tasks dialog, drawn in the composer's slot: the
// background tasks, or one task's output while open names it.
type taskDialog struct {
	d  *dialog
	km *keyMap
	// list is in the order of its rows.
	list []tool.TaskInfo
	open string
	info tool.TaskInfo
	// width, th and at are what the title and options were drawn for; at is the
	// displayed second, which they depend on only while ticking.
	width   int
	th      theme
	at      time.Time
	ticking bool
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
	t := &taskDialog{d: &dialog{}, km: &m.keys, list: listed(list), width: m.width, th: m.th}
	t.setOptions()
	m.open(promptTasks, t)
	return nil
}

// listed is tasks in the order of the list: the running ones as they started,
// then the others, the last to end first.
func listed(tasks []tool.TaskInfo) []tool.TaskInfo {
	return slices.SortedStableFunc(slices.Values(tasks), func(a, b tool.TaskInfo) int {
		ra, rb := a.Status == tool.TaskRunning, b.Status == tool.TaskRunning
		switch {
		case ra && rb:
			return 0
		case ra:
			return -1
		case rb:
			return 1
		}
		return b.Ended.Compare(a.Ended)
	})
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
	t.setOptions()
	return true
}

// taskList shows the list with task id selected. It reports whether the dialog
// stays open: with no tasks it does not.
func (t *taskDialog) taskList(m *model, id string) bool {
	list := listed(m.c.Tasks())
	if len(list) == 0 {
		return false
	}
	sel := slices.IndexFunc(list, func(i tool.TaskInfo) bool { return i.ID == id })
	t.list, t.open, t.d.sel = list, "", max(0, sel)
	t.d.scroll = scroller{}
	t.setOptions()
	return true
}

func (m *model) showError(err error) {
	m.view().tr.Apply(event.Error{Err: err})
	m.relayout()
}

func (t *taskDialog) stoppable() bool { return t.info.Status == tool.TaskRunning }

// setOptions draws the title and options for the dialog's width and theme at
// this second: the list's rows, running tasks first and the others under a
// heading, or an open task's choices.
func (t *taskDialog) setOptions() {
	d := t.d
	d.options = d.options[:0]
	t.at = time.Now().Truncate(time.Second)
	if t.open != "" {
		t.ticking = t.stoppable()
		d.title = cut(taskLine(t.info, t.at), t.width-2)
		if t.stoppable() {
			d.options = append(d.options, option{label: "Stop"})
		}
		d.options = append(d.options, option{label: "Back"})
	} else {
		t.ticking = false
		d.title = "Background tasks"
		for i, info := range t.list {
			var rows []string
			running := info.Status == tool.TaskRunning
			if !running && (i == 0 || t.list[i-1].Status == tool.TaskRunning) {
				rows = append(rows, "  "+fg(t.th.muted).Render("Finished"))
			}
			t.ticking = t.ticking || running
			d.options = append(d.options, option{drawn: append(rows, taskRow(info, t.at, t.width, t.th))})
		}
	}
	d.sel = min(d.sel, len(d.options)-1)
}

// dialog is the task list or, for an open task, its output, which fills what
// the other rows leave, from its end until it is scrolled.
func (t *taskDialog) dialog(width int, th theme) *dialog {
	if width != t.width || th != t.th || (t.ticking && !time.Now().Truncate(time.Second).Equal(t.at)) {
		t.width, t.th = width, th
		t.setOptions()
	}
	t.d.body, t.d.tail, t.d.hint = nil, t.open != "", scrollHint(t.km)
	if t.open != "" {
		t.d.body, t.d.foot = t.wrapped(width-2, th), binding(t.km.dismiss, "go back")
		return t.d
	}
	parts := []string{binding(t.km.submit, "view")}
	if info := t.list[t.d.sel]; info.Agent != "" && info.Status == tool.TaskRunning {
		parts = append(parts, binding(t.km.stopAgent, "stop agent"))
	}
	parts = append(parts, binding(t.km.dismiss, "close"))
	t.d.foot = strings.Join(slices.DeleteFunc(parts, isEmpty), " · ")
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

// taskWhat is what a task is: its command's first line, an agent's shown as
// "<agent> › <description>".
func taskWhat(info tool.TaskInfo) string {
	command, _, _ := strings.Cut(info.Command, "\n")
	what := oneLine(command)
	if info.Agent != "" {
		what = oneLine(info.Agent) + " › " + what
	}
	return what
}

// taskTime is how long a task has run at now, or ran.
func taskTime(info tool.TaskInfo, now time.Time) string {
	end := info.Ended
	if info.Status == tool.TaskRunning {
		end = now
	}
	return formatDuration(end.Sub(info.Started))
}

// taskLine is an open task's title: "running · 1m 2s · <what it is>".
func taskLine(info tool.TaskInfo, now time.Time) string {
	return string(info.Status) + " · " + taskTime(info, now) + " · " + taskWhat(info)
}

// taskRow is a task's row in the list: its bullet in the colour of its status,
// what it is, and flush right how it ended, unless it completed, and its time.
func taskRow(info tool.TaskInfo, now time.Time, width int, th theme) string {
	meta := taskTime(info, now)
	if info.Status == tool.TaskFailed || info.Status == tool.TaskStopped {
		meta = string(info.Status) + " · " + meta
	}
	return stateRow("  "+stateBullet(info.Status, th)+" ", taskWhat(info), meta, width, th)
}

func cut(s string, width int) string { return ansi.Truncate(s, max(1, width), "…") }
