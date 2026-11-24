package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// runner drives one session's runs: the main session's or a subagent's.
type runner struct {
	c    *Controller
	role role
	// id and name are a subagent's task id and definition name; "" for the main
	// session.
	id, name string
	// d, rec and model are a subagent's display, classifier record and model.
	d     *display
	rec   *classifier.Record
	model string

	// The fields below are guarded by Controller.mu. The main session's are
	// replaced at each switch.

	sess *pigruntime.Session
	// emit is bound to sess's relay epoch for the main session. A subagent's is
	// subagentRole.emit, which reports through bound.
	emit func(event.Event)
	// bound is the relay emit of sess's epoch as it was, without the record and
	// the display; approval events and a subagent's session events go through it.
	bound       func(event.Event)
	unsubscribe func()
	work        *Work

	// The fields below are guarded by Controller.queue.

	// pending is the input queued for the running turn, the only source of
	// QueueChanged. It is non-empty only while a turn is registered.
	pending []pigruntime.Prompt
	// notices are the task notices waiting for the model. They are never part
	// of pending: they are not the human's input, so they are never reported,
	// handed back or taken.
	notices []pigruntime.Notice
	// hold keeps notices from waking the model after an Interrupt until a human
	// prompt starts a turn.
	hold bool
	// told is the last mode line delivered to the model in the current session;
	// "" when none was, or when one was not delivered. Only the main session's
	// is used.
	told string
}

// role is what differs between the kinds of session a runner drives.
type role interface {
	// begin admits w, the run about to start; queue is held, mu is not. An
	// error refuses the input.
	begin(w *Work) error
	// end records w's end before it is deregistered; queue is held, mu is not.
	// unread is the input another agent sent that w never read.
	end(w *Work, unread []pigruntime.Prompt)
}

// mainRole is the role of the main session, whose Works headless follows.
type mainRole struct{ c *Controller }

func (m mainRole) begin(w *Work) error {
	for q := range m.c.followers {
		q.works = append(q.works, w)
	}
	return nil
}

func (mainRole) end(*Work, []pigruntime.Prompt) {}

// submit starts a turn with p or queues p for the running one. It never waits
// for a turn to end: input given after an interrupt starts the next turn once
// the stopped one ends.
func (r *runner) submit(ctx context.Context, p pigruntime.Prompt, now bool) (*Work, error) {
	r.c.queue.Lock()
	defer r.c.queue.Unlock()
	return r.submitLocked(ctx, p, now)
}

