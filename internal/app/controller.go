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
}

// Controller holds all frontend-neutral behaviour: frontends pass input in
// and render the events they subscribe to.
//
// Lock order is life, then queue, then mu. mu is never held while calling
// into PiG or delivering events; life and queue are held across both, which is
// safe because subscribers must not call back into the Controller.
type Controller struct {
	rt        *pigruntime.Runtime
	cfg       Config
	prompt    string
	authority *policy.Authority
	relay     relay
	wg        sync.WaitGroup
	// prepareSteer is a field so tests can hold a prompt between its
	// preparation and the decision made after it.
	prepareSteer func(*pigruntime.Session, pigruntime.Prompt) pigruntime.Steering

	// life serialises session start, switch and close.
	life sync.Mutex
	// queue makes steering into the running Work atomic with taking PiG's
	// queue and deciding to stop, continue or end that Work; it guards the
	// Work's queue fields. PiG's abort leaves its queue in place and the next
	// run would drain it, so the queue is always taken before a cancel.
	queue sync.Mutex

	mu     sync.Mutex
	sess   *pigruntime.Session
	sessID string
	// emit is bound to sess's relay epoch.
	emit        func(event.Event)
	unsubscribe func()
	// switching refuses prompts while a session transition is under way, so
	// no work can start on a session that is about to be replaced.
	switching bool
	// closed is set by Close; afterwards the Controller refuses all input,
	// leaves the mode alone and delivers no events.
	closed  bool
	turn    *Work
	turnSeq int
}

