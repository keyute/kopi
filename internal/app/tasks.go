package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// Tasks is the background task manager the Controller reports on and controls.
// OnChange's callback re-enters the Controller, so the Controller never calls
// a Tasks method with queue or mu held.
type Tasks interface {
	OnChange(f func(tool.TaskInfo))
	OnForeground(f func())
	Detach() bool
	Foreground() bool
	List() []tool.TaskInfo
	Output(id string) (tool.TaskInfo, error)
	Stop(id string) (tool.TaskInfo, error)
	StopAll()
}

const (
	// noticeLines is how many of a task's last non-empty output lines a notice
	// carries.
	noticeLines = 20
	// noticeCommandRunes caps the command line a notice quotes.
	noticeCommandRunes = 80
)

// pendingNotice is a notice waiting for the model; wake reports whether it
// may start or continue a turn on its own.
type pendingNotice struct {
	n    pigruntime.Notice
	wake bool
}

// taskChanged is the task manager's OnChange callback. A task the model stops
// gets no notice: the model's own tool result already says so.
func (c *Controller) taskChanged(info tool.TaskInfo) {
	c.relay.emit(event.TasksChanged{})
	if info.Status != tool.TaskCompleted && info.Status != tool.TaskFailed {
		return
	}
	head := fmt.Sprintf("Background task %s %s (%s): %s", info.ID, info.Status, info.Reason, commandLine(info.Command))
	c.queueNotice(notice(info, head), true)
}

// queueNotice queues n for the model and, for a waking notice while no turn
// runs and no session switch is under way, starts a turn with no human prompt.
// It drops n once closed or with no session. A switch that commits clears what
// was queued; one that fails keeps it for the next prompt or wake.
func (c *Controller) queueNotice(n pigruntime.Notice, wake bool) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.closed || c.sess == nil {
		c.mu.Unlock()
		return
	}
	c.notices = append(c.notices, pendingNotice{n: n, wake: wake})
	if !wake || c.turn != nil || c.switching != nil || !c.cfg.Wake || c.hold {
		c.mu.Unlock()
		return
	}
	c.startTurn(context.Background(), nil)
}

// takeNotices empties notices; it must be called with queue held.
func (c *Controller) takeNotices() []pigruntime.Notice {
	if len(c.notices) == 0 {
		return nil
	}
	out := make([]pigruntime.Notice, 0, len(c.notices))
	for _, p := range c.notices {
		out = append(out, p.n)
	}
	c.notices = nil
	return out
}

// waking reports whether a queued notice may continue a turn; it must be
// called with queue held.
func (c *Controller) waking() bool {
	return slices.ContainsFunc(c.notices, func(p pendingNotice) bool { return p.wake })
}

// requeue puts notices the model never read back ahead of those queued since;
// it must be called with queue held. Only StopTask queues a stopped notice, so
// one comes back non-waking and every other waking.
func (c *Controller) requeue(ns []pigruntime.Notice) {
	if len(ns) == 0 {
		return
	}
	back := make([]pendingNotice, 0, len(ns)+len(c.notices))
	for _, n := range ns {
		back = append(back, pendingNotice{n: n, wake: n.Shown.Status != string(tool.TaskStopped)})
	}
	c.notices = append(back, c.notices...)
}

func notice(info tool.TaskInfo, head string) pigruntime.Notice {
	text := head
	if tail := outputTail(info.Output); tail != "" {
		text += "\n" + tail
	}
	return pigruntime.Notice{
		Text:  text,
		Shown: event.TaskEnded{ID: info.ID, Command: info.Command, Status: string(info.Status), Elapsed: info.Ended.Sub(info.Started)},
	}
}

func commandLine(command string) string {
	line, _, _ := strings.Cut(command, "\n")
	if r := []rune(line); len(r) > noticeCommandRunes {
		return string(r[:noticeCommandRunes])
	}
	return line
}

func outputTail(output string) string {
	var lines []string
	for line := range strings.Lines(output) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines[max(0, len(lines)-noticeLines):], "\n")
}

// Background moves the running foreground command to the background and
// reports whether there was one.
func (c *Controller) Background() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}
	return c.cfg.Tasks.Detach()
}

// Tasks returns the background tasks in id order.
func (c *Controller) Tasks() []tool.TaskInfo {
	return c.cfg.Tasks.List()
}

func (c *Controller) TaskOutput(id string) (tool.TaskInfo, error) {
	return c.cfg.Tasks.Output(id)
}

// StopTask stops a running background task for the human. The model is told
// with the next request but is not woken for it. Stopping a task that has
// already ended does nothing.
func (c *Controller) StopTask(id string) error {
	info, err := c.cfg.Tasks.Output(id)
	if err != nil {
		return err
	}
	if info.Status != tool.TaskRunning {
		return nil
	}
	info, err = c.cfg.Tasks.Stop(id)
	if err != nil {
		return err
	}
	if info.Status != tool.TaskStopped {
		return nil
	}
	head := fmt.Sprintf("Background task %s stopped by the user: %s", info.ID, commandLine(info.Command))
	c.queueNotice(notice(info, head), false)
	return nil
}
