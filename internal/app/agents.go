package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

// maxAgents is how many subagents may run at once.
const maxAgents = 20

// neverChild are the tools a subagent never has, whatever its definition says.
// A subagent builds its own ToolSearch.
var neverChild = []string{"Agent", "SendMessage", "TaskStop", "ToolSearch", "Workflow"}

// agentSpec is what a subagent or a workflow agent opens with, computed once
// per process.
type agentSpec struct {
	def    subagent.Definition
	effort pigruntime.Effort
	tools  []tool.Tool
	prompt []pigruntime.PromptPart
}

// buildAgents computes the tools and prompt of each definition, in order, as
// subagents, and as workflow agents: those follow the default workflow agent,
// whose spec has no definition (and so an empty name) and exists even with no
// definitions. extra are built-in tools created with the root's session tools
// rather than in Config.AgentTools; serverTools and servers are the root's. All
// are already filtered by the rules.
func (c *Controller) buildAgents(extra, serverTools []tool.Tool, servers []mcpServer) (subagents, workflows []agentSpec) {
	base := slices.DeleteFunc(slices.Concat(c.cfg.AgentTools, extra), func(t tool.Tool) bool {
		return c.cfg.Rules.Removes(t.Name) || slices.Contains(neverChild, t.Name)
	})
	workflows = append(workflows, c.buildSpecs(base, serverTools, servers, nil)[1])
	for i := range c.cfg.Agents {
		both := c.buildSpecs(base, serverTools, servers, &c.cfg.Agents[i])
		subagents = append(subagents, both[0])
		workflows = append(workflows, both[1])
	}
	return subagents, workflows
}

// buildSpecs is def's spec as a subagent and as a workflow agent, which differ
// only in the prompt; nil def is the default workflow agent, which has every
// tool, the instruction files and the default effort.
func (c *Controller) buildSpecs(base, serverTools []tool.Tool, servers []mcpServer, def *subagent.Definition) [2]agentSpec {
	spec := agentSpec{effort: pigruntime.DefaultEffort}
	named := func(tool.Tool) bool { return true }
	groups := c.cfg.Instructions
	if def != nil {
		spec.def = *def
		if def.Effort != "" {
			// The definition's effort was validated when it was loaded.
			spec.effort, _ = pigruntime.ParseEffort(def.Effort)
		}
		if def.Tools != nil {
			named = func(t tool.Tool) bool { return slices.Contains(def.Tools, t.Name) }
		}
		if !def.Instructions {
			groups = nil
		}
	}
	tools := slices.DeleteFunc(slices.Clone(base), func(t tool.Tool) bool { return !named(t) })
	mcp := slices.DeleteFunc(slices.Clone(serverTools), func(t tool.Tool) bool { return !named(t) })
	if deferred := slices.DeleteFunc(slices.Clone(mcp), func(t tool.Tool) bool { return !t.Deferred }); len(deferred) > 0 {
		tools = append(tools, tool.NewToolSearch(deferred, pigruntime.RankTools, pigruntime.LoadTools))
	}
	tools = append(tools, mcp...)

	var childServers []mcpServer
	for _, s := range servers {
		own := slices.DeleteFunc(slices.Clone(s.tools), func(name string) bool {
			return !slices.ContainsFunc(mcp, func(t tool.Tool) bool { return t.Name == name })
		})
		if len(own) > 0 {
			childServers = append(childServers, mcpServer{name: s.name, instructions: s.instructions, tools: own})
		}
	}
	spec.tools = tools
	classified := c.cfg.Classifier != nil
	child := spec
	child.prompt = buildPrompt(c.cfg.Workspace, tools, groups, childServers, classified, def, false)
	spec.prompt = buildPrompt(c.cfg.Workspace, tools, groups, childServers, classified, def, true)
	return [2]agentSpec{child, spec}
}

// agentLauncher is the tool.Agents over the Controller, kept off its exported API.
type agentLauncher struct{ c *Controller }

func (l agentLauncher) Launch(ctx context.Context, agent, description, prompt string) (string, error) {
	return l.c.launch(ctx, agent, description, prompt)
}

func (l agentLauncher) Message(ctx context.Context, id, message string) (tool.TaskInfo, bool, error) {
	resumed, err := l.c.message(ctx, id, message)
	if err != nil {
		return tool.TaskInfo{}, false, err
	}
	to, _ := l.c.cfg.Tasks.Output(id)
	return to, resumed, nil
}

// subagentRole is the role of a subagent's session. The session lives until
// the root session is replaced or closed, so a later view can use its Subscribe,
// History and Send.
type subagentRole struct {
	r *runner
	// task is set by launch before the first run starts.
	task *tool.AgentTask
	// parent is the session the subagent was launched under.
	parent *pigruntime.Session

	outMu   sync.Mutex
	midLine bool
}

// begin counts the run in and marks the task running. The first run of a task
// is neither: its launch reserved the slot and started the task.
func (s *subagentRole) begin(w *Work) error {
	c := s.r.c
	if c.live >= maxAgents {
		return ErrTooManyAgents
	}
	c.live++
	if !s.task.Run(s.r.stopper(w)) {
		c.live--
		return tool.ErrUnknownTask
	}
	c.relay.emit(event.TasksChanged{})
	return nil
}