func NewController(rt *pigruntime.Runtime, cfg Config) *Controller {
	return &Controller{
		rt:           rt,
		cfg:          cfg,
		prompt:       systemPrompt(cfg.Workspace, cfg.Tools),
		authority:    policy.NewAuthority(),
		prepareSteer: (*pigruntime.Session).PrepareSteer,
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

func (c *Controller) Sessions() ([]pigruntime.SessionInfo, error) {
	return c.rt.Sessions()
}

// Submit takes one message of human input with the paths of its images. A
// control command is executed and returns (nil, nil), ErrQuit for /quit or
// ErrUnknownCommand. Anything else is a prompt: it starts a root turn or,
// while one runs, is queued for that turn and returns (nil, nil). After Close
// every input but a blank line returns ErrNoSession.
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

// submitPrompt starts a turn with p, or hands p to the running one. A Work that
// has been stopped or has already decided to end is waited for, so that p
// starts the next turn. Images are prepared for steering without queue held,
// and the decision is then made again.
func (c *Controller) submitPrompt(ctx context.Context, p pigruntime.Prompt, now bool) (*Work, error) {
	var (
		steering pigruntime.Steering
		// preparedFor is the session steering was prepared for, if any.
		preparedFor *pigruntime.Session
	)
	for {
		c.queue.Lock()
		c.mu.Lock()
		if c.sess == nil || c.closed {
			c.mu.Unlock()
			c.queue.Unlock()
			return nil, ErrNoSession
		}
		if c.switching {
			c.mu.Unlock()
			c.queue.Unlock()
			return nil, ErrBusy
		}
		sess, w := c.sess, c.turn
		if w == nil {
			if p.Text == "" {
				c.mu.Unlock()
				c.queue.Unlock()
				return nil, nil
			}
			w = c.startTurn(ctx, p)
			c.queue.Unlock()
			return w, nil
		}
		c.mu.Unlock()
		if !w.ending && !w.stopped {
			if now {
				c.sendNow(sess, w, p)
				c.queue.Unlock()
				return nil, nil
			}
			if preparedFor != sess {
				if len(p.Images) > 0 {
					c.queue.Unlock()
					steering, preparedFor = c.prepareSteer(sess, p), sess
					continue
				}
				steering = sess.PrepareSteer(p)
			}
			sess.Steer(steering)
			w.steered = true
			c.queue.Unlock()
			return nil, nil
		}
		c.queue.Unlock()
		select {
		case <-w.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("failed to submit prompt: %w", ctx.Err())
		}
	}
}

// sendNow must be called with queue held.
func (c *Controller) sendNow(sess *pigruntime.Session, w *Work, p pigruntime.Prompt) {
	next := c.take(sess, w)
	if p.Text != "" {
		next = append(next, p)
	}
	if len(next) == 0 {
		return
	}
	w.next = append(w.next, next...)
	w.cancel()
}

// take empties PiG's queue for w; it must be called with queue held.
func (c *Controller) take(sess *pigruntime.Session, w *Work) []pigruntime.Prompt {
	if !w.steered {
		return nil
	}
	w.steered = false
	return sess.TakeQueued()
}

// TakeQueued removes and returns the input queued for the running turn.
func (c *Controller) TakeQueued() []event.Input {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	sess, w := c.sess, c.turn
	c.mu.Unlock()
	if w == nil {
		return nil
	}
	return inputs(c.take(sess, w))
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
	sess, w := c.sess, c.turn
	c.mu.Unlock()
	if w == nil {
		return false
	}
	c.stop(sess, w)
	return true
}

// stop must be called with queue held.
func (c *Controller) stop(sess *pigruntime.Session, w *Work) {
	if w.ending {
		return
	}
	w.next = append(w.next, c.take(sess, w)...)
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
	s := Status{SessionID: sessID, Mode: mode, Model: c.cfg.Model, Workspace: c.cfg.Workspace, Busy: busy, Branch: gitBranch(c.cfg.Workspace)}
	if sess != nil {
		s.MessageCount = sess.MessageCount()
		if tokens, window, ok := sess.ContextUsage(); ok {
			s.ContextTokens, s.ContextWindow = tokens, window
		}
	}
	for _, t := range c.cfg.Tools {
		allowed := !t.Capability.NeedsExecute() || mode == policy.Execute
		s.Tools = append(s.Tools, ToolStatus{Name: t.Name, Capability: t.Capability, Allowed: allowed})
	}
	return s
}

// Close stops running work and closes the session within min(ctx,
// JoinTimeout). It does not close the Runtime. If work does not stop, the
// session is left open, the work stays tracked and the error wraps
// pigruntime.ErrJoinTimeout; a later Close tries again. No event is delivered
// after Close returns.
func (c *Controller) Close(ctx context.Context) error {
	c.life.Lock()
	defer c.life.Unlock()
	defer c.relay.shut()

	c.mu.Lock()
	c.closed = true
	sess, unsubscribe, w := c.sess, c.unsubscribe, c.turn
	c.mu.Unlock()
	if sess == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
	defer cancel()
	if w != nil && !c.halt(ctx, sess, w) {
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
func (c *Controller) startTurn(ctx context.Context, p pigruntime.Prompt) *Work {
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

	emit(event.TurnStart{WorkID: w.id, Prompt: p.Text})
	c.wg.Go(func() { c.runTurn(base, sendCtx, sess, emit, w, p) })
	return w
}

// runTurn sends p, then keeps sending while settle finds input to continue
// with. It emits the final TurnEnd before deregistering the work, so a
// transition that finds no work never races an undelivered TurnEnd of the old
// session.
func (c *Controller) runTurn(base, ctx context.Context, sess *pigruntime.Session, emit func(event.Event), w *Work, p pigruntime.Prompt) {
	for {
		start := time.Now()
		outcome, err := sess.Send(ctx, p.Text, p.Images...)
		elapsed := time.Since(start)
		if err != nil {
			outcome = event.Outcome{Status: event.Failed, Message: err.Error()}
			emit(event.Error{Err: err})
		}

		c.queue.Lock()
		next, unsent := settle(w.stopped, outcome, w.next, c.take(sess, w))
		w.next = nil
		w.cancel()
		if len(next) == 0 {
			w.ending = true
			if w.discard {
				unsent = nil
			}
			c.queue.Unlock()
			w.outcome = outcome
			emit(event.TurnEnd{WorkID: w.id, Outcome: outcome, Elapsed: elapsed, Unsent: inputs(unsent)})
			c.mu.Lock()
			if c.turn == w {
				c.turn = nil
			}
			c.mu.Unlock()
			close(w.done)
			return
		}
		ctx, w.cancel = context.WithCancel(base)
		p = joined(next)
		emit(event.TurnEnd{WorkID: w.id, Outcome: outcome, Elapsed: elapsed})
		emit(event.TurnStart{WorkID: w.id, Prompt: p.Text})
		c.queue.Unlock()
	}
}

// settle decides what follows a finished send: the input to send next or,
// when there is none, the input to hand back unsent. next is the input taken
// for the Work while the send ran; left is what was still queued after it.
func settle(stopped bool, outcome event.Outcome, next, left []pigruntime.Prompt) (continueWith, unsent []pigruntime.Prompt) {
	pending := slices.Concat(next, left)
	switch {
	case stopped:
		return nil, pending
	case len(next) > 0:
		return pending, nil
	// left was queued after PiG's last drain of the completed send.
	case outcome.Status == event.Completed && len(left) > 0:
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

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrNoSession
	}
	c.switching = true
	old, oldUnsubscribe, w := c.sess, c.unsubscribe, c.turn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.switching = false
		c.mu.Unlock()
	}()

	if w != nil {
		joinCtx, cancel := context.WithTimeout(ctx, c.cfg.JoinTimeout)
		stopped := c.halt(joinCtx, old, w)
		cancel()
		if !stopped {
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
		// A display of its own, so the args of calls that never finished in
		// the stored history do not linger in the live one.
		hd := newDisplay(c.cfg.Tools)
		for _, ev := range sess.History() {
			changed.History = append(changed.History, hd.apply(ev))
		}
	}
	c.relay.emit(changed)
	return nil
}

// halt stops w as Interrupt does, but discards its queued input, and reports
// whether it finished before ctx ended.
func (c *Controller) halt(ctx context.Context, sess *pigruntime.Session, w *Work) bool {
	c.queue.Lock()
	w.discard = true
	c.stop(sess, w)
	c.queue.Unlock()
	select {
	case <-w.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Controller) notice(text string) {
	c.relay.emit(event.Notice{Text: text})
}
