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
var neverChild = []string{"Agent", "SendMessage", "TaskOutput", "TaskStop", "ToolSearch"}

// agentSpec is what a subagent opens with, computed once per process.
type agentSpec struct {
	def    subagent.Definition
	effort pigruntime.Effort
	tools  []tool.Tool
	prompt []pigruntime.PromptPart
}

// buildAgents computes the tools and prompt of each definition, in order.
// serverTools and servers are the root's, already filtered by the rules.
func (c *Controller) buildAgents(serverTools []tool.Tool, servers []mcpServer) []agentSpec {
	base := slices.DeleteFunc(slices.Clone(c.cfg.AgentTools), func(t tool.Tool) bool {
		return c.cfg.Rules.Removes(t.Name) || slices.Contains(neverChild, t.Name)
	})
	var specs []agentSpec
	for _, def := range c.cfg.Agents {
		effort := pigruntime.DefaultEffort
		if def.Effort != "" {
			// The definition's effort was validated when it was loaded.
			effort, _ = pigruntime.ParseEffort(def.Effort)
		}
		named := func(t tool.Tool) bool { return def.Tools == nil || slices.Contains(def.Tools, t.Name) }
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
		groups := c.cfg.Instructions
		if !def.Instructions {
			groups = nil
		}
		prompt := buildPrompt(c.cfg.Workspace, tools, groups, childServers, c.cfg.Classifier != nil, &def)
		specs = append(specs, agentSpec{def: def, effort: effort, tools: tools, prompt: prompt})
	}
	return specs
}

// agentLauncher is the tool.Agents over the Controller, kept off its exported API.
type agentLauncher struct{ c *Controller }

func (l agentLauncher) Launch(ctx context.Context, agent, description, prompt string) (string, error) {
	return l.c.launch(ctx, agent, description, prompt)
}

func (l agentLauncher) Message(ctx context.Context, id, message string) (bool, error) {
	return l.c.message(ctx, id, message)
}

// child is a running or ended subagent of the current session. Its session
// lives until the root session is replaced or closed, so a later view can use
// its Subscribe, History and Send.
type child struct {
	spec        agentSpec
	parent      *pigruntime.Session
	d           *display
	rec         *classifier.Record
	sess        *pigruntime.Session
	unsubscribe func()
	task        *tool.AgentTask

	outMu   sync.Mutex
	midLine bool

	// The fields below are guarded by Controller.queue.

	running bool
	// ending is set once the run has decided to end and until it has: a
	// message then waits for the end instead of joining the inbox.
	ending bool
	// inbox holds the messages queued for the running run.
	inbox []string
	// runCtx and cancel belong to the current run and are independent of the
	// root's turn.
	runCtx context.Context
	cancel context.CancelFunc
	// userStopped is set when the human rejected an ask without a reason.
	userStopped bool
	// endDone closes when the current run has ended.
	endDone chan struct{}
}

// writeLine puts a line of its own in the task's output.
func (ch *child) writeLine(prefix, text string) {
	ch.outMu.Lock()
	defer ch.outMu.Unlock()
	if ch.midLine {
		_, _ = ch.task.Write([]byte("\n"))
	}
	_, _ = ch.task.Write([]byte(prefix + text + "\n"))
	ch.midLine = false
}

func (ch *child) writeText(text string) {
	if text == "" {
		return
	}
	ch.outMu.Lock()
	defer ch.outMu.Unlock()
	_, _ = ch.task.Write([]byte(text))
	ch.midLine = !strings.HasSuffix(text, "\n")
}

// observe is the child session's subscriber.
func (ch *child) observe(ev event.Event) {
	ch.rec.Observe(ev)
	switch e := ch.d.apply(ev).(type) {
	case event.UserMessage:
		ch.writeLine("› ", e.Text)
	case event.ToolStart:
		ch.writeLine("• ", e.Title)
	case event.TextDelta:
		ch.writeText(e.Text)
	}
}

// givePrompt records a prompt the session does not emit, which is the first of
// each send.
func (ch *child) givePrompt(text string) {
	ch.writeLine("› ", text)
	ch.rec.Observe(event.TurnStart{Prompt: text})
}