// end counts the run out and reports it in the queue section that deregisters
// it, so no observer sees neither a running subagent nor its notice.
func (s *subagentRole) end(w *Work, unread []pigruntime.Prompt) {
	c := s.r.c
	status, reason := tool.TaskCompleted, ""
	switch {
	case w.stopped || w.outcome.Status == event.Interrupted:
		status = tool.TaskStopped
	case w.outcome.Status == event.Failed:
		status, reason = tool.TaskFailed, w.outcome.Message
	}
	info, reported := s.task.End(status, reason, w.reply)
	c.live--
	c.signalLocked()
	if reported {
		texts := make([]string, 0, len(unread))
		for _, p := range unread {
			texts = append(texts, p.Text)
		}
		// A send-now cancel whose queued input the human took back ends the run
		// with nothing to continue: that is the human's stop.
		s.reportEnd(info, w.byHuman || !w.stopped && w.outcome.Status == event.Interrupted, texts)
	}
	c.relay.emit(event.TasksChanged{})
}

// emit feeds the classifier record, applies the display, writes the task's text
// log and reports the displayed event to the frontends as a session event. It
// is called from the session's subscriber and from the runner, so it must not
// block.
func (s *subagentRole) emit(ev event.Event) {
	s.r.rec.Observe(ev)
	ev = s.r.d.apply(ev)
	switch e := ev.(type) {
	case event.TurnStart:
		if e.Prompt != "" {
			s.writeLine("› ", e.Prompt)
		}
	case event.UserMessage:
		s.writeLine("› ", e.Text)
	case event.ToolStart:
		s.writeLine("• ", e.Title)
	case event.TextDelta:
		s.writeText(e.Text)
	}
	s.r.bound(event.Session{ID: s.r.id, Event: ev})
}

// writeLine puts a line of its own in the task's output.
func (s *subagentRole) writeLine(prefix, text string) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.midLine {
		_, _ = s.task.Write([]byte("\n"))
	}
	_, _ = s.task.Write([]byte(prefix + text + "\n"))
	s.midLine = false
}

func (s *subagentRole) writeText(text string) {
	if text == "" {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.task.Write([]byte(text))
	s.midLine = !strings.HasSuffix(text, "\n")
}

// admitLocked fails unless a subagent may start or run; it must be called with
// queue and mu held.
func (c *Controller) admitLocked() error {
	switch {
	case c.closed || c.main.sess == nil:
		return ErrNoSession
	case c.switching != nil:
		return ErrBusy
	case c.login != nil:
		return ErrLoggingIn
	}
	return nil
}

// openChild opens run's session under parent with tools: run.rec is its
// classifier record when a classifier is configured, and run.sess the session.
// A definition that pins plan mode gets an authority of its own.
func (c *Controller) openChild(ctx context.Context, spec agentSpec, tools []tool.Tool, parent *pigruntime.Session, run *runner) error {
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
		Parent:       parent,
		SystemPrompt: spec.prompt,
		Tools:        tools,
		Authority:    authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(run.d, run),
		Review:       c.reviewer(run.rec),
	})
	if err != nil {
		return err
	}
	c.offerSearch(sess)
	run.sess = sess
	return nil
}

// offerSearch makes WebSearch callable in sess while the model that would
// answer has hosted search.
func (c *Controller) offerSearch(sess *pigruntime.Session) {
	sess.SetCallable("WebSearch", c.rt.CanSearch(c.SmallModel()))
}

// launch starts a run of the subagent agent in the background and returns its
// task id.
func (c *Controller) launch(ctx context.Context, agent, description, prompt string) (string, error) {
	i := slices.IndexFunc(c.agents, func(s agentSpec) bool { return s.def.Name == agent })
	if i < 0 {
		return "", fmt.Errorf("failed to start subagent: %w: %s", tool.ErrUnknownAgent, agent)
	}
	spec := c.agents[i]

	c.queue.Lock()
	c.mu.Lock()
	err := c.admitLocked()
	parent := c.main.sess
	c.mu.Unlock()
	if err == nil && c.live >= maxAgents {
		err = ErrTooManyAgents
	}
	if err != nil {
		c.queue.Unlock()
		return "", fmt.Errorf("failed to start subagent %s: %w", agent, err)
	}
	c.live++
	c.queue.Unlock()

	model := spec.def.Model
	if model == "" {
		model = c.Model()
	}
	r := &runner{c: c, name: spec.def.Name, d: newDisplay(spec.tools), model: model}
	role := &subagentRole{r: r, parent: parent}
	r.role = role
	if err := c.openChild(ctx, spec, spec.tools, parent, r); err != nil {
		c.queue.Lock()
		c.live--
		c.queue.Unlock()
		return "", fmt.Errorf("failed to start subagent %s: %w", agent, err)
	}

	// A switch or Close that began meanwhile has run, or will run, StopAll
	// without a child to stop; it must not be left running. One that begins
	// after this section finds the child registered.
	c.queue.Lock()
	c.mu.Lock()
	err = c.admitLocked()
	if err == nil && c.main.sess != parent {
		err = ErrNoSession
	}
	if err != nil {
		c.live--
		c.mu.Unlock()
		c.queue.Unlock()
		c.closeChild(r)
		return "", fmt.Errorf("failed to start subagent %s: %w", agent, err)
	}
	r.work = r.newWork(context.Background())
	w := r.work
	c.mu.Unlock()
	role.task = c.cfg.Tasks.StartAgent(spec.def.Name, description, r.stopper(w))
	r.id, r.bound, r.emit = role.task.ID(), c.relay.bind(), role.emit
	r.unsubscribe = r.sess.Subscribe(role.emit)
	if c.children == nil {
		c.children = map[string]*runner{}
	}
	c.children[r.id] = r
	r.start(w, []pigruntime.Prompt{{Text: prompt, FromAgent: true}})
	c.queue.Unlock()
	c.relay.emit(event.TasksChanged{})
	return r.id, nil
}

