package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type Config struct {
	Model     string // "provider/id"
	Workspace string
	// Tools are the built-in tools; the MCP servers' tools come from Servers.
	Tools []tool.Tool
	// JoinTimeout bounds how long a session switch or Close waits for running
	// work to stop.
	JoinTimeout time.Duration
	// History records human input; nil disables it.
	History *history.Store
	// Rules nil means no rules.
	Rules *policy.Rules
	// Unattended means no human can answer, so every call that would ask for
	// approval is denied.
	Unattended bool
	// Mode is the controller's initial mode, which the first session starts
	// in, and the mode every later session starts in.
	Mode policy.Mode
	// Files lists the entries that @ references complete to; nil offers none.
	Files *tool.Workspace
	// Instructions are added to the system prompt in order: the global
	// file's group first, then each root's.
	Instructions []instruction.Group
	// Errors are reported once as Error events, after the first session
	// opens, before those about the MCP servers.
	Errors []error
	// Servers waits for the MCP servers to settle and returns their statuses;
	// Open or Start calls it once, before the first session. Open returns its
	// error unchanged; Start reports it as an Error event. Nil means there are
	// no servers.
	Servers func(ctx context.Context) ([]pigruntime.ServerStatus, error)
	// Tasks is the background task manager the Bash tool shares; nil means one
	// no tool shares, so there are never tasks.
	Tasks Tasks
	// Wake lets a task's end start a turn on its own; otherwise its notice
	// waits for a request a human prompt starts.
	Wake bool
	// Classifier reviews the calls that would otherwise ask; nil means none
	// are reviewed.
	Classifier *classifier.Classifier
}

const maxReferenceEntries = 10000

// Controller holds all frontend-neutral behaviour: frontends pass input in
// and render the events they subscribe to.
//
// Lock order is life, then queue, then mu. mu is never held while calling
// into PiG or delivering events; life is held across both and queue across
// delivering events, which is safe because subscribers must not call back into
// the Controller. Neither queue nor mu is held while calling Config.Tasks,
// whose callback takes both.
type Controller struct {
	rt  *pigruntime.Runtime
	cfg Config
	// tools and prompt are what every session opens with; set before the
	// first session and not changed after.
	tools     []tool.Tool
	prompt    []pigruntime.PromptPart
	authority *policy.Authority
	relay     relay
	wg        sync.WaitGroup
	// startup is what the first open that succeeds reports, so a failed first
	// open does not lose it; life guards it once a session can be opened.
	startup []event.Event
	// attempted is set once the first open has been tried or its wait for the
	// servers has failed; life guards it.
	attempted bool

	// life serialises session start, switch and close.
	life sync.Mutex
	// stopAllDone retains a task cleanup across timed-out Close calls; life guards it.
	stopAllDone chan struct{}
	// queue makes queueing input atomic with taking it from pending and with
	// deciding to stop, continue or end the running Work; it guards pending
	// and the Work's queue fields. PiG calls back under its own lock to take
	// pending, so nothing may call into PiG with queue held.
	queue sync.Mutex
	// pending is the input queued for the running turn, the only source of
	// QueueChanged. It is non-empty only while a turn is registered, or once
	// the first open was started by Start and until a session opens and takes
	// it.
	pending []pigruntime.Prompt
	// notices are the task notices waiting for the model, also guarded by
	// queue. They are never part of pending: they are not the human's input,
	// so they are never reported, handed back or taken.
	notices []pigruntime.Notice
	// hold, guarded by queue, keeps notices from waking the model after an
	// Interrupt until a human prompt starts a turn.
	hold bool

	mu     sync.Mutex
	sess   *pigruntime.Session
	sessID string
	// emit is bound to sess's relay epoch.
	emit        func(event.Event)
	unsubscribe func()
	// switching refuses prompts while a session transition is under way, so
	// no work can start on a session that is about to be replaced.
	switching *sessionSwitch
	// closed is set by Close; afterwards the Controller refuses all input,
	// leaves the mode alone and delivers no events.
	closed  bool
	turn    *Work
	turnSeq int
	// approval is the call waiting for Answer; nil when none is.
	approval    *pendingApproval
	approvalSeq uint64
	// login is the running login; nil when none is. A login and a turn never
	// run together: each is refused while the other is registered.
	login    *loginRun
	loginSeq uint64
	// deferred is the first open that Start put off for a login; cleared once
	// any session opens.
	deferred *OpenRequest
}