// admitLocked fails unless a subagent may start or run; it must be called with
// queue and mu held.
func (c *Controller) admitLocked() error {
	switch {
	case c.closed || c.sess == nil:
		return ErrNoSession
	case c.switching != nil:
		return ErrBusy
	case c.login != nil:
		return ErrLoggingIn
	}
	return nil
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
	parent := c.sess
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

	ch := &child{spec: spec, parent: parent, d: newDisplay(spec.tools)}
	ch.runCtx, ch.cancel = context.WithCancel(context.Background())
	if c.cfg.Classifier != nil {
		ch.rec = &classifier.Record{}
	}
	authority := c.authority
	if spec.def.Plan {
		authority = policy.NewAuthority()
	}
	model := spec.def.Model
	if model == "" {
		model = c.cfg.Model
	}
	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        model,
		Effort:       spec.effort,
		Parent:       parent,
		SystemPrompt: spec.prompt,
		Tools:        spec.tools,
		Authority:    authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(ch.d, ch),
		Review:       c.reviewer(ch.rec),
	})
	if err != nil {
		ch.cancel()
		c.queue.Lock()
		c.live--
		c.queue.Unlock()
		return "", fmt.Errorf("failed to start subagent %s: %w", agent, err)
	}
	ch.sess = sess

	// A switch or Close that began meanwhile has run, or will run, StopAll
	// without a child to stop; it must not be left running. One that begins
	// after this section finds the child registered.
	c.queue.Lock()
	c.mu.Lock()
	err = c.admitLocked()
	if err == nil && c.sess != parent {
		err = ErrNoSession
	}
	if err != nil {
		c.live--
		c.mu.Unlock()
		c.queue.Unlock()
		ch.cancel()
		c.closeChild(ch)
		return "", fmt.Errorf("failed to start subagent %s: %w", agent, err)
	}
	c.mu.Unlock()
	ch.task = c.cfg.Tasks.StartAgent(spec.def.Name, description, ch.cancel)
	ch.unsubscribe = sess.Subscribe(ch.observe)
	ch.givePrompt(prompt)
	if c.children == nil {
		c.children = map[string]*child{}
	}
	c.children[ch.task.ID()] = ch
	ch.running = true
	ch.endDone = make(chan struct{})
	c.wg.Go(func() { c.runChild(ch, ch.runCtx, prompt) })
	c.queue.Unlock()
	c.relay.emit(event.TasksChanged{})
	return ch.task.ID(), nil
}

// message delivers message to the subagent task id.
func (c *Controller) message(ctx context.Context, id, message string) (bool, error) {
	for {
		c.queue.Lock()
		ch := c.children[id]
		if ch == nil {
			c.queue.Unlock()
			return false, fmt.Errorf("failed to find task %q: %w", id, tool.ErrUnknownTask)
		}
		c.mu.Lock()
		err := c.admitLocked()
		c.mu.Unlock()
		if err != nil {
			c.queue.Unlock()
			return false, fmt.Errorf("failed to message subagent %s: %w", id, err)
		}
		switch {
		case ch.running && !ch.ending && ch.runCtx.Err() == nil:
			ch.inbox = append(ch.inbox, message)
			c.queue.Unlock()
			return false, nil
		case ch.running:
			// The run is ending or being stopped: a message for it would be dropped.
			done := ch.endDone
			c.queue.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return false, fmt.Errorf("failed to message subagent %s: %w", id, ctx.Err())
			}
		}
		if c.live >= maxAgents {
			c.queue.Unlock()
			return false, fmt.Errorf("failed to message subagent %s: %w", id, ErrTooManyAgents)
		}
		c.live++
		runCtx, cancel := context.WithCancel(context.Background())
		ch.running, ch.userStopped, ch.runCtx, ch.cancel, ch.endDone = true, false, runCtx, cancel, make(chan struct{})
		c.wg.Add(1)
		c.queue.Unlock()

		if !ch.task.Run(cancel) {
			c.queue.Lock()
			ch.running = false
			c.live--
			close(ch.endDone)
			c.queue.Unlock()
			cancel()
			c.wg.Done()
			return false, fmt.Errorf("failed to find task %q: %w", id, tool.ErrUnknownTask)
		}
		ch.givePrompt(message)
		go func() {
			defer c.wg.Done()
			c.runChild(ch, runCtx, message)
		}()
		c.relay.emit(event.TasksChanged{})
		return true, nil
	}
}

