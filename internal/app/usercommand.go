package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// userCommand is a shell command the human ran with "!". It is guarded by
// Controller.queue, like the entry that holds it.
type userCommand struct {
	text    string
	state   event.CommandState
	started time.Time
	elapsed time.Duration
	// reason is why it ended as the task registry words it, such as "exit code
	// 3"; exit is that as shown, "exit 3".
	reason, exit string
	failed       bool
	output       string
	// file is where a moved command's output is saved; taskID is its task.
	file, taskID string
	// live is the running command, nil before it begins and after it ends.
	live *tool.Live
	// stop is set when the command must not run on: its Begin may still be under
	// way, so live alone cannot carry it.
	stop bool
}

// snapshot is the command as the queue reports it; every report gets its own, as
// frontends tell reports apart by identity.
func (u *userCommand) snapshot() *event.UserCommand {
	s := u.shown()
	return &s
}

func (u *userCommand) shown() event.UserCommand {
	return event.UserCommand{Command: u.text, State: u.state, Started: u.started, Elapsed: u.elapsed, Exit: u.exit, Failed: u.failed, Output: u.output, TaskID: u.taskID}
}

// notice is what the model reads of a command that has finished, was stopped or
// moved to the background; wakes is whether a background task's end reaches the
// model whatever its turn does.
func (u *userCommand) notice(wakes bool) pigruntime.Notice {
	var head string
	switch u.state {
	case event.CommandStopped:
		head = "The user ran a shell command and stopped it"
		if u.elapsed >= time.Second {
			head += " after " + u.elapsed.Round(time.Second).String()
		}
		head += ":"
	case event.CommandMoved:
		head = fmt.Sprintf("The user ran a shell command and moved it to the background as task %s%s", u.taskID, tool.BackgroundTail(u.file, wakes))
	default:
		detail := u.reason
		if u.elapsed >= time.Second {
			detail += ", " + u.elapsed.Round(time.Second).String()
		}
		head = fmt.Sprintf("The user ran a shell command (%s):", detail)
	}
	text := head + "\n$ " + u.text
	if u.output != "" {
		text += "\n" + u.output
	}
	return pigruntime.Notice{Text: text, Shown: u.shown()}
}

// exitText words a finished command's reason as shown: "exit code 3" is "exit
// 3", and any other reason stays as the registry gives it.
func exitText(reason string) string {
	if code, ok := strings.CutPrefix(reason, "exit code "); ok {
		return "exit " + code
	}
	return reason
}

// runShell queues the human's command and starts it when none runs. It takes the
// refusals a prompt gets.
func (c *Controller) runShell(command, text string) error {
	if command == "" {
		return nil
	}
	c.record(text)
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	var err error
	switch {
	case c.closed || c.main.sess == nil:
		err = ErrNoSession
	case c.login != nil:
		err = ErrLoggingIn
	case c.switching != nil || c.main.changingModel():
		err = ErrBusy
	}
	emit := c.main.emit
	c.mu.Unlock()
	if err != nil {
		return err
	}
	r := c.main
	r.hold = false
	r.queued = append(r.queued, &entry{cmd: &userCommand{text: command, state: event.CommandQueued}})
	r.startNextLocked()
	r.queueChanged(emit)
	return nil
}

// startNextLocked runs the first queued command when none runs: commands run one
// at a time, in the order they were given, whether or not a turn runs. A switch
// holds the chain until it ends. It reports whether a command started, which the
// caller reports in a QueueChanged in the same queue section. It must be called
// with queue held.
func (r *runner) startNextLocked() bool {
	c := r.c
	c.mu.Lock()
	blocked := c.closed || c.switching != nil
	c.mu.Unlock()
	if blocked {
		return false
	}
	var next *entry
	for _, e := range r.queued {
		if e.cmd == nil {
			continue
		}
		if e.cmd.state == event.CommandRunning {
			return false
		}
		if next == nil && e.cmd.state == event.CommandQueued {
			next = e
		}
	}
	if next == nil {
		return false
	}
	next.cmd.state, next.cmd.started = event.CommandRunning, time.Now()
	c.wg.Go(func() { r.runCommand(next) })
	return true
}

