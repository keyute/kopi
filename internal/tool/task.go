package tool

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskStopped   TaskStatus = "stopped"
)

// TaskInfo describes a background task. Started is when its command started,
// also for one moved from the foreground; Ended is zero while it runs.
type TaskInfo struct {
	ID, Command    string
	Started, Ended time.Time
	Status         TaskStatus
	// Reason is why the task completed or failed, "" while running or stopped.
	Reason string
	Output string
}

func (i TaskInfo) StatusText() string {
	switch i.Status {
	case TaskRunning:
		return fmt.Sprintf("running for %v", time.Since(i.Started).Round(time.Second))
	case TaskStopped:
		return string(i.Status)
	default:
		return fmt.Sprintf("%s (%s)", i.Status, i.Reason)
	}
}

// Moved tells where a Run's command went; an empty ID means it finished in
// the foreground.
type Moved struct {
	ID       string
	TimedOut bool
}

// Tasks runs commands in the foreground, moves them to the background at a
// timeout or on Detach, and tracks the background ones.
type Tasks struct {
	mu           sync.Mutex
	next         int
	tasks        []*task // background only, in id order
	fg           map[*task]struct{}
	pending      map[*task]struct{}
	onChange     func(TaskInfo)
	onForeground func()
}

type task struct {
	id, command string
	started     time.Time
	p           *process
	// ready closes once a task's start has returned, setting p on
	// success.
	ready chan struct{}
	moved chan struct{}
	// watched closes once a background task's watch has returned, ended
	// callback included.
	watched chan struct{}
	// Guarded by Tasks.mu.
	detached, stopped, forgotten bool
}

func NewTasks() *Tasks {
	return &Tasks{fg: map[*task]struct{}{}, pending: map[*task]struct{}{}}
}

// OnChange registers f, called without the manager's lock when a task enters
// the background running and when a background task ends. A command that
// ends while its Run still waits on it never reaches f, and once StopAll has
// returned no ended call for a task it forgot is running or will run.
func (m *Tasks) OnChange(f func(TaskInfo)) {
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}

// OnForeground registers f, called without the manager's lock when a Run's
// command becomes live in the foreground and when it leaves the foreground;
// f must not block and re-reads Foreground.
func (m *Tasks) OnForeground(f func()) {
	m.mu.Lock()
	m.onForeground = f
	m.mu.Unlock()
}

// Start runs command as a background task; ctx is checked only before it
// starts.
func (m *Tasks) Start(ctx context.Context, sh *Shell, command string) (TaskInfo, error) {
	t := &task{command: command, ready: make(chan struct{})}
	m.mu.Lock()
	m.pending[t] = struct{}{}
	m.mu.Unlock()
	p, err := sh.start(ctx, command)
	m.mu.Lock()
	delete(m.pending, t)
	t.p = p
	if err != nil || t.stopped {
		close(t.ready)
		m.mu.Unlock()
		if err != nil {
			return TaskInfo{}, err
		}
		p.stop(ErrStopped)
		<-p.done
		return TaskInfo{}, ErrStopped
	}
	t.started = time.Now()
	info := m.adopt(t)
	f := m.onChange
	close(t.ready)
	m.mu.Unlock()
	if f != nil {
		f(info)
	}
	go m.watch(t)
	return info, nil
}

