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

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// workflowPrefix and workflowSuffix wrap a script's code. The prefix is one
// line, so the script's line numbers stay its own, and defines agent, parallel
// and pipeline over the host global.
const (
	workflowPrefix = `let __n=0;const agent=(prompt,o)=>host({...o,prompt,n:++__n}),parallel=ts=>Promise.all(ts.map(t=>Promise.resolve().then(()=>typeof t==="function"?t():t).catch(()=>null))),pipeline=(items,...stages)=>parallel(items.map((item,i)=>async()=>{let v=item;for(const s of stages)v=await s(v,item,i);return v}));return(async()=>{`
	workflowSuffix = "\n})()"
)

// workflowLauncher is the tool.Workflows over the Controller, kept off its exported API.
type workflowLauncher struct{ c *Controller }

func (l workflowLauncher) Start(_ context.Context, description, script string) (string, error) {
	return l.c.startWorkflow(description, script)
}

// workflowRun is one running script. Its agents are not subagents: they have
// no task of their own, are not in Controller.children and count for neither
// live nor maxAgents.
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
}

// startWorkflow runs script in the background and returns its task id.
func (c *Controller) startWorkflow(description, script string) (string, error) {
	c.queue.Lock()
	c.mu.Lock()
	err := c.admitLocked()
	parent := c.main.sess
	c.mu.Unlock()
	if err == nil && len(c.workflowRuns) >= c.cfg.MaxWorkflows {
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
	if c.workflowRuns == nil {
		c.workflowRuns = map[string]struct{}{}
	}
	c.workflowRuns[r.task.ID()] = struct{}{}
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

// end records the run's end and, in the queue section that deregisters it,
// reports a completed or failed run to the main session, only while that is
// still the session the run started under: a switch that installed another
// session meanwhile must not receive it.
func (r *workflowRun) end(status tool.TaskStatus, reason, value string) {
	c := r.c
	c.queue.Lock()
	info, reported := r.task.End(status, reason, value)
	delete(c.workflowRuns, r.task.ID())
	if reported {
		c.mu.Lock()
		current := c.main.sess == r.parent
		c.mu.Unlock()
		switch {
		case !current:
		case info.Status == tool.TaskCompleted:
			c.main.queueNoticeLocked(pigruntime.Notice{
				Text:  fmt.Sprintf("Workflow %s completed: %s\n%s", info.ID, commandLine(info.Command), info.Result),
				Shown: taskEnded(info),
			})
		case info.Status == tool.TaskFailed:
			c.main.queueNoticeLocked(pigruntime.Notice{
				Text:  fmt.Sprintf("Workflow %s failed (%s): %s", info.ID, info.Reason, commandLine(info.Command)),
				Shown: taskEnded(info),
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
func (r *workflowRun) call(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
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
		N         int    `json:"n"`
	}
	if err := json.Unmarshal(raw, &arg); err != nil {
		return nil, fmt.Errorf("failed to read the arguments of agent(): %w", err)
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	i := slices.IndexFunc(c.workflowSpecs, func(s agentSpec) bool { return s.def.Name == arg.AgentType })
	if i < 0 {
		return nil, fmt.Errorf("failed to start agent: %w: %s", ErrUnknownAgentType, arg.AgentType)
	}
	spec := c.workflowSpecs[i]
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

	label := commandLine(arg.Label)
	head := fmt.Sprintf("#%d %s", arg.N, cmp.Or(spec.def.Name, "agent"))
	name := fmt.Sprintf("workflow #%d", arg.N)
	if label != "" {
		head += " › " + label
		name += " " + label
	}
	run := &runner{c: c, name: name, id: r.task.ID(), d: newDisplay(tools), model: c.workflowModel(spec), bound: r.bound, emit: r.bound}
	if c.cfg.Classifier != nil {
		run.rec = &classifier.Record{}
	}
	authority := c.authority
	if spec.def.Plan {
		authority = policy.NewAuthority()
	}
	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        run.model,
		Effort:       spec.effort,
		Parent:       r.parent,
		SystemPrompt: spec.prompt,
		Tools:        tools,
		Authority:    authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(run.d, run),
		Review:       c.reviewer(run.rec),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start agent: %w", err)
	}
	run.sess = sess
	if run.rec != nil {
		run.unsubscribe = sess.Subscribe(run.rec.Observe)
	}
	defer c.closeChild(run)

	r.writeLine(head + ": started")
	started := time.Now()
	result, err := r.send(ctx, run, arg.Prompt, arg.Schema != nil, &recorded)
	switch {
	case err == nil:
		r.writeLine(fmt.Sprintf("%s: completed · %s", head, time.Since(started).Round(time.Second)))
	case errors.Is(err, ErrAgentStopped) || ctx.Err() != nil:
		r.writeLine(head + ": stopped")
	default:
		r.writeLine(head + ": failed: " + err.Error())
	}
	return result, err
}

// send gives run's session its one prompt and returns the agent's result: the
// last StructuredOutput arguments with a schema, else its final message. A stop
// of the agent by a blank No on its approval ends only this send.
func (r *workflowRun) send(ctx context.Context, run *runner, prompt string, schema bool, recorded *structuredResult) (json.RawMessage, error) {
	c := r.c
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	run.work = &Work{sendCtx: sendCtx, cancel: cancel}
	c.mu.Unlock()
	run.rec.Observe(event.TurnStart{Prompt: prompt})
	res, err := run.sess.Send(sendCtx, pigruntime.Prompt{Text: prompt, FromAgent: true}, nil, nil)
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