// sessionSwitch retains work even after it deregisters while start waits for life.
type sessionSwitch struct {
	work *Work
	emit func(event.Event)
	// cancelWait is set only for the first open started by Start, during which
	// prompts are queued rather than refused; Close calls it to end the wait
	// for the servers.
	cancelWait context.CancelFunc
}

func NewController(rt *pigruntime.Runtime, cfg Config) *Controller {
	cfg.Tools = slices.DeleteFunc(slices.Clone(cfg.Tools), func(t tool.Tool) bool { return cfg.Rules.Removes(t.Name) })
	if cfg.Tasks == nil {
		cfg.Tasks = tool.NewTasks()
	}
	c := &Controller{rt: rt, cfg: cfg, authority: policy.NewAuthority()}
	if cfg.Mode == policy.Execute {
		c.authority.EnterExecute()
	}
	c.setup(nil)
	cfg.Tasks.OnChange(c.taskChanged)
	cfg.Tasks.OnForeground(func() { c.relay.emit(event.TasksChanged{}) })
	return c
}

// setup sets the session tools and prompt from the built-in tools and
// statuses, and returns the events about the servers.
func (c *Controller) setup(statuses []pigruntime.ServerStatus) []event.Event {
	var events []event.Event
	var serverTools []tool.Tool
	var servers []mcpServer
	for _, st := range statuses {
		if st.Err != nil {
			events = append(events, event.Error{Err: fmt.Errorf("MCP server %s failed to start: %w", st.Name, st.Err)})
			continue
		}
		server := mcpServer{name: st.Name, instructions: st.Instructions}
		for _, t := range st.Tools {
			if c.cfg.Rules.Removes(t.Name) {
				continue
			}
			serverTools = append(serverTools, t)
			server.tools = append(server.tools, t.Name)
		}
		for _, skipped := range st.Skipped {
			events = append(events, event.Notice{Text: fmt.Sprintf("MCP server %s: skipped tool %s", st.Name, skipped)})
		}
		servers = append(servers, server)
	}
	tools := slices.Clone(c.cfg.Tools)
	if len(serverTools) > 0 {
		search := tool.NewToolSearch(serverTools, pigruntime.RankTools, pigruntime.LoadTools)
		if c.cfg.Rules.Removes(search.Name) {
			// ToolSearch is the only way to load deferred tools.
			serverTools = nil
			for i := range servers {
				servers[i].tools = nil
			}
		} else {
			tools = append(tools, search)
		}
	}
	tools = append(tools, serverTools...)
	c.tools = tools
	c.prompt = systemPrompt(c.cfg.Workspace, c.tools, c.cfg.Instructions, servers, c.cfg.Classifier != nil)
	return events
}

// prepareStartup sets up the first session from statuses and keeps what it
// reports once open: Config.Errors, then the events about the servers.
func (c *Controller) prepareStartup(statuses []pigruntime.ServerStatus) {
	for _, err := range c.cfg.Errors {
		c.startup = append(c.startup, event.Error{Err: err})
	}
	c.startup = append(c.startup, c.setup(statuses)...)
}

// Subscribe registers fn for every event of the current session plus
// controller events. fn runs synchronously; it must not block and must not
// call back into the Controller.
func (c *Controller) Subscribe(fn func(event.Event)) func() {
	return c.relay.subscribe(fn)
}

type OpenRequest struct {
	ResumeID string
	Continue bool
}

// Open checks that req can be opened, waits for the MCP servers, opens the
// first session with their tools, then emits each of Config.Errors and the
// events about the servers.
func (c *Controller) Open(ctx context.Context, req OpenRequest) error {
	if err := c.check(ctx, req); err != nil {
		return err
	}
	statuses, err := c.servers(ctx)
	if err != nil {
		return err
	}
	c.prepareStartup(statuses)
	return c.start(ctx, req)
}

