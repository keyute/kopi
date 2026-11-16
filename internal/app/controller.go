package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type Config struct {
	Model     string // "provider/id"
	Workspace string
	Tools     []tool.Tool
	// JoinTimeout bounds how long a session switch or Close waits for running
	// work to stop.
	JoinTimeout time.Duration
	// History records human input; nil disables it.
	History *history.Store
	// Files lists the entries that @ references complete to; nil offers none.
	Files *tool.Workspace
}

const maxReferenceEntries = 10000

// Controller holds all frontend-neutral behaviour: frontends pass input in
// and render the events they subscribe to.
//
// Lock order is life, then queue, then mu. mu is never held while calling
// into PiG or delivering events; life is held across both and queue across
// delivering events, which is safe because subscribers must not call back into
// the Controller.
type Controller struct {
	rt        *pigruntime.Runtime
	cfg       Config
	prompt    string
	authority *policy.Authority
	relay     relay
	wg        sync.WaitGroup

	// life serialises session start, switch and close.
	life sync.Mutex
	// queue makes queueing input atomic with taking it from pending and with
	// deciding to stop, continue or end the running Work; it guards pending
	// and the Work's queue fields. PiG calls back under its own lock to take
	// pending, so nothing may call into PiG with queue held.
	queue sync.Mutex
	// pending is the input queued for the running turn, the only source of
	// QueueChanged. It is non-empty only while a turn is registered.
	pending []pigruntime.Prompt

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
}

// sessionSwitch retains work even after it deregisters while start waits for life.
type sessionSwitch struct {
	work *Work
	emit func(event.Event)
}