// submitLocked is submit with queue held.
func (r *runner) submitLocked(ctx context.Context, p pigruntime.Prompt, now bool) (*Work, error) {
	c := r.c
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.login != nil {
		c.mu.Unlock()
		return nil, ErrLoggingIn
	}
	if r.sess == nil {
		c.mu.Unlock()
		return nil, ErrNoSession
	}
	if c.switching != nil || r.changingModel() {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	w, emit := r.work, r.emit
	if w == nil {
		if p.Text == "" {
			c.mu.Unlock()
			return nil, nil
		}
		return r.startTurn(ctx, []pigruntime.Prompt{p})
	}
	c.mu.Unlock()
	if p.Text != "" {
		r.pending = append(r.pending, p)
		r.queueChanged(emit)
	}
	if now && !w.stopped && len(r.pending) > 0 {
		// Not stopped: the end of the send continues the Work with pending.
		w.cancel()
	}
	return nil, nil
}

// take empties pending; it must be called with queue held.
func (r *runner) take(emit func(event.Event)) []pigruntime.Prompt {
	taken := r.pending
	if len(taken) > 0 {
		r.pending = nil
		r.queueChanged(emit)
	}
	return taken
}

// takeHuman empties pending of the human's input, leaving what other agents
// sent; it must be called with queue held.
func (r *runner) takeHuman(emit func(event.Event)) []pigruntime.Prompt {
	human := slices.DeleteFunc(slices.Clone(r.pending), func(p pigruntime.Prompt) bool { return p.FromAgent })
	if len(human) > 0 {
		r.pending = slices.DeleteFunc(r.pending, func(p pigruntime.Prompt) bool { return !p.FromAgent })
		r.queueChanged(emit)
	}
	return human
}

// queueChanged reports the human's input in pending. It must be called with
// queue held, so reports arrive in the order pending changed.
func (r *runner) queueChanged(emit func(event.Event)) {
	texts := make([]string, 0, len(r.pending))
	for _, p := range r.pending {
		if !p.FromAgent {
			texts = append(texts, p.Text)
		}
	}
	emit(event.QueueChanged{Pending: texts})
}

// interrupt requests abort of the running turn and reports whether there was
// one; it must be called with queue held.
func (r *runner) interrupt() bool {
	r.c.mu.Lock()
	w, emit := r.work, r.emit
	r.c.mu.Unlock()
	if w == nil {
		return false
	}
	r.hold = true
	r.stop(w, emit)
	return true
}

// stop must be called with queue held. It leaves notices queued.
func (r *runner) stop(w *Work, emit func(event.Event)) {
	c := r.c
	w.handBack = append(w.handBack, r.take(emit)...)
	w.stopped = true
	c.mu.Lock()
	c.approvals = slices.DeleteFunc(c.approvals, func(p *pendingApproval) bool { return p.work == w })
	if c.question != nil && c.question.work == w {
		c.question = nil
	}
	if c.plan != nil && c.plan.work == w {
		c.plan = nil
	}
	c.mu.Unlock()
	w.cancel()
}

// stopper returns what the task registry calls to stop w's run. It does
// nothing once w has ended, so a stop that arrives late never reaches a later
// run of the same session. The registry calls it without queue.
func (r *runner) stopper(w *Work) func() {
	return func() {
		r.c.queue.Lock()
		defer r.c.queue.Unlock()
		r.c.mu.Lock()
		current, emit := r.work == w, r.emit
		r.c.mu.Unlock()
		if current && !w.stopped {
			r.stop(w, emit)
		}
	}
}

// newWork returns the Work of a run that starts on ctx's values. The run
// outlives the call: only Interrupt, SendNow, a session switch, a task stop or
// Close may stop a send, by cancelling its context. Cancelling it after the send
// has ended is harmless, so no abort can reach a later send. It must be called
// with queue held.
func (r *runner) newWork(ctx context.Context) *Work {
	base := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithCancel(base)
	r.c.turnSeq++
	return &Work{id: fmt.Sprintf("turn-%d", r.c.turnSeq), base: base, sendCtx: sendCtx, cancel: cancel, done: make(chan struct{})}
}

// startTurn must be called with queue and mu held; it releases mu. The turn
// takes every queued notice; with no parts it is a wake, which the notices
// alone start. It returns the error of role.begin, with nothing registered.
func (r *runner) startTurn(ctx context.Context, parts []pigruntime.Prompt) (*Work, error) {
	c := r.c
	w := r.newWork(ctx)
	r.work = w
	c.mu.Unlock()
	if err := r.role.begin(w); err != nil {
		c.mu.Lock()
		r.work = nil
		c.mu.Unlock()
		w.cancel()
		return nil, err
	}
	r.start(w, parts)
	return w, nil
}

// start sends parts and the queued notices on w, which is r.work and has been
// admitted by role.begin or, for a subagent's first run, by its launch. It must
// be called with queue held and mu not.
func (r *runner) start(w *Work, parts []pigruntime.Prompt) {
	c := r.c
	c.mu.Lock()
	sess, emit := r.sess, r.emit
	c.mu.Unlock()
	notices := r.takeNotices()
	parts = r.tell(parts)
	if len(parts) > 0 {
		r.hold = false
	}
	c.signalLocked()

	emit(event.TurnStart{WorkID: w.id, Prompt: joined(parts).Text})
	sendCtx := w.sendCtx
	c.wg.Go(func() { r.runTurn(sendCtx, sess, emit, w, parts, notices) })
}

// runTurn sends parts and notices, then keeps sending while settle finds input
// to continue with or a waking notice is queued. The decision, the final
// TurnEnd and what follows it are one queue section, so no input, notice or
// interrupt lands after the Work found nothing left but before it is
// deregistered. Only a switch-held terminal TurnEnd may be deferred past
// deregistration; the switch resolves it before advancing the relay epoch.
func (r *runner) runTurn(ctx context.Context, sess *pigruntime.Session, emit func(event.Event), w *Work, parts []pigruntime.Prompt, notices []pigruntime.Notice) {
	c := r.c
	// carry keeps a send-now's raise subject to the session's effort revision;
	// a turn that follows a completed send starts at its own level.
	var carry *pigruntime.EffortContinuation
	if r == c.main {
		c.awaitServers(ctx)
	}
	for {
		start := time.Now()
		p := joined(parts)
		p.Continuation = carry
		res, err := sess.Send(ctx, p, notices, r.pull(ctx, emit))
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
			r.requeue(res.UndeliveredNotices)
		}
		if res.ModeUndelivered {
			r.told = ""
		}
		next, unsent := settle(w.stopped, cancelled, res.Outcome, res.Sent, parts, res.Undelivered, w.handBack, r.pending)
		r.untell(next, unsent)
		if !w.stopped {
			r.take(emit)
		}
		// A failed or unsent send never continues for notices alone, so a
		// failing provider cannot be woken in a loop.
		wake := !w.stopped && (cancelled || res.Outcome.Status == event.Completed && res.Sent) && r.waking()
		carry = nil
		if cancelled {
			carry = res.Continuation
		}
		if len(next) > 0 || wake {
			ctx, w.cancel = context.WithCancel(w.base)
			w.sendCtx = ctx
			parts = r.tell(next)
			notices = r.takeNotices()
			emit(event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed})
			emit(event.TurnStart{WorkID: w.id, Prompt: joined(parts).Text})
			c.queue.Unlock()
			continue
		}
		unread := slices.DeleteFunc(slices.Clone(unsent), func(p pigruntime.Prompt) bool { return !p.FromAgent })
		unsent = slices.DeleteFunc(unsent, func(p pigruntime.Prompt) bool { return p.FromAgent })
		if w.discard {
			unsent = nil
		}
		w.outcome = res.Outcome
		w.reply = res.Reply
		r.role.end(w, unread)
		end := event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed, Unsent: inputs(unsent)}
		if w.switchHold {
			w.terminal = &end
		} else {
			emit(end)
		}
		r.finish(w, emit)
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
func (r *runner) pull(ctx context.Context, emit func(event.Event)) func() ([]pigruntime.Prompt, []pigruntime.Notice) {
	return func() ([]pigruntime.Prompt, []pigruntime.Notice) {
		r.c.queue.Lock()
		defer r.c.queue.Unlock()
		if ctx.Err() != nil {
			return nil, nil
		}
		prompts := r.tell(r.take(emit))
		if len(prompts) == 0 && !r.waking() {
			return nil, nil
		}
		return prompts, r.takeNotices()
	}
}