// Start is Open without waiting: it returns the check's error, and otherwise
// returns at once and opens the session in the background, reporting failure
// as an Error event. Prompts given until the session opens are queued and
// start its first turn; /new and /resume are refused with ErrBusy meanwhile.
// The wait for the servers runs without life, so Close can end it.
//
// When the check fails only because the model's provider has no credential,
// an attended Start instead returns nil and runs that provider's login, as
// /login does; the open follows once it succeeds. If the login is cancelled or
// fails, no session opens and a later /login that succeeds retries the open.
func (c *Controller) Start(ctx context.Context, req OpenRequest) error {
	if err := c.check(ctx, req); err != nil {
		if !c.canLoginFirst(ctx, err) {
			return err
		}
		return c.loginFirst(ctx, req, c.withLoginHint(err))
	}
	return c.startFirst(ctx, req, nil)
}

// startFirst opens the first session in the background; notices are reported
// after it opens, ahead of the other startup events.
func (c *Controller) startFirst(ctx context.Context, req OpenRequest, notices []event.Event) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	op, err := c.beginSwitch(cancel)
	if err != nil {
		cancel()
		return err
	}
	if c.cfg.Servers != nil {
		c.relay.emit(event.Activity{Text: "Connecting to MCP servers…"})
	}
	go func() {
		defer cancel()
		statuses, err := c.servers(ctx)
		if c.cfg.Servers != nil {
			c.relay.emit(event.Activity{})
		}
		c.life.Lock()
		defer c.life.Unlock()
		if err == nil {
			c.startup = append(c.startup, notices...)
			c.prepareStartup(statuses)
			err = c.startSwitch(ctx, req, op)
		} else {
			c.attempted = true
			c.mu.Lock()
			c.switching = nil
			c.mu.Unlock()
			for _, ev := range notices {
				c.relay.emit(ev)
			}
		}
		if err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
	return nil
}

func (c *Controller) check(ctx context.Context, req OpenRequest) error {
	return c.rt.Check(ctx, pigruntime.OpenOptions{Model: c.cfg.Model, ResumeID: req.ResumeID, Continue: req.Continue})
}

func (c *Controller) servers(ctx context.Context) ([]pigruntime.ServerStatus, error) {
	if c.cfg.Servers == nil {
		return nil, nil
	}
	return c.cfg.Servers(ctx)
}

func (c *Controller) NewSession(ctx context.Context) error {
	return c.start(ctx, OpenRequest{})
}

func (c *Controller) Resume(ctx context.Context, id string) error {
	return c.start(ctx, OpenRequest{ResumeID: id})
}