// message delivers message to the subagent task id: it joins the running run
// at its next boundary, or starts the next run, which follows a run that is
// being stopped. It reports whether the subagent runs again because of it.
func (c *Controller) message(ctx context.Context, id, message string) (bool, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	r := c.children[id]
	if r == nil {
		return false, fmt.Errorf("failed to find task %q: %w", id, tool.ErrUnknownTask)
	}
	w, err := r.submitLocked(ctx, pigruntime.Prompt{Text: message, FromAgent: true}, false)
	if err != nil {
		return false, fmt.Errorf("failed to message subagent %s: %w", id, err)
	}
	c.mu.Lock()
	running := r.work
	c.mu.Unlock()
	return w != nil || running != nil && running.stopped, nil
}

// reportEnd receives the end of a run the task registry reported: the task as
// it then is, whether the human stopped the run, and the messages it never
// read. It queues the end notice on the main runner, only while the parent is
// still the current session: a switch that installed another session after the
// run ended must not receive it. It must be called with queue held.
func (s *subagentRole) reportEnd(info tool.TaskInfo, byHuman bool, unread []string) {
	c := s.r.c
	c.mu.Lock()
	current := c.main.sess == s.parent
	c.mu.Unlock()
	if !current {
		return
	}
	switch {
	case info.Status == tool.TaskCompleted || info.Status == tool.TaskFailed:
		c.main.queueNoticeLocked(agentNotice(info, unread))
	case info.Status == tool.TaskStopped && byHuman:
		c.main.queueNoticeLocked(withUndelivered(pigruntime.Notice{Text: agentStoppedHead(info), Shown: taskEnded(info)}, unread))
	}
}

// agentStoppedHead is the first line of the notice for an agent the human stopped.
func agentStoppedHead(info tool.TaskInfo) string {
	return fmt.Sprintf("Agent %s (%s) stopped by the user: %s", info.ID, info.Agent, commandLine(info.Command))
}

// withUndelivered appends the messages a run ended without reading, which
// SendMessage had reported as queued.
func withUndelivered(n pigruntime.Notice, unread []string) pigruntime.Notice {
	if len(unread) > 0 {
		n.Text += "\nUndelivered messages:\n" + strings.Join(unread, "\n\n")
	}
	return n
}

// agentNotice is what the model reads when a subagent's run completed or
// failed; unread are the messages a failed run never read.
func agentNotice(info tool.TaskInfo, unread []string) pigruntime.Notice {
	text := fmt.Sprintf("Agent %s (%s) failed (%s): %s", info.ID, info.Agent, info.Reason, commandLine(info.Command))
	if info.Status == tool.TaskCompleted {
		text = fmt.Sprintf("Agent %s (%s) completed: %s\n%s", info.ID, info.Agent, commandLine(info.Command), info.Result)
	}
	return withUndelivered(pigruntime.Notice{Text: text, Shown: taskEnded(info)}, unread)
}

// closeChildren ends the children of the session being replaced or closed:
// their tasks have been stopped, so no child or workflow run is left. A switch
// or Close registers no child concurrently, so none is missed.
func (c *Controller) closeChildren() {
	c.queue.Lock()
	children := c.children
	c.children = nil
	c.workflows = nil
	c.mu.Lock()
	c.approvals = slices.DeleteFunc(c.approvals, func(p *pendingApproval) bool { return p.runner != c.main })
	c.mu.Unlock()
	for _, r := range children {
		c.mu.Lock()
		w, emit := r.work, r.emit
		c.mu.Unlock()
		if w != nil {
			r.stop(w, emit)
		}
	}
	c.queue.Unlock()
	for _, r := range children {
		c.closeChild(r)
	}
}

func (c *Controller) closeChild(r *runner) {
	if r.unsubscribe != nil {
		r.unsubscribe()
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.JoinTimeout)
	defer cancel()
	if err := r.sess.Close(ctx); err != nil {
		slog.Warn("failed to close subagent session", "agent", r.name, "err", err)
	}
}