// Run returns the command's combined output. The error of a failed command
// carries the output, then the reason on a final line. At timeout, or after
// Detach, the command keeps running as a background task and Run returns where
// it went.
// A cancel of ctx kills the command only while it is in the foreground.
func (m *Tasks) Run(ctx context.Context, sh *Shell, command string, timeout time.Duration) (string, Moved, error) {
	t := &task{command: command, ready: make(chan struct{}), moved: make(chan struct{})}
	m.mu.Lock()
	m.fg[t] = struct{}{}
	m.mu.Unlock()
	p, err := sh.start(ctx, command)
	if err != nil {
		m.mu.Lock()
		delete(m.fg, t)
		m.mu.Unlock()
		close(t.ready)
		return "", Moved{}, err
	}
	t.p, t.started = p, time.Now()
	close(t.ready)
	m.mu.Lock()
	f := m.onForeground
	m.mu.Unlock()
	if f != nil {
		f()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var timedOut, canceled bool
	select {
	case <-p.done:
	case <-t.moved:
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		canceled = true
	}
	m.mu.Lock()
	detach, stopped := t.detached, t.stopped
	var info TaskInfo
	if (detach || timedOut) && !stopped && !ended(p) {
		info = m.adopt(t)
		delete(m.fg, t)
	}
	change := m.onChange
	f = m.onForeground
	m.mu.Unlock()
	if info.ID != "" {
		if f != nil {
			f()
		}
		if change != nil {
			change(info)
		}
		go m.watch(t)
		return "", Moved{ID: info.ID, TimedOut: !detach}, nil
	}
	if canceled {
		p.stop(ctx.Err())
	}
	<-p.done
	m.mu.Lock()
	delete(m.fg, t)
	f = m.onForeground
	m.mu.Unlock()
	if f != nil {
		f()
	}
	out, err := p.result()
	return out, Moved{}, err
}

// adopt registers t in the background while the caller holds m.mu.
func (m *Tasks) adopt(t *task) TaskInfo {
	m.next++
	t.id = fmt.Sprintf("t%d", m.next)
	t.watched = make(chan struct{})
	m.tasks = append(m.tasks, t)
	return t.report(false)
}

func (m *Tasks) watch(t *task) {
	defer close(t.watched)
	<-t.p.done
	m.mu.Lock()
	forgotten := t.forgotten
	f := m.onChange
	m.mu.Unlock()
	if !forgotten && f != nil {
		f(t.report(true))
	}
}

// Detach marks every foreground run, including one still starting, to be
// moved to the background, and reports whether there was any.
func (m *Tasks) Detach() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t := range m.fg {
		if !t.detached {
			t.detached = true
			close(t.moved)
		}
	}
	return len(m.fg) > 0
}

func (m *Tasks) Foreground() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fg) > 0
}

// List returns the background tasks in id order, without their Output.
func (m *Tasks) List() []TaskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]TaskInfo, 0, len(m.tasks))
	for _, t := range m.tasks {
		infos = append(infos, t.info(ended(t.p)))
	}
	return infos
}

func (m *Tasks) Output(id string) (TaskInfo, error) {
	t, err := m.find(id)
	if err != nil {
		return TaskInfo{}, err
	}
	return t.report(ended(t.p)), nil
}

// Stop kills the task's process group and returns its final info; on a task
// that has already ended it only returns that.
func (m *Tasks) Stop(id string) (TaskInfo, error) {
	t, err := m.find(id)
	if err != nil {
		return TaskInfo{}, err
	}
	t.p.stop(ErrStopped)
	<-t.p.done
	return t.report(true), nil
}

// StopAll stops every pending, foreground and background command, waits for
// them, and forgets the background tasks without reporting their end: when it
// returns, no ended call to the OnChange callback for them is still running.
func (m *Tasks) StopAll() {
	m.mu.Lock()
	fg := make([]*task, 0, len(m.fg))
	for t := range m.fg {
		t.stopped = true
		fg = append(fg, t)
	}
	pending := make([]*task, 0, len(m.pending))
	for t := range m.pending {
		t.stopped = true
		pending = append(pending, t)
	}
	bg := m.tasks
	for _, t := range bg {
		t.forgotten = true
	}
	m.tasks = nil
	m.mu.Unlock()
	for _, t := range fg {
		<-t.ready
		if t.p != nil {
			t.p.stop(ErrStopped)
		}
	}
	for _, t := range bg {
		t.p.stop(ErrStopped)
	}
	for _, t := range pending {
		<-t.ready
		if t.p != nil {
			t.p.stop(ErrStopped)
			<-t.p.done
		}
	}
	for _, t := range fg {
		if t.p != nil {
			<-t.p.done
		}
	}
	for _, t := range bg {
		<-t.p.done
		<-t.watched
	}
}

func (m *Tasks) find(id string) (*task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.id == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("failed to find task %q: %w", id, ErrUnknownTask)
}

// report is info with the task's output.
func (t *task) report(done bool) TaskInfo {
	info := t.info(done)
	info.Output = t.p.out.String()
	return info
}

// info reads the process's outcome only when done says it has ended. It
// leaves Output empty.
func (t *task) info(done bool) TaskInfo {
	info := TaskInfo{ID: t.id, Command: t.command, Started: t.started, Status: TaskRunning}
	if !done {
		return info
	}
	info.Ended = t.p.ended
	var exitStatus *exec.ExitError
	switch {
	case t.p.reason != nil:
		info.Status = TaskStopped
	case t.p.err != nil:
		info.Status, info.Reason = TaskFailed, t.p.err.Error()
	case t.p.exit == nil:
		info.Status, info.Reason = TaskCompleted, "exit code 0"
	case errors.As(t.p.exit, &exitStatus):
		info.Status, info.Reason = TaskFailed, exitReason(exitStatus)
	default:
		info.Status, info.Reason = TaskFailed, fmt.Sprintf("failed to run command: %v", t.p.exit)
	}
	return info
}

func ended(p *process) bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
