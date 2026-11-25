package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// workflowPrefix and workflowSuffix wrap a script's code. The prefix is one
// line, so the script's line numbers stay its own, and defines agent, parallel,
// pipeline and phase over the host global.
const (
	workflowPrefix = `let __n=0,__p;const agent=(prompt,o)=>host({...o,phase:o&&o.phase!=null?String(o.phase):__p,prompt,n:++__n}),phase=t=>{__p=t==null?t:String(t)},parallel=ts=>Promise.all(ts.map(t=>Promise.resolve().then(()=>typeof t==="function"?t():t).catch(()=>null))),pipeline=(items,...stages)=>parallel(items.map((item,i)=>async()=>{let v=item;for(const s of stages)v=await s(v,item,i);return v}));return(async()=>{`
	workflowSuffix = "\n})()"
)

// workflowLauncher is the tool.Workflows over the Controller, kept off its exported API.
type workflowLauncher struct{ c *Controller }

func (l workflowLauncher) Start(_ context.Context, description, script string) (string, error) {
	return l.c.startWorkflow(description, script)
}

// workflowCall is the record of one agent() call. Its fields are guarded by
// Controller.mu, and a reader copies it.
type workflowCall struct {
	WorkflowAgent
	// sess is the agent's session while it is open, for its live usage; d is
	// how its tools display, and saved its transcript once it has closed.
	sess  *pigruntime.Session
	d     *display
	saved pigruntime.Saved
	stop  context.CancelCauseFunc
}

// workflowRun is one script, running or ended. Its agents are not subagents:
// they have no task of their own, are not in Controller.children and count for
// neither live nor maxAgents.
type workflowRun struct {
	c    *Controller
	task *tool.AgentTask
	// parent is the main session the run started under.
	parent *pigruntime.Session
	// bound is the relay emit of parent's epoch.
	bound  func(event.Event)
	cancel context.CancelCauseFunc
	slots  chan struct{}
	calls  atomic.Int32
	// agents are the run's calls by number, guarded by Controller.mu; ended is
	// guarded by Controller.queue.
	agents []*workflowCall
	ended  bool
}

// startWorkflow runs script in the background and returns its task id.
func (c *Controller) startWorkflow(description, script string) (string, error) {
	c.queue.Lock()
	c.mu.Lock()
	err := c.admitLocked()
	parent := c.main.sess
	c.mu.Unlock()
	if err == nil && c.runningWorkflowsLocked() >= c.cfg.MaxWorkflows {
		err = fmt.Errorf("%d %w", c.cfg.MaxWorkflows, ErrTooManyWorkflows)
	}
	if err != nil {
		c.queue.Unlock()
		return "", fmt.Errorf("failed to start workflow: %w", err)
	}
	// The run outlives the call: only a task stop, a session switch or Close
	// ends it.
	runCtx, cancel := context.WithCancelCause(context.Background())
	r := &workflowRun{c: c, parent: parent, bound: c.relay.bind(), cancel: cancel, slots: make(chan struct{}, c.cfg.MaxAgents)}
	r.task = c.cfg.Tasks.StartAgent(tool.WorkflowAgent, description, func() { cancel(nil) })
	if c.workflows == nil {
		c.workflows = map[string]*workflowRun{}
	}
	c.workflows[r.task.ID()] = r
	c.wg.Go(func() { r.run(runCtx, script) })
	c.queue.Unlock()
	c.relay.emit(event.TasksChanged{})
	return r.task.ID(), nil
}

func (r *workflowRun) run(ctx context.Context, script string) {
	value, err := pigruntime.RunScript(ctx, workflowPrefix+script+workflowSuffix, r.call)
	status, reason := tool.TaskCompleted, ""
	cause := context.Cause(ctx)
	switch {
	// The script may have caught the rejection that came with the cancel.
	case errors.Is(cause, ErrTooManyWorkflowCalls):
		status, reason = tool.TaskFailed, cause.Error()
	case ctx.Err() != nil:
		status = tool.TaskStopped
	case err != nil:
		status, reason = tool.TaskFailed, err.Error()
	}
	r.cancel(nil)
	r.end(status, reason, string(value))
}

// runningWorkflowsLocked counts the runs that have not ended; queue must be held.
func (c *Controller) runningWorkflowsLocked() int {
	n := 0
	for _, r := range c.workflows {
		if !r.ended {
			n++
		}
	}
	return n
}

// taskEnded is info, the end of this run, as the transcript draws it. A stopped
// run's calls that have not yet noticed the stop count as stopped.
func (r *workflowRun) taskEnded(info tool.TaskInfo) event.TaskEnded {
	shown := taskEnded(info)
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	var n agentCounts
	for _, a := range r.agents {
		n.add(a.State)
	}
	shown.Agents, shown.Failed = len(r.agents), n.Failed
	if info.Status == tool.TaskStopped {
		shown.Failed += n.Running + n.Queued
	}
	return shown
}