func NewController(rt *pigruntime.Runtime, cfg Config) *Controller {
	return &Controller{
		rt:        rt,
		cfg:       cfg,
		prompt:    systemPrompt(cfg.Workspace, cfg.Tools),
		authority: policy.NewAuthority(),
	}
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

func (c *Controller) Open(ctx context.Context, req OpenRequest) error {
	return c.start(ctx, req)
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
	op, err := c.beginSwitch()
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
// control command is executed and returns (nil, nil), ErrQuit for /quit or
// ErrUnknownCommand. /new and /resume return at once and report their result
// through events. Anything else is a prompt: it starts a root turn or, while
// one runs, is queued for that turn and returns (nil, nil). After Close every
// input but a blank line returns ErrNoSession.
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

// submitPrompt starts a turn with p or queues p for the running one. It never
// waits for a turn to end: input given after an interrupt starts the next turn
// once the stopped one ends.
func (c *Controller) submitPrompt(ctx context.Context, p pigruntime.Prompt, now bool) (*Work, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.sess == nil || c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.switching != nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	w, emit := c.turn, c.emit
	if w == nil {
		if p.Text == "" {
			c.mu.Unlock()
			return nil, nil
		}
		return c.startTurn(ctx, []pigruntime.Prompt{p}), nil
	}
	c.mu.Unlock()
	if p.Text != "" {
		c.pending = append(c.pending, p)
		c.queueChanged(emit)
	}
	if now && !w.stopped && len(c.pending) > 0 {
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

// TakeQueued removes and returns the input queued for the running turn.
func (c *Controller) TakeQueued() []event.Input {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	emit := c.emit
	c.mu.Unlock()
	return inputs(c.take(emit))
}

func (c *Controller) Commands() []Command {
	cmds := commandTable()
	out := make([]Command, 0, len(cmds))
	for _, cmd := range cmds {
		out = append(out, cmd.Command)
	}
	return out
}

// Interrupt requests abort of the running root turn without waiting for it.
// Input still queued is handed back in the turn's TurnEnd.Unsent.
func (c *Controller) Interrupt() bool {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	w, emit := c.turn, c.emit
	c.mu.Unlock()
	if w == nil {
		return false
	}
	c.stop(w, emit)
	return true
}

// stop must be called with queue held.
func (c *Controller) stop(w *Work, emit func(event.Event)) {
	w.handBack = append(w.handBack, c.take(emit)...)
	w.stopped = true
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
	return s
}

// Close stops running work and closes the session within min(ctx,
// JoinTimeout). Before that it waits for any session start or listing
// already in progress, which that bound does not cover. It does not close the Runtime. If work does not stop, the
// session is left open, the work stays tracked and the error wraps
// pigruntime.ErrJoinTimeout; a later Close tries again. No event is delivered
// after Close returns.
func (c *Controller) Close(ctx context.Context) error {
	c.life.Lock()
	defer c.life.Unlock()
	defer c.relay.shut()

	c.queue.Lock()
	c.mu.Lock()
	c.closed = true
	sess, unsubscribe, w := c.sess, c.unsubscribe, c.turn
	if c.switching != nil && c.switching.work != nil {
		w = c.switching.work
		w.discard = true
		w.switchHold = false
		w.terminal = nil
	}
	c.mu.Unlock()
	c.queue.Unlock()
	if sess == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	if w != nil && !c.halt(ctx, w) {
		return fmt.Errorf("failed to close controller: work %s did not stop: %w", w.id, pigruntime.ErrJoinTimeout)
	}
	c.wg.Wait()
	unsubscribe()
	c.mu.Lock()
	c.sess, c.sessID, c.emit, c.unsubscribe = nil, "", nil, nil
	c.mu.Unlock()
	if err := sess.Close(ctx); err != nil {
		return fmt.Errorf("failed to close controller: %w", err)
	}
	return nil
}

// startTurn must be called with queue and mu held; it releases mu.
func (c *Controller) startTurn(ctx context.Context, parts []pigruntime.Prompt) *Work {
	sess, emit := c.sess, c.emit
	// The turn outlives the Submit call: only Interrupt, SendNow, a session
	// switch or Close may stop a send, by cancelling its context. Cancelling
	// it after the send has ended is harmless, so no abort can reach a later
	// send.
	base := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithCancel(base)
	c.turnSeq++
	w := &Work{id: fmt.Sprintf("turn-%d", c.turnSeq), cancel: cancel, done: make(chan struct{})}
	c.turn = w
	c.mu.Unlock()

	emit(event.TurnStart{WorkID: w.id, Prompt: joined(parts).Text})
	c.wg.Go(func() { c.runTurn(base, sendCtx, sess, emit, w, parts) })
	return w
}

// runTurn sends parts, then keeps sending while settle finds input to continue
// with. The decision, the final TurnEnd and what follows it are one queue
// section, so no input or interrupt lands after the Work found nothing left
// but before it is deregistered. Only a switch-held terminal TurnEnd may be
// deferred past deregistration; the switch resolves it before advancing the
// relay epoch.
func (c *Controller) runTurn(base, ctx context.Context, sess *pigruntime.Session, emit func(event.Event), w *Work, parts []pigruntime.Prompt) {
	for {
		start := time.Now()
		res, err := sess.Send(ctx, joined(parts), c.pull(ctx, emit))
		elapsed := time.Since(start)
		if err != nil {
			res.Outcome = event.Outcome{Status: event.Failed, Message: err.Error()}
		}

		c.queue.Lock()
		cancelled := ctx.Err() != nil
		w.cancel()
		next, unsent := settle(w.stopped, cancelled, res.Outcome, res.Sent, parts, res.Undelivered, w.handBack, c.pending)
		if !w.stopped {
			c.take(emit)
		}
		if len(next) > 0 {
			ctx, w.cancel = context.WithCancel(base)
			parts = next
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

// pull returns the callback through which the send on ctx takes pending at a
// boundary. It hands nothing over once ctx is cancelled: a stop or a send-now
// cancels ctx in the same queue section as it changes pending, and input handed
// to a run being cancelled would enter history with no reply.
func (c *Controller) pull(ctx context.Context, emit func(event.Event)) func() []pigruntime.Prompt {
	return func() []pigruntime.Prompt {
		c.queue.Lock()
		defer c.queue.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		return c.take(emit)
	}
}

// finish deregisters the ended w and starts the next turn with the input given
// after w was stopped, unless the session is closing or being switched; that
// input is then dropped. It must be called with queue held.
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

// start opens a session for req and makes it current. Running work is
// stopped and joined first; if it will not stop the switch is refused. The new
// session is opened before the old one is closed, so a failed open leaves the
// old session in place.
func (c *Controller) start(ctx context.Context, req OpenRequest) error {
	c.life.Lock()
	defer c.life.Unlock()
	op, err := c.beginSwitch()
	if err != nil {
		return err
	}
	return c.startSwitch(ctx, req, op)
}

// beginSwitch marks and stops in one queue section, before settlement can
// drain input queued after an earlier Interrupt.
func (c *Controller) beginSwitch() (*sessionSwitch, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.switching != nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	op := &sessionSwitch{work: c.turn, emit: c.emit}
	c.switching = op
	c.mu.Unlock()
	if op.work != nil {
		op.work.switchHold = true
		c.stop(op.work, op.emit)
	}
	return op, nil
}

// startSwitch runs with life held and resolves the captured operation, not
// the current turn, which may already have deregistered.
func (c *Controller) startSwitch(ctx context.Context, req OpenRequest, op *sessionSwitch) error {
	defer func() {
		c.mu.Lock()
		c.switching = nil
		c.mu.Unlock()
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

	sess, err := c.rt.Open(ctx, pigruntime.OpenOptions{
		Model:        c.cfg.Model,
		ResumeID:     req.ResumeID,
		Continue:     req.Continue,
		SystemPrompt: c.prompt,
		Tools:        c.cfg.Tools,
		Authority:    c.authority,
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
	}
	// Execute mode is never carried into a session start.
	c.EnterPlan()

	bound, d := c.relay.bind(), newDisplay(c.cfg.Tools)
	emit := func(ev event.Event) { bound(d.apply(ev)) }
	unsubscribe := sess.Subscribe(emit)
	id := sess.ID()
	c.mu.Lock()
	c.sess, c.sessID, c.emit, c.unsubscribe = sess, id, emit, unsubscribe
	c.mu.Unlock()
	changed := event.SessionChanged{
		SessionID:    id,
		Resumed:      req.ResumeID != "" || req.Continue,
		MessageCount: sess.MessageCount(),
	}
	if changed.Resumed {
		for _, ev := range sess.History() {
			changed.History = append(changed.History, d.apply(ev))
		}
	}
	c.relay.emit(changed)
	return nil
}

// halt permanently discards Close's input and joins without queue held.
func (c *Controller) halt(ctx context.Context, w *Work) bool {
	c.queue.Lock()
	c.mu.Lock()
	emit := c.emit
	c.mu.Unlock()
	w.discard = true
	w.switchHold = false
	w.terminal = nil
	c.stop(w, emit)
	c.queue.Unlock()
	select {
	case <-w.done:
		return true
	case <-ctx.Done():
		return false
	}
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

func (c *Controller) notice(text string) {
	c.relay.emit(event.Notice{Text: text})
}