// finish deregisters the ended w and starts the next turn with the input given
// after w was stopped, unless the session is closing or being switched; that
// input is then dropped. Notices alone start no turn here: a waking one would
// have continued w, unless w failed or was stopped. It must be called with
// queue held.
func (r *runner) finish(w *Work, emit func(event.Event)) {
	c := r.c
	parts := r.take(emit)
	c.mu.Lock()
	if r.work == w {
		r.work = nil
		c.signalLocked()
	}
	if len(parts) == 0 || c.closed || c.switching != nil {
		c.mu.Unlock()
		return
	}
	// w's end freed its slot in this section, so a subagent's begin refuses only
	// for a task a switch or close has forgotten, whose input is dropped anyway.
	_, _ = r.startTurn(w.base, parts)
}

// queueNotice queues n for the model and, while no turn runs, no session
// switch is under way and no login runs, starts a turn with no human prompt;
// the end of the login does that for a notice that arrived during it.
// It drops n once closed or with no session. A switch that commits clears what
// was queued; one that fails keeps it for the next prompt or wake.
func (r *runner) queueNotice(n pigruntime.Notice) {
	r.c.queue.Lock()
	defer r.c.queue.Unlock()
	r.queueNoticeLocked(n)
}

// queueNoticeLocked is queueNotice with queue held.
func (r *runner) queueNoticeLocked(n pigruntime.Notice) {
	r.c.mu.Lock()
	if r.c.closed || r.sess == nil {
		r.c.mu.Unlock()
		return
	}
	r.notices = append(r.notices, n)
	r.wakeLocked()
}

// wakeLocked starts a turn with no human prompt for the queued notices unless
// one runs, a switch is under way, a login runs or nothing queued wakes. It
// must be called with queue and mu held, and releases mu.
func (r *runner) wakeLocked() {
	c := r.c
	if c.closed || r.sess == nil || r.work != nil || c.switching != nil || r.changingModel() || c.login != nil || r.hold || !r.waking() {
		c.mu.Unlock()
		return
	}
	_, _ = r.startTurn(context.Background(), nil)
}

// takeNotices empties notices; it must be called with queue held.
func (r *runner) takeNotices() []pigruntime.Notice {
	if len(r.notices) == 0 {
		return nil
	}
	out := r.notices
	r.notices = nil
	return out
}

// waking reports whether a queued notice may start or continue a turn; it must
// be called with queue held. The notice StopTask queues for a stopped task
// never wakes; a subagent's end wakes even without Config.Wake, a shell
// command's only with it.
func (r *runner) waking() bool {
	return slices.ContainsFunc(r.notices, func(n pigruntime.Notice) bool {
		return n.Shown.Status != string(tool.TaskStopped) && (r.c.cfg.Wake || n.Shown.Agent != "")
	})
}

// requeue puts notices the model never read back ahead of those queued since;
// it must be called with queue held.
func (r *runner) requeue(ns []pigruntime.Notice) {
	if len(ns) == 0 {
		return
	}
	r.notices = append(slices.Clone(ns), r.notices...)
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
	var skills, tools []string
	var mode string
	var effort pigruntime.Effort
	for _, p := range ps {
		texts = append(texts, p.Text)
		if p.Effort != pigruntime.DefaultEffort {
			effort = p.Effort
		}
		if p.Mode != "" {
			mode = p.Mode
		}
		images = append(images, p.Images...)
		for _, sk := range p.Skills {
			if !slices.Contains(skills, sk) {
				skills = append(skills, sk)
			}
		}
		for _, name := range p.Tools {
			if !slices.Contains(tools, name) {
				tools = append(tools, name)
			}
		}
	}
	return pigruntime.Prompt{Text: strings.Join(texts, "\n\n"), Images: images, Skills: skills, Tools: tools, Mode: mode, Effort: effort}
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

// changingModel reports whether r is the main runner during a /model change;
// the caller holds mu.
func (r *runner) changingModel() bool { return r == r.c.main && r.c.changingModel }