// end records the run's end and, in the queue section that deregisters it,
// reports a completed or failed run to the main session, only while that is
// still the session the run started under: a switch that installed another
// session meanwhile must not receive it.
func (r *workflowRun) end(status tool.TaskStatus, reason, value string) {
	c := r.c
	c.queue.Lock()
	info, reported := r.task.End(status, reason, value)
	r.ended = true
	if reported {
		shown := r.taskEnded(info)
		c.mu.Lock()
		current := c.main.sess == r.parent
		c.mu.Unlock()
		switch {
		case !current:
		case info.Status == tool.TaskCompleted:
			c.main.queueNoticeLocked(pigruntime.Notice{
				Text:  fmt.Sprintf("Workflow %s completed: %s\n%s", info.ID, commandLine(info.Command), info.Result),
				Shown: shown,
			})
		case info.Status == tool.TaskFailed:
			c.main.queueNoticeLocked(pigruntime.Notice{
				Text:  fmt.Sprintf("Workflow %s failed (%s): %s", info.ID, info.Reason, commandLine(info.Command)),
				Shown: shown,
			})
		}
	}
	c.queue.Unlock()
	c.relay.emit(event.TasksChanged{})
}

// workflowModel is the model a workflow agent of spec runs on, the only place
// one is chosen.
func (c *Controller) workflowModel(spec agentSpec) string {
	return cmp.Or(spec.def.Model, c.cfg.WorkflowModel, c.Model())
}

// call is one agent() call of the script.
func (r *workflowRun) call(ctx context.Context, raw json.RawMessage) (result json.RawMessage, err error) {
	c := r.c
	if r.calls.Add(1) > tool.WorkflowCalls {
		r.cancel(ErrTooManyWorkflowCalls)
		return nil, ErrTooManyWorkflowCalls
	}
	var arg struct {
		Prompt    string `json:"prompt"`
		AgentType string `json:"agentType"`
		Schema    any    `json:"schema"`
		Label     string `json:"label"`
		Phase     string `json:"phase"`
		N         int    `json:"n"`
	}
	if err := json.Unmarshal(raw, &arg); err != nil {
		return nil, fmt.Errorf("failed to read the arguments of agent(): %w", err)
	}

	// callCtx ends with the run's and, alone, when StopWorkflowAgent stops this call.
	callCtx, stop := context.WithCancelCause(ctx)
	label := commandLine(arg.Label)
	rec := &workflowCall{stop: stop, WorkflowAgent: WorkflowAgent{
		AgentStatus: AgentStatus{Agent: arg.AgentType, Description: label, State: WorkflowQueued},
		N:           arg.N,
		Phase:       commandLine(arg.Phase),
	}}
	c.mu.Lock()
	i, _ := slices.BinarySearchFunc(r.agents, rec, func(a, b *workflowCall) int { return cmp.Compare(a.N, b.N) })
	r.agents = slices.Insert(r.agents, i, rec)
	c.mu.Unlock()
	r.bound(event.TasksChanged{})

	var run *runner
	slotted := false
	// state is decided once the call has ended, after send or by the defer.
	var state tool.TaskStatus
	var reason string
	defer func() {
		if state == "" {
			state, reason = callOutcome(callCtx, err)
		}
		if err != nil && errors.Is(context.Cause(callCtx), ErrAgentStopped) {
			err = ErrAgentStopped
		}
		stop(nil)
		status := rec.AgentStatus
		if run != nil {
			status = c.agentStatus(status, run.sess)
			used := run.sess.UsageByModel()
			c.closeChild(run)
			c.charge(r.parent, used)
		}
		c.mu.Lock()
		rec.AgentStatus, rec.sess = status, nil
		rec.State, rec.Reason, rec.Ended = state, reason, time.Now()
		c.mu.Unlock()
		if run != nil {
			r.bound(event.SessionClosed{ID: rec.ID})
		}
		r.bound(event.TasksChanged{})
		if slotted {
			<-r.slots
		}
	}()

	select {
	case r.slots <- struct{}{}:
		slotted = true
	case <-callCtx.Done():
		return nil, context.Cause(callCtx)
	}
	c.mu.Lock()
	specs := c.workflowSpecs
	c.mu.Unlock()
	i = slices.IndexFunc(specs, func(s agentSpec) bool { return s.def.Name == arg.AgentType })
	if i < 0 {
		return nil, fmt.Errorf("failed to start agent: %w: %s", ErrUnknownAgentType, arg.AgentType)
	}
	spec := specs[i]
	tools := spec.tools
	var recorded structuredResult
	if arg.Schema != nil {
		schema, ok := arg.Schema.(map[string]any)
		if !ok || schema["type"] != "object" {
			return nil, ErrBadSchema
		}
		if c.cfg.Rules.Removes("StructuredOutput") {
			return nil, ErrStructuredOutputDenied
		}
		tools = append(slices.Clone(tools), tool.NewStructuredOutput(schema, recorded.set))
	}

	head := fmt.Sprintf("#%d %s", arg.N, cmp.Or(spec.def.Name, "agent"))
	name := fmt.Sprintf("workflow #%d", arg.N)
	if label != "" {
		head += " › " + label
		name += " " + label
	}
	agent := &runner{c: c, name: name, id: r.task.ID(), d: newDisplay(tools), model: c.workflowModel(spec), noFiles: spec.noFiles, bound: r.bound, emit: r.bound}
	if err := c.openChild(callCtx, spec, tools, r.parent, agent); err != nil {
		return nil, fmt.Errorf("failed to start agent: %w", err)
	}
	run = agent
	emit := func(ev event.Event) {
		run.rec.Observe(ev)
		r.bound(event.Session{ID: run.sess.ID(), Event: run.d.apply(ev)})
	}
	run.unsubscribe = run.sess.Subscribe(emit)
	c.mu.Lock()
	rec.sess, rec.saved, rec.d = run.sess, run.sess.Saved(), run.d
	rec.ID, rec.Model, rec.State, rec.Started = run.sess.ID(), run.model, tool.TaskRunning, time.Now()
	c.mu.Unlock()
	r.bound(event.TasksChanged{})

	r.writeLine(head + ": started")
	result, err = r.send(callCtx, run, emit, arg.Prompt, arg.Schema != nil, &recorded)
	state, reason = callOutcome(callCtx, err)
	switch state {
	case tool.TaskCompleted:
		r.writeLine(fmt.Sprintf("%s: completed · %s", head, time.Since(rec.Started).Round(time.Second)))
	case tool.TaskStopped:
		r.writeLine(head + ": stopped")
	default:
		r.writeLine(head + ": failed: " + reason)
	}
	return result, err
}

