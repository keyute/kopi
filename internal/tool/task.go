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
	ID string
	// Command is the shell command, or the description of an agent call.
	Command string
	// Agent is the name of the agent running an agent task, "" for a command.
	Agent          string
	Started, Ended time.Time
	Status         TaskStatus
	// Reason is why the task completed or failed, "" while running or stopped.
	Reason string
	// Result is the final message of a completed agent run.
	Result string
	Output string
}

func (i TaskInfo) StatusText() string {
	switch i.Status {
	case TaskRunning:
		return fmt.Sprintf("running for %v", time.Since(i.Started).Round(time.Second))
	case TaskStopped:
		return string(i.Status)
	default:
		if i.Reason == "" {
			return string(i.Status)
		}
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
// timeout or on Detach, and tracks the background ones and the agent tasks its
// callers start.
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
	id, command, agent string
	started            time.Time
	// p is a command's process; an agent task has a instead.
	p *process
	a *agentJob
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

// job is what the registry needs from a running command or agent run.
type job interface {
	finished() <-chan struct{}
	stop(reason error)
	text() string
	outcome() jobOutcome
}

// jobOutcome is a job's state; started is set only when the job owns it.
type jobOutcome struct {
	started, ended time.Time
	status         TaskStatus
	reason, result string
}

func (t *task) job() job {
	if t.p != nil {
		return t.p
	}
	return t.a
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
	// Running whatever the process's state: watch reports its end.
	return TaskInfo{ID: t.id, Command: t.command, Started: t.started, Status: TaskRunning, Output: t.p.text()}
}

func (m *Tasks) watch(t *task) {
	defer close(t.watched)
	<-t.p.done
	m.mu.Lock()
	forgotten := t.forgotten
	f := m.onChange
	m.mu.Unlock()
	if !forgotten && f != nil {
		f(t.report())
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
		infos = append(infos, t.info())
	}
	return infos
}

func (m *Tasks) Output(id string) (TaskInfo, error) {
	t, err := m.find(id)
	if err != nil {
		return TaskInfo{}, err
	}
	return t.report(), nil
}

// Stop kills the task's process group, or asks an agent task to stop, and
// returns its final info once the task has ended; on a task that has already
// ended it only returns that.
func (m *Tasks) Stop(id string) (TaskInfo, error) {
	t, err := m.find(id)
	if err != nil {
		return TaskInfo{}, err
	}
	j := t.job()
	done := j.finished()
	j.stop(ErrStopped)
	<-done
	return t.report(), nil
}

// StopAll stops every pending, foreground and background command and agent
// task, waits for them, and forgets the background tasks without reporting
// their end: when it returns, no ended call to the OnChange callback for them
// is still running, and End and Run of a forgotten agent task report false.
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
		t.job().stop(ErrStopped)
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
		<-t.job().finished()
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
func (t *task) report() TaskInfo {
	info := t.info()
	info.Output = t.job().text()
	return info
}

// info leaves Output empty.
func (t *task) info() TaskInfo {
	o := t.job().outcome()
	info := TaskInfo{
		ID: t.id, Command: t.command, Agent: t.agent, Started: t.started,
		Ended: o.ended, Status: o.status, Reason: o.reason, Result: o.result,
	}
	if !o.started.IsZero() {
		info.Started = o.started
	}
	return info
}

func ended(j job) bool {
	select {
	case <-j.finished():
		return true
	default:
		return false
	}
}

func (p *process) finished() <-chan struct{} { return p.done }

func (p *process) text() string { return p.out.String() }

func (p *process) outcome() jobOutcome {
	if !ended(p) {
		return jobOutcome{status: TaskRunning}
	}
	o := jobOutcome{ended: p.ended}
	var exitStatus *exec.ExitError
	switch {
	case p.reason != nil:
		o.status = TaskStopped
	case p.err != nil:
		o.status, o.reason = TaskFailed, p.err.Error()
	case p.exit == nil:
		o.status, o.reason = TaskCompleted, "exit code 0"
	case errors.As(p.exit, &exitStatus):
		o.status, o.reason = TaskFailed, exitReason(exitStatus)
	default:
		o.status, o.reason = TaskFailed, fmt.Sprintf("failed to run command: %v", p.exit)
	}
	return o
}

// agentJob is the state of an agent task, which its owner ends. Guarded by mu,
// which is never held while calling stopFn or taking Tasks.mu.
type agentJob struct {
	mu             sync.Mutex
	out            *outputBuffer
	done           chan struct{}
	stopFn         func()
	running        bool
	stopped        bool
	started, ended time.Time
	status         TaskStatus
	reason, result string
}

func (j *agentJob) finished() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// stop asks the owner to stop a running run, once per run.
func (j *agentJob) stop(error) {
	j.mu.Lock()
	if !j.running || j.stopped {
		j.mu.Unlock()
		return
	}
	j.stopped = true
	f := j.stopFn
	j.mu.Unlock()
	f()
}

func (j *agentJob) text() string { return j.out.String() }

func (j *agentJob) outcome() jobOutcome {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return jobOutcome{started: j.started, status: TaskRunning}
	}
	return jobOutcome{started: j.started, ended: j.ended, status: j.status, reason: j.reason, result: j.result}
}

// AgentTask is a task in a Tasks registry that its owner starts and ends.
type AgentTask struct {
	m *Tasks
	t *task
}

// StartAgent registers a running agent task under the shared id counter; stop
// asks the owner to end the run. OnChange is never called for an agent task:
// its owner reports it.
func (m *Tasks) StartAgent(agent, description string, stop func()) *AgentTask {
	j := &agentJob{out: &outputBuffer{}, done: make(chan struct{}), stopFn: stop, running: true, started: time.Now()}
	watched := make(chan struct{})
	close(watched)
	t := &task{command: description, agent: agent, a: j, watched: watched}
	m.mu.Lock()
	m.next++
	t.id = fmt.Sprintf("t%d", m.next)
	m.tasks = append(m.tasks, t)
	m.mu.Unlock()
	return &AgentTask{m: m, t: t}
}

func (a *AgentTask) ID() string { return a.t.id }

// Write appends to the task's output, which is kept across runs.
func (a *AgentTask) Write(p []byte) (int, error) { return a.t.a.out.Write(p) }

// Run starts another run of an ended task: running again, Started now, Ended
// and Result cleared. It reports false for a task that is still running or
// that StopAll forgot.
func (a *AgentTask) Run(stop func()) bool {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if a.t.forgotten {
		return false
	}
	j := a.t.a
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running, j.stopped, j.stopFn = true, false, stop
	j.done = make(chan struct{})
	j.started = time.Now()
	j.ended, j.status, j.reason, j.result = time.Time{}, TaskRunning, "", ""
	return true
}

// End records how the current run ended and returns the task as it then is. A
// run that was stopped ends as TaskStopped whatever status is passed. It
// reports false once StopAll forgot the task, in which case nothing may be
// reported for it, and for a task that is not running.
func (a *AgentTask) End(status TaskStatus, reason, result string) (TaskInfo, bool) {
	a.m.mu.Lock()
	forgotten := a.t.forgotten
	j := a.t.a
	j.mu.Lock()
	if !j.running {
		j.mu.Unlock()
		a.m.mu.Unlock()
		return TaskInfo{}, false
	}
	if j.stopped || status == TaskStopped {
		status, reason, result = TaskStopped, "", ""
	}
	j.running = false
	j.ended, j.status, j.reason, j.result = time.Now(), status, reason, result
	close(j.done)
	j.mu.Unlock()
	a.m.mu.Unlock()
	return a.t.report(), !forgotten
}