// switchTo starts the switch to req in the background; its failure arrives as
// an Error event. It marks the switch before returning, so input given
// meanwhile is refused rather than landing on the session being replaced. The
// goroutine is not in wg: Close holds life across wg.Wait, and start needs
// life. start refuses once closed, and Close shuts the relay before releasing
// life, so nothing reaches the runtime or a subscriber after Close.
func (c *Controller) switchTo(ctx context.Context, req OpenRequest) error {
	op, err := c.beginSwitch(nil)
	if err != nil {
		return err
	}
	go func() {
		c.life.Lock()
		defer c.life.Unlock()
		if err := c.startSwitch(ctx, req, op); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
	return nil
}

func (c *Controller) Sessions() ([]pigruntime.SessionInfo, error) {
	return c.rt.Sessions()
}

// Submit takes one message of human input with the paths of its images. A
// control command is executed and returns (nil, nil), ErrQuit for /quit,
// ErrShowTasks for /tasks, ErrMissingArgument for /resume, /login or /logout
// without an argument, or ErrUnknownCommand. /new, /resume with an id and
// /login with a provider return at once and report their result through
// events; while a login runs, /new and /resume return ErrLoggingIn.
// Anything else is a prompt: it starts a root turn or, while one runs, is
// queued for that turn and returns (nil, nil); while a login runs it returns
// ErrLoggingIn. After Close every input but a
// blank line returns ErrNoSession.
func (c *Controller) Submit(ctx context.Context, text string, images ...string) (*Work, error) {
	return c.input(ctx, text, images, false)
}

// SendNow is Submit, except that while a turn runs it stops the turn's
// current send and continues the turn with everything queued plus text. With
// nothing queued and a blank text it does nothing.
func (c *Controller) SendNow(ctx context.Context, text string, images ...string) error {
	_, err := c.input(ctx, text, images, true)
	return err
}

// Prompt sends text verbatim as a prompt, even when it reads as a control
// command, for input that must never change mode. It records no history. Like
// Submit, it returns a nil Work when the input is queued behind a running turn.
func (c *Controller) Prompt(ctx context.Context, text string) (*Work, error) {
	return c.submitPrompt(ctx, pigruntime.Prompt{Text: text}, false)
}

func (c *Controller) input(ctx context.Context, text string, paths []string, now bool) (*Work, error) {
	blank := strings.TrimSpace(text) == ""
	if blank && !now {
		return nil, nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, ErrNoSession
	}
	images, err := loadImages(paths)
	if err != nil {
		return nil, err
	}
	if !blank {
		c.record(text)
	}
	name, args, isCommand := parse(text)
	if !isCommand {
		prompt := strings.TrimSpace(text)
		if strings.HasPrefix(prompt, "//") {
			prompt = prompt[1:]
		}
		return c.submitPrompt(ctx, pigruntime.Prompt{Text: prompt, Images: images}, now)
	}
	cmds := commandTable()
	i := slices.IndexFunc(cmds, func(cmd command) bool { return cmd.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("%w: /%s", ErrUnknownCommand, name)
	}
	return nil, cmds[i].run(c, ctx, args)
}

func (c *Controller) record(text string) {
	if c.cfg.History == nil {
		return
	}
	if err := c.cfg.History.Add(text); err != nil {
		slog.Warn("failed to record prompt history", "err", err)
	}
}

// History returns the recorded inputs, oldest first; nil when disabled.
func (c *Controller) History() []string {
	if c.cfg.History == nil {
		return nil
	}
	return c.cfg.History.Texts()
}

// Files returns the workspace entries @ references complete to; nil when disabled.
func (c *Controller) Files() ([]Candidate, error) {
	if c.cfg.Files == nil {
		return nil, nil
	}
	paths, err := c.cfg.Files.List(maxReferenceEntries)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}
	out := make([]Candidate, 0, len(paths))
	for _, p := range paths {
		out = append(out, Candidate{Value: p})
	}
	return out, nil
}

// submitPrompt starts a turn with p or queues p for the running one or, during
// the first open started by Start, for the first session. It never waits for
// a turn to end: input given after an interrupt starts the next turn once the
// stopped one ends.
func (c *Controller) submitPrompt(ctx context.Context, p pigruntime.Prompt, now bool) (*Work, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	first := c.switching != nil && c.switching.cancelWait != nil
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.login != nil {
		c.mu.Unlock()
		return nil, ErrLoggingIn
	}
	if c.sess == nil && !first {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.switching != nil && !first {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	w, emit := c.turn, c.emit
	if w == nil && !first {
		if p.Text == "" {
			c.mu.Unlock()
			return nil, nil
		}
		return c.startTurn(ctx, []pigruntime.Prompt{p}), nil
	}
	c.mu.Unlock()
	if emit == nil {
		emit = c.relay.emit
	}
	if p.Text != "" {
		c.pending = append(c.pending, p)
		c.queueChanged(emit)
	}
	if now && w != nil && !w.stopped && len(c.pending) > 0 {
		// Not stopped: the end of the send continues the Work with pending.
		w.cancel()
	}
	return nil, nil
}

// take empties pending; it must be called with queue held.
func (c *Controller) take(emit func(event.Event)) []pigruntime.Prompt {
	taken := c.pending
	if len(taken) > 0 {
		c.pending = nil
		c.queueChanged(emit)
	}
	return taken
}

// queueChanged must be called with queue held, so reports arrive in the order
// pending changed.
func (c *Controller) queueChanged(emit func(event.Event)) {
	texts := make([]string, 0, len(c.pending))
	for _, p := range c.pending {
		texts = append(texts, p.Text)
	}
	emit(event.QueueChanged{Pending: texts})
}

// TakeQueued removes and returns the input queued for the running turn or
// the first session.
func (c *Controller) TakeQueued() []event.Input {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	emit := c.emit
	c.mu.Unlock()
	if emit == nil {
		emit = c.relay.emit
	}
	return inputs(c.take(emit))
}

func (c *Controller) Commands() []Command {
	cmds := commandTable()
	out := make([]Command, 0, len(cmds))
	for _, cmd := range cmds {
		cm := cmd.Command
		cm.Argument = cmd.args != nil
		out = append(out, cm)
	}
	return out
}

// Interrupt requests abort of the running root turn or of a running login,
// without waiting for it, and reports whether there was one. Input still queued
// is handed back in the turn's TurnEnd.Unsent; task notices are not, and wait
// for the next human prompt without waking the model.
func (c *Controller) Interrupt() bool {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	w, emit, login := c.turn, c.emit, c.login
	c.mu.Unlock()
	if login != nil {
		login.cancel()
		return true
	}
	if w == nil {
		return false
	}
	c.hold = true
	c.stop(w, emit)
	return true
}

// stop must be called with queue held. It leaves notices queued.
func (c *Controller) stop(w *Work, emit func(event.Event)) {
	w.handBack = append(w.handBack, c.take(emit)...)
	w.stopped = true
	c.mu.Lock()
	if c.approval != nil && c.approval.work == w {
		c.approval = nil
	}
	c.mu.Unlock()
	w.cancel()
}

// ToggleMode switches between plan and execute mode.
func (c *Controller) ToggleMode() {
	c.changeMode(func() uint64 {
		if mode, _ := c.authority.Snapshot(); mode == policy.Plan {
			return c.authority.EnterExecute()
		}
		return c.authority.EnterPlan()
	})
}

func (c *Controller) EnterPlan() {
	c.changeMode(c.authority.EnterPlan)
}

func (c *Controller) EnterExecute() {
	c.changeMode(c.authority.EnterExecute)
}

// changeMode holds mu across the read and the change so that only the caller
// that actually changed the mode emits ModeChanged.
func (c *Controller) changeMode(enter func() uint64) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	_, before := c.authority.Snapshot()
	after := enter()
	mode, _ := c.authority.Snapshot()
	c.mu.Unlock()
	if after != before {
		c.relay.emit(event.ModeChanged{Mode: mode})
	}
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	sess, sessID := c.sess, c.sessID
	busy := c.turn != nil
	c.mu.Unlock()

	mode, _ := c.authority.Snapshot()
	s := Status{SessionID: sessID, Mode: mode, Model: c.cfg.Model, Busy: busy, Branch: gitBranch(c.cfg.Workspace)}
	if sess != nil {
		s.MessageCount = sess.MessageCount()
		if tokens, window, ok := sess.ContextUsage(); ok {
			s.ContextTokens, s.ContextWindow = tokens, window
		}
	}
	for _, t := range c.cfg.Tools {
		if t.Capability == policy.Exec {
			s.Shell, s.Sandboxed = true, t.Sandboxed
			break
		}
	}
	for _, info := range c.cfg.Tasks.List() {
		if info.Status == tool.TaskRunning {
			s.Tasks++
		}
	}
	s.Foreground = c.cfg.Tasks.Foreground()
	return s
}

// Close stops running work and every task, and closes the session within
// min(ctx, JoinTimeout). Before that it waits for any session start or listing
// already in progress, which that bound does not cover. It does not close the Runtime. If work or a
// login does not stop, the session is left open, the work stays tracked and the
// error wraps pigruntime.ErrJoinTimeout; a later Close tries again. No event is delivered
// after Close returns.
func (c *Controller) Close(ctx context.Context) error {
	c.life.Lock()
	defer c.life.Unlock()
	defer c.relay.shut()

	c.queue.Lock()
	c.mu.Lock()
	c.closed = true
	c.notices = nil
	sess, unsubscribe, w, emit, login := c.sess, c.unsubscribe, c.turn, c.emit, c.login
	if login != nil {
		login.cancel()
	}
	if c.switching != nil && c.switching.cancelWait != nil {
		c.switching.cancelWait()
	}
	if c.switching != nil && c.switching.work != nil {
		w = c.switching.work
	}
	c.mu.Unlock()
	if w != nil {
		// Permanently, even if the join below times out.
		w.discard = true
		w.switchHold = false
		w.terminal = nil
		c.stop(w, emit)
	}
	c.queue.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	// closed is set, so no login starts now. The login needs neither life nor
	// queue, and it does not end before its last emit, so after this nothing of
	// it reaches the runtime or a subscriber.
	if login != nil {
		select {
		case <-login.done:
		case <-ctx.Done():
			return fmt.Errorf("failed to close controller: login did not stop: %w", pigruntime.ErrJoinTimeout)
		}
	}
	if err := c.stopTasks(ctx); err != nil {
		return err
	}
	if sess == nil {
		return nil
	}

	if w != nil {
		select {
		case <-w.done:
		case <-ctx.Done():
			return fmt.Errorf("failed to close controller: work %s did not stop: %w", w.id, pigruntime.ErrJoinTimeout)
		}
	}
	c.wg.Wait()
	// Abort reaches tools asynchronously: adoption can follow the first sweep.
	if len(c.cfg.Tasks.List()) > 0 {
		if err := c.stopTasks(ctx); err != nil {
			return err
		}
	}
	unsubscribe()
	c.mu.Lock()
	c.sess, c.sessID, c.emit, c.unsubscribe = nil, "", nil, nil
	c.mu.Unlock()
	if err := sess.Close(ctx); err != nil {
		return fmt.Errorf("failed to close controller: %w", err)
	}
	return nil
}

// stopTasks runs with life held. A retry joins unfinished cleanup rather than
// overlapping it, even when StopAll has already emptied the visible task list.
func (c *Controller) stopTasks(ctx context.Context) error {
	if c.stopAllDone == nil {
		done := make(chan struct{})
		c.stopAllDone = done
		go func() {
			c.cfg.Tasks.StopAll()
			close(done)
		}()
	}
	select {
	case <-c.stopAllDone:
		c.stopAllDone = nil
		return nil
	case <-ctx.Done():
		return fmt.Errorf("failed to close controller: tasks did not stop: %w", pigruntime.ErrJoinTimeout)
	}
}

// startTurn must be called with queue and mu held; it releases mu. The turn
// takes every queued notice; with no parts it is a wake, which the notices
// alone start.
func (c *Controller) startTurn(ctx context.Context, parts []pigruntime.Prompt) *Work {
	sess, emit := c.sess, c.emit
	notices := c.takeNotices()
	if len(parts) > 0 {
		c.hold = false
	}
	// The turn outlives the Submit call: only Interrupt, SendNow, a session
	// switch or Close may stop a send, by cancelling its context. Cancelling
	// it after the send has ended is harmless, so no abort can reach a later
	// send.
	base := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithCancel(base)
	c.turnSeq++
	w := &Work{id: fmt.Sprintf("turn-%d", c.turnSeq), sendCtx: sendCtx, cancel: cancel, done: make(chan struct{})}
	c.turn = w
	c.mu.Unlock()

	emit(event.TurnStart{WorkID: w.id, Prompt: joined(parts).Text})
	c.wg.Go(func() { c.runTurn(base, sendCtx, sess, emit, w, parts, notices) })
	return w
}

// runTurn sends parts and notices, then keeps sending while settle finds input
// to continue with or a waking notice is queued. The decision, the final
// TurnEnd and what follows it are one queue section, so no input, notice or
// interrupt lands after the Work found nothing left but before it is
// deregistered. Only a switch-held terminal TurnEnd may be deferred past
// deregistration; the switch resolves it before advancing the relay epoch.
func (c *Controller) runTurn(base, ctx context.Context, sess *pigruntime.Session, emit func(event.Event), w *Work, parts []pigruntime.Prompt, notices []pigruntime.Notice) {
	for {
		start := time.Now()
		res, err := sess.Send(ctx, joined(parts), notices, c.pull(ctx, emit))
		elapsed := time.Since(start)
		if err != nil {
			res.Outcome = event.Outcome{Status: event.Failed, Message: err.Error()}
		}

		c.queue.Lock()
		cancelled := ctx.Err() != nil
		w.cancel()
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if !closed {
			c.requeue(res.UndeliveredNotices)
		}
		next, unsent := settle(w.stopped, cancelled, res.Outcome, res.Sent, parts, res.Undelivered, w.handBack, c.pending)
		if !w.stopped {
			c.take(emit)
		}
		// A failed or unsent send never continues for notices alone, so a
		// failing provider cannot be woken in a loop.
		wake := !w.stopped && c.cfg.Wake && (cancelled || res.Outcome.Status == event.Completed && res.Sent) && c.waking()
		if len(next) > 0 || wake {
			ctx, w.cancel = context.WithCancel(base)
			w.sendCtx = ctx
			parts = next
			notices = c.takeNotices()
			emit(event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed})
			emit(event.TurnStart{WorkID: w.id, Prompt: joined(parts).Text})
			c.queue.Unlock()
			continue
		}
		if w.discard {
			unsent = nil
		}
		w.outcome = res.Outcome
		w.reply = res.Reply
		end := event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed, Unsent: inputs(unsent)}
		if w.switchHold {
			w.terminal = &end
		} else {
			emit(end)
		}
		c.finish(base, w, emit)
		c.queue.Unlock()
		close(w.done)
		return
	}
}