// callOutcome is the state a call that returned err ends in, and its reason
// when it failed.
func callOutcome(ctx context.Context, err error) (tool.TaskStatus, string) {
	switch {
	case err == nil:
		return tool.TaskCompleted, ""
	case errors.Is(err, ErrAgentStopped) || ctx.Err() != nil:
		return tool.TaskStopped, ""
	default:
		return tool.TaskFailed, err.Error()
	}
}

// send gives run's session its one prompt and returns the agent's result: the
// last StructuredOutput arguments with a schema, else its final message. A stop
// of the agent by a blank No on its approval ends only this send. emit receives
// the turn's start and end, which the session does not report.
func (r *workflowRun) send(ctx context.Context, run *runner, emit func(event.Event), prompt string, schema bool, recorded *structuredResult) (json.RawMessage, error) {
	c := r.c
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	run.work = &Work{sendCtx: sendCtx, cancel: cancel}
	c.mu.Unlock()
	emit(event.TurnStart{Prompt: prompt})
	start := time.Now()
	dirs := c.dirsText(0, c.dirCount(), !run.noFiles)
	res, err := run.sess.Send(sendCtx, pigruntime.Prompt{Text: prompt, FromAgent: true, Mode: dirs}, nil, nil)
	if err != nil {
		res.Outcome = event.Outcome{Status: event.Failed, Message: err.Error()}
	}
	emit(event.TurnEnd{Outcome: res.Outcome, Elapsed: time.Since(start)})
	switch {
	case err != nil:
		return nil, fmt.Errorf("failed to run agent: %w", err)
	case res.Outcome.Status == event.Interrupted:
		return nil, ErrAgentStopped
	case res.Outcome.Status == event.Failed:
		return nil, errors.New(res.Outcome.Message)
	case !schema:
		return json.Marshal(res.Reply)
	}
	if raw := recorded.get(); raw != nil {
		return raw, nil
	}
	return nil, ErrNoStructuredOutput
}

// Workflows describes the running workflows in task order.
func (c *Controller) Workflows() []WorkflowStatus {
	type snapshot struct {
		info   tool.TaskInfo
		agents []workflowCall
	}
	var snaps []snapshot
	// queue stays held so a run is listed only while its task is running.
	c.queue.Lock()
	for _, info := range c.cfg.Tasks.List() {
		if run := c.workflows[info.ID]; run != nil && info.Status == tool.TaskRunning {
			snaps = append(snaps, snapshot{info, run.copyAgents()})
		}
	}
	c.queue.Unlock()
	out := make([]WorkflowStatus, 0, len(snaps))
	for _, s := range snaps {
		status, _ := c.workflowStatus(s.info, s.agents)
		out = append(out, status)
	}
	return out
}