// runCommand runs e's command to its end, or its move to the background, and
// records how it went. It holds no lock while the command runs; the result is
// dropped when e has left the queue, which a switch or Close does.
func (r *runner) runCommand(e *entry) {
	c := r.c
	u := e.cmd
	live, err := c.cfg.Tasks.Begin(context.Background(), c.cfg.UserShell, u.text)
	var moved tool.Moved
	var info tool.TaskInfo
	if err == nil {
		c.queue.Lock()
		u.live = live
		stop := u.stop
		c.queue.Unlock()
		if stop {
			live.Stop()
		}
		if moved = live.Wait(context.Background(), 0); moved.ID == "" {
			info = live.Info()
		}
	}

	c.queue.Lock()
	defer c.queue.Unlock()
	u.live = nil
	c.mu.Lock()
	closed, emit := c.closed, r.emit
	c.mu.Unlock()
	if closed || !slices.Contains(r.queued, e) {
		return
	}
	switch {
	case err != nil:
		u.state, u.failed, u.reason, u.exit, u.output = event.CommandExited, true, "failed to start", "failed to start", err.Error()
		u.elapsed = time.Since(u.started)
	case moved.ID != "":
		u.state, u.taskID, u.file = event.CommandMoved, moved.ID, moved.OutputFile
		u.elapsed = time.Since(u.started)
	case info.Status == tool.TaskStopped:
		u.state = event.CommandStopped
		u.elapsed, u.output = info.Ended.Sub(info.Started), info.Output
	default:
		u.state, u.failed, u.reason, u.exit = event.CommandExited, info.Status == tool.TaskFailed, info.Reason, exitText(info.Reason)
		u.elapsed, u.output, u.file = info.Ended.Sub(info.Started), info.Output, info.OutputFile
	}
	r.startNextLocked()
	r.queueChanged(emit)
	c.mu.Lock()
	r.advanceLocked(true)
}

// CommandOutput returns the output so far of the command that runs, "" when none
// does.
func (c *Controller) CommandOutput() string {
	c.queue.Lock()
	var live *tool.Live
	for _, e := range c.main.queued {
		if e.cmd != nil && e.cmd.state == event.CommandRunning {
			live = e.cmd.live
		}
	}
	c.queue.Unlock()
	if live == nil {
		return ""
	}
	return live.Output()
}

// interruptCommand stops the command that runs, with no turn running: the
// commands queued behind it never run and the prompts waiting are handed back
// with a count of those dropped. The stopped command keeps its entry, and ends
// as a record without a reply. It returns what stops the command, which the
// caller calls once queue is released, and whether there was a command. It must
// be called with queue held.
func (r *runner) interruptCommand(emit func(event.Event)) (stop func(), ok bool) {
	var running *entry
	for _, e := range r.queued {
		if e.cmd != nil && e.cmd.state == event.CommandRunning {
			running = e
		}
	}
	if running == nil {
		return nil, false
	}
	running.cmd.stop = true
	dropped := 0
	r.queued = slices.DeleteFunc(r.queued, func(e *entry) bool {
		if e.cmd != nil && e.cmd.state == event.CommandQueued {
			dropped++
			return true
		}
		return false
	})
	taken := r.take(emit)
	if len(taken) > 0 || dropped > 0 {
		emit(event.Unsent{Inputs: r.c.inputs(taken), Dropped: dropped})
	}
	r.queueChanged(emit)
	if live := running.cmd.live; live != nil {
		return live.Stop, true
	}
	return func() {}, true
}

// clearQueueLocked empties the queue of a session that ends and returns what
// stops its running command; the caller calls it once queue is released. A
// command whose Begin is under way stops when it returns. It must be called
// with queue held.
func (r *runner) clearQueueLocked() (stop func()) {
	var stops []func()
	for _, e := range r.queued {
		if e.cmd == nil {
			continue
		}
		e.cmd.stop = true
		if e.cmd.live != nil {
			stops = append(stops, e.cmd.live.Stop)
		}
	}
	r.queued = nil
	return func() {
		for _, s := range stops {
			s()
		}
	}
}