// pull returns the callback through which the send on ctx takes pending and
// the queued notices at a boundary. Notices go only with a prompt or a waking
// notice: at a run's last boundary a hand-over extends the run by a request,
// which a non-waking notice alone must not cause. It hands nothing over once
// ctx is cancelled: a stop or a send-now cancels ctx in the same queue section
// as it changes pending, and input handed to a run being cancelled would enter
// history with no reply.
func (c *Controller) pull(ctx context.Context, emit func(event.Event)) func() ([]pigruntime.Prompt, []pigruntime.Notice) {
	return func() ([]pigruntime.Prompt, []pigruntime.Notice) {
		c.queue.Lock()
		defer c.queue.Unlock()
		if ctx.Err() != nil {
			return nil, nil
		}
		prompts := c.take(emit)
		if len(prompts) == 0 && (!c.cfg.Wake || !c.waking()) {
			return nil, nil
		}
		return prompts, c.takeNotices()
	}
}

// finish deregisters the ended w and starts the next turn with the input given
// after w was stopped, unless the session is closing or being switched; that
// input is then dropped. Notices alone start no turn here: a waking one would
// have continued w, unless w failed or was stopped. It must be called with
// queue held.
func (c *Controller) finish(base context.Context, w *Work, emit func(event.Event)) {
	parts := c.take(emit)
	c.mu.Lock()
	if c.turn == w {
		c.turn = nil
	}
	if len(parts) == 0 || c.closed || c.switching != nil {
		c.mu.Unlock()
		return
	}
	c.startTurn(base, parts)
}