// Workflow describes the workflow run id of the current session, running or
// ended, and its agents in call order.
func (c *Controller) Workflow(id string) (WorkflowStatus, []WorkflowAgent, bool) {
	c.queue.Lock()
	run := c.workflows[id]
	if run == nil {
		c.queue.Unlock()
		return WorkflowStatus{}, nil, false
	}
	info, err := c.cfg.Tasks.Output(id)
	agents := run.copyAgents()
	c.queue.Unlock()
	if err != nil {
		return WorkflowStatus{}, nil, false
	}
	status, list := c.workflowStatus(info, agents)
	return status, list, true
}

// copyAgents copies the run's call records; the caller reads the live usage of
// each outside the locks.
func (r *workflowRun) copyAgents() []workflowCall {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	out := make([]workflowCall, len(r.agents))
	for i, a := range r.agents {
		out[i] = *a
	}
	return out
}

// workflowStatus is the run info's status and agents, the live ones with their
// current usage.
func (c *Controller) workflowStatus(info tool.TaskInfo, calls []workflowCall) (WorkflowStatus, []WorkflowAgent) {
	s := WorkflowStatus{ID: info.ID, Description: info.Command, State: info.Status, Started: info.Started, Ended: info.Ended}
	agents := make([]WorkflowAgent, 0, len(calls))
	var n agentCounts
	for _, call := range calls {
		a := call.WorkflowAgent
		a.AgentStatus = c.agentStatus(a.AgentStatus, call.sess)
		agents = append(agents, a)
		n.add(a.State)
		if a.State == tool.TaskRunning && a.Phase != "" {
			s.Phase = a.Phase
		}
	}
	s.Done, s.Failed, s.Running, s.Queued = n.Done, n.Failed, n.Running, n.Queued
	return s, agents
}

// agentCounts tallies a run's calls by state; a stopped call counts as failed.
type agentCounts struct{ Done, Failed, Running, Queued int }

func (n *agentCounts) add(state tool.TaskStatus) {
	switch state {
	case tool.TaskCompleted:
		n.Done++
	case tool.TaskFailed, tool.TaskStopped:
		n.Failed++
	case tool.TaskRunning:
		n.Running++
	case WorkflowQueued:
		n.Queued++
	}
}

// lookupCall is a copy of call n of workflow run id.
func (c *Controller) lookupCall(id string, n int) (workflowCall, bool) {
	c.queue.Lock()
	run := c.workflows[id]
	c.queue.Unlock()
	if run == nil {
		return workflowCall{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	i, ok := slices.BinarySearchFunc(run.agents, n, func(a *workflowCall, n int) int { return cmp.Compare(a.N, n) })
	if !ok {
		return workflowCall{}, false
	}
	return *run.agents[i], true
}

// StopWorkflowAgent stops call n of workflow run id, queued or running, and
// nothing else. A call that is unknown or has ended is left alone.
func (c *Controller) StopWorkflowAgent(id string, n int) {
	if a, ok := c.lookupCall(id, n); ok && (a.State == WorkflowQueued || a.State == tool.TaskRunning) {
		a.stop(ErrAgentStopped)
	}
}

// WorkflowHistory is the saved transcript of call n of workflow run id as
// display events, once the agent has ended.
func (c *Controller) WorkflowHistory(id string, n int) ([]event.Event, error) {
	call, found := c.lookupCall(id, n)
	switch {
	case !found:
		return nil, fmt.Errorf("failed to find workflow agent %s #%d: %w", id, n, tool.ErrUnknownTask)
	case call.State == WorkflowQueued || call.State == tool.TaskRunning:
		return nil, fmt.Errorf("failed to read workflow agent %s #%d: %w", id, n, ErrAgentRunning)
	case call.d == nil:
		return nil, fmt.Errorf("failed to read workflow agent %s #%d: %w", id, n, pigruntime.ErrNotSaved)
	}
	events, err := c.rt.SavedHistory(call.saved)
	if err != nil {
		return nil, fmt.Errorf("failed to read workflow agent %s #%d: %w", id, n, err)
	}
	for i, ev := range events {
		events[i] = call.d.apply(ev)
	}
	return events, nil
}

func (r *workflowRun) writeLine(line string) {
	_, _ = r.task.Write([]byte(line + "\n"))
}

// structuredResult holds the arguments of an agent's last StructuredOutput call.
type structuredResult struct {
	mu  sync.Mutex
	raw json.RawMessage
}

func (s *structuredResult) set(raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = slices.Clone(raw)
}

func (s *structuredResult) get() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw
}
