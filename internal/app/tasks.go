package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// Tasks is the background task manager the Controller reports on and controls.
// OnChange's callback re-enters the Controller, so the Controller never calls
// a Tasks method with queue or mu held; the exception is a subagent's task,
// started, run and ended with queue held, for which the callback is never
// called.
type Tasks interface {
	OnChange(f func(tool.TaskInfo))
	OnForeground(f func())
	Begin(ctx context.Context, sh *tool.Shell, command string) (*tool.Live, error)
	Detach() bool
	ForegroundSince() time.Time
	List() []tool.TaskInfo
	Output(id string) (tool.TaskInfo, error)
	Stop(id string) (info tool.TaskInfo, stopped bool, err error)
	StopAll()
	StartAgent(agent, description string, stop func()) *tool.AgentTask
}

const (
	// noticeLines is how many of a task's last non-empty output lines a notice
	// carries.
	noticeLines = 20
	// noticeCommandRunes caps the command line a notice quotes.
	noticeCommandRunes = 80
)

// taskChanged is the task manager's OnChange callback. A task the model stops
// gets no notice: the model's own tool result already says so.
func (c *Controller) taskChanged(info tool.TaskInfo) {
	c.relay.emit(event.TasksChanged{})
	if info.Status != tool.TaskCompleted && info.Status != tool.TaskFailed {
		return
	}
	head := fmt.Sprintf("Background task %s %s (%s): %s", info.ID, info.Status, info.Reason, commandLine(info.Command))
	c.main.queueNotice(notice(info, head))
}

func notice(info tool.TaskInfo, head string) pigruntime.Notice {
	var lines []string
	tail := outputTail(info.Output)
	if tail != "" {
		lines = append(lines, tail)
	}
	// A tail that still holds the output's cut marker already names the file.
	if info.OutputFile != "" && !strings.Contains(tail, info.OutputFile) {
		lines = append(lines, "[Output saved to "+info.OutputFile+"; use Read to read it.]")
	}
	body := strings.Join(lines, "\n")
	shown := taskEnded(info)
	if info.Agent == "" {
		shown.Result = body
	}
	if body != "" {
		head += "\n" + body
	}
	return pigruntime.Notice{Text: head, Shown: shown}
}

// taskEnded is info as the transcript draws its end; a task that did not fail
// has no reason to show.
func taskEnded(info tool.TaskInfo) event.TaskEnded {
	shown := event.TaskEnded{ID: info.ID, Agent: info.Agent, Command: info.Command, Status: string(info.Status), Elapsed: info.Ended.Sub(info.Started), Result: info.Result}
	if info.Status == tool.TaskFailed {
		shown.Reason = info.Reason
	}
	return shown
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
// with the next request but is not woken for it; a subagent's stop is reported
// by its run's end, like Interrupt's. Stopping a task that has
// already ended does nothing.
func (c *Controller) StopTask(id string) error {
	c.mu.Lock()
	owner := c.main.sess
	c.mu.Unlock()
	info, err := c.cfg.Tasks.Output(id)
	if err != nil {
		return err
	}
	if info.Status != tool.TaskRunning {
		return nil
	}
	c.queue.Lock()
	child := c.children[id]
	run := c.workflows[id]
	var w *Work
	if child != nil {
		w = c.interruptByHumanLocked(child)
	}
	c.queue.Unlock()
	if child != nil {
		if w != nil {
			<-w.done
		}
		return nil
	}
	info, _, err = c.cfg.Tasks.Stop(id)
	if err != nil {
		return err
	}
	if info.Status != tool.TaskStopped {
		return nil
	}
	head := fmt.Sprintf("Background task %s stopped by the user: %s", info.ID, commandLine(info.Command))
	switch {
	case run != nil:
		head = fmt.Sprintf("Workflow %s stopped by the user: %s", info.ID, commandLine(info.Command))
	case info.Agent != "":
		head = agentStoppedHead(info)
	}
	n := notice(info, head)
	if run != nil {
		n.Shown = run.taskEnded(info)
	}
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && owner != nil && c.main.sess == owner {
		c.main.notices = append(c.main.notices, n)
	}
	return nil
}