// settle decides what follows a finished send of parts: the input to send next
// or, when there is none, the input to hand back unsent. undelivered is what
// the send took from pending but the model never read; handBack is what was
// pending when the Work was stopped. Input pending after a stop is neither: it
// starts the next turn.
func settle(stopped, cancelled bool, outcome event.Outcome, sent bool, parts, undelivered, handBack, pending []pigruntime.Prompt) (next, unsent []pigruntime.Prompt) {
	left := undelivered
	if !sent {
		left = slices.Concat(parts, undelivered)
	}
	if stopped {
		return nil, slices.Concat(left, handBack)
	}
	left = slices.Concat(left, pending)
	// Cancelled without a stop is send-now, which continues whatever the
	// outcome.
	if cancelled || outcome.Status == event.Completed && sent {
		return left, nil
	}
	return nil, left
}

func joined(ps []pigruntime.Prompt) pigruntime.Prompt {
	texts := make([]string, 0, len(ps))
	var images []pigruntime.Image
	for _, p := range ps {
		texts = append(texts, p.Text)
		images = append(images, p.Images...)
	}
	return pigruntime.Prompt{Text: strings.Join(texts, "\n\n"), Images: images}
}

func inputs(ps []pigruntime.Prompt) []event.Input {
	if len(ps) == 0 {
		return nil
	}
	out := make([]event.Input, 0, len(ps))
	for _, p := range ps {
		in := event.Input{Text: p.Text}
		for _, img := range p.Images {
			in.Images = append(in.Images, img.Path)
		}
		out = append(out, in)
	}
	return out
}