// runChild sends prompt and then whatever was messaged meanwhile, until
// nothing is left, and ends the run. ctx is the run's own, never the root's.
func (c *Controller) runChild(ch *child, ctx context.Context, prompt string) {
	for {
		res, err := ch.sess.Send(ctx, pigruntime.Prompt{Text: prompt}, nil, func() ([]pigruntime.Prompt, []pigruntime.Notice) {
			c.queue.Lock()
			defer c.queue.Unlock()
			if ctx.Err() != nil {
				return nil, nil
			}
			prompts := make([]pigruntime.Prompt, 0, len(ch.inbox))
			for _, m := range ch.inbox {
				prompts = append(prompts, pigruntime.Prompt{Text: m})
			}
			ch.inbox = nil
			return prompts, nil
		})
		if err != nil {
			res.Outcome = event.Outcome{Status: event.Failed, Message: err.Error()}
		}

		c.queue.Lock()
		if ctx.Err() == nil && res.Outcome.Status == event.Completed {
			var texts []string
			for _, p := range res.Undelivered {
				texts = append(texts, p.Text)
			}
			texts = append(texts, ch.inbox...)
			ch.inbox = nil
			if len(texts) > 0 {
				c.queue.Unlock()
				prompt = strings.Join(texts, "\n\n")
				ch.givePrompt(prompt)
				continue
			}
		}
		ch.ending = true
		c.queue.Unlock()

		status, reason := tool.TaskCompleted, ""
		switch res.Outcome.Status {
		case event.Interrupted:
			status = tool.TaskStopped
		case event.Failed:
			status, reason = tool.TaskFailed, res.Outcome.Message
		}
		info, reported := ch.task.End(status, reason, res.Reply)

		// Counting the run out and queueing its notice are one section, so no
		// observer sees neither a running child nor the notice.
		c.queue.Lock()
		ch.running, ch.ending = false, false
		unread := ch.inbox
		ch.inbox = nil
		c.live--
		close(ch.endDone)
		if reported {
			c.queueEndNoticeLocked(ch, info, unread)
		}
		c.signalLocked()
		c.queue.Unlock()
		c.relay.emit(event.TasksChanged{})
		return
	}
}

// queueEndNoticeLocked queues the notice for a run that ended, only while the
// session the child belongs to is still the current one: a switch that
// installed another session after End must not receive it. It must be called
// with queue held.
func (c *Controller) queueEndNoticeLocked(ch *child, info tool.TaskInfo, unread []string) {
	c.mu.Lock()
	current := c.sess == ch.parent
	c.mu.Unlock()
	if !current {
		return
	}
	switch {
	case info.Status == tool.TaskCompleted || info.Status == tool.TaskFailed:
		c.queueNoticeLocked(agentNotice(info, unread))
	case info.Status == tool.TaskStopped && ch.userStopped:
		c.queueNoticeLocked(withUndelivered(notice(info, agentStoppedHead(info)), unread))
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
// their tasks have been stopped, so no run is left. A switch or Close
// registers no child concurrently, so none is missed.
func (c *Controller) closeChildren() {
	c.queue.Lock()
	children := c.children
	c.children = nil
	c.mu.Lock()
	c.approvals = slices.DeleteFunc(c.approvals, func(p *pendingApproval) bool { return p.child != nil })
	c.mu.Unlock()
	for _, ch := range children {
		ch.cancel()
	}
	c.queue.Unlock()
	for _, ch := range children {
		c.closeChild(ch)
	}
}

func (c *Controller) closeChild(ch *child) {
	if ch.unsubscribe != nil {
		ch.unsubscribe()
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.JoinTimeout)
	defer cancel()
	if err := ch.sess.Close(ctx); err != nil {
		slog.Warn("failed to close subagent session", "agent", ch.spec.def.Name, "err", err)
	}
}