// start opens a session for req and makes it current, then emits events.
// Running work is stopped and joined first; if it will not stop the switch is
// refused. The new session is opened before the old one is closed, so a failed
// open leaves the old session in place.
func (c *Controller) start(ctx context.Context, req OpenRequest) error {
	c.life.Lock()
	defer c.life.Unlock()
	op, err := c.beginSwitch(nil)
	if err != nil {
		return err
	}
	return c.startSwitch(ctx, req, op)
}

// beginSwitch marks and stops in one queue section, before settlement can
// drain input queued after an earlier Interrupt. cancelWait is non-nil only
// for the first open started by Start, which a login's deferred open also
// starts; every other switch is refused with ErrLoggingIn while a login runs.
func (c *Controller) beginSwitch(cancelWait context.CancelFunc) (*sessionSwitch, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.login != nil && cancelWait == nil {
		c.mu.Unlock()
		return nil, ErrLoggingIn
	}
	if c.switching != nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	op := &sessionSwitch{work: c.turn, emit: c.emit, cancelWait: cancelWait}
	c.switching = op
	c.mu.Unlock()
	if op.work != nil {
		op.work.switchHold = true
		c.stop(op.work, op.emit)
	}
	return op, nil
}

// startSwitch runs with life held and resolves the captured operation, not
// the current turn, which may already have deregistered. Once the session is
// open, input still pending, which only the first open started by Start or a
// later open after it failed can leave, starts the session's first turn in the
// same queue section that ends the switch, ahead of any input given later.
func (c *Controller) startSwitch(ctx context.Context, req OpenRequest, op *sessionSwitch) (err error) {
	defer func() {
		c.queue.Lock()
		defer c.queue.Unlock()
		var parts []pigruntime.Prompt
		if err == nil {
			c.mu.Lock()
			emit := c.emit
			c.mu.Unlock()
			parts = c.take(emit)
		}
		c.mu.Lock()
		c.switching = nil
		if len(parts) == 0 {
			c.mu.Unlock()
			return
		}
		c.startTurn(ctx, parts)
	}()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrNoSession
	}
	old, oldUnsubscribe := c.sess, c.unsubscribe
	c.mu.Unlock()
	w, oldEmit := op.work, op.emit
	if w != nil {
		defer c.releaseSwitch(w, oldEmit, false)
		joinCtx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
		defer cancel()
		select {
		case <-w.done:
		case <-joinCtx.Done():
			return fmt.Errorf("failed to switch session: work %s did not stop: %w", w.id, ErrWorkStillRunning)
		}
	}

	later := c.attempted
	c.attempted = true
	d := newDisplay(c.tools)
	var rec *classifier.Record
	if c.cfg.Classifier != nil {
		rec = &classifier.Record{}
	}
	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        c.cfg.Model,
		ResumeID:     req.ResumeID,
		Continue:     req.Continue,
		SystemPrompt: c.prompt,
		Tools:        c.tools,
		Authority:    c.authority,
		Rules:        c.cfg.Rules,
		Approve:      c.approver(d),
		Review:       c.reviewer(rec),
	})
	if err != nil {
		return err
	}

	if w != nil {
		c.releaseSwitch(w, oldEmit, true)
	}
	c.relay.advance()
	if old != nil {
		oldUnsubscribe()
		// The old session is released even if the caller's ctx has ended:
		// the new session is already open.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.JoinTimeout)
		if err := old.Close(closeCtx); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
		cancel()
		// The tasks and their notices belong to the old session; a failed
		// open keeps both. Notices are cleared atomically with installation.
		c.cfg.Tasks.StopAll()
		c.relay.emit(event.TasksChanged{})
	}
	// A session opened after the first open was tried, even one that failed,
	// starts in the configured mode, never in the mode it was left in; the
	// first keeps the mode chosen while it was opening.
	if later {
		if c.cfg.Mode == policy.Execute {
			c.EnterExecute()
		} else {
			c.EnterPlan()
		}
	}

	bound := c.relay.bind()
	emit := func(ev event.Event) {
		rec.Observe(ev)
		bound(d.apply(ev))
	}
	unsubscribe := sess.Subscribe(emit)
	id := sess.ID()
	c.queue.Lock()
	c.mu.Lock()
	if old != nil {
		c.notices = nil
		c.hold = false
	}
	c.sess, c.sessID, c.emit, c.unsubscribe = sess, id, emit, unsubscribe
	c.deferred = nil
	c.mu.Unlock()
	c.queue.Unlock()
	changed := event.SessionChanged{
		SessionID:    id,
		Resumed:      req.ResumeID != "" || req.Continue,
		MessageCount: sess.MessageCount(),
	}
	if changed.Resumed {
		for _, ev := range sess.History() {
			rec.Observe(ev)
			changed.History = append(changed.History, d.apply(ev))
		}
	}
	c.relay.emit(changed)
	for _, ev := range c.startup {
		c.relay.emit(ev)
	}
	c.startup = nil
	return nil
}

// releaseSwitch resolves the held terminal event through the old session's
// emitter before its relay epoch can be retired. It is safe to call again.
func (c *Controller) releaseSwitch(w *Work, emit func(event.Event), committed bool) {
	c.queue.Lock()
	defer c.queue.Unlock()
	w.switchHold = false
	if w.terminal != nil {
		end := *w.terminal
		w.terminal = nil
		if committed || w.discard {
			end.Unsent = nil
		}
		emit(end)
	}
}
