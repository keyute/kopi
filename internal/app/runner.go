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

	// queued is the human's input in the order it was given, prompts and user
	// commands, the only source of QueueChanged: the input the model has not read
	// yet. Prompts are queued only while a turn is registered or a command ahead
	// of them has not finished. An entry leaves it when the model reads it
	// (reading), when it is taken back (take, takeHuman, interruptCommand) or when
	// the send that was handed it ends without reading it (dropHanded).
	queued []*entry
	// notices are the task notices waiting for the model. They are never part
	// of queued: they are not the human's input, so they are never reported,
	// handed back or taken.
	notices []pigruntime.Notice
	// hold keeps notices from waking the model after an Interrupt until a human
	// prompt starts a turn.
	hold bool
	// recording is set while recordSilently's record is in flight: the entries
	// it handed are read one by one, so the queue can be empty with nothing
	// handed while the session is still inside RecordNotices, which refuses a
	// send.
	recording bool
	// wakeDue is set when a wake was refused because a record was in flight; the
	// record's end advances with it.
	wakeDue bool
	// unread is set on a subagent's runner from the moment its completion or
	// failure notice is queued until the main session's model reads that notice
	// or the subagent runs again; guarded by Controller.queue like the fields
	// above.
	unread bool
	// told is the last mode line delivered to the model in the current session;
	// "" when none was, or when one was not delivered. Only the main session's
	// is used.
	told string
}

// entry is one item of runner.queued: a prompt, or a command when cmd is set.
type entry struct {
	prompt pigruntime.Prompt
	cmd    *userCommand
	// handed is set once the entry was given to the model, in a send's hand-over
	// or a record, and has not been read yet. Handed entries are still reported,
	// but no longer belong to the human to take back or to settle to continue
	// with.
	handed bool
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
		if len(r.queued) == 0 && !r.recording {
			return r.startTurn(ctx, []pigruntime.Prompt{p})
		}
		// Behind a command that has not finished, or a record in flight.
		c.mu.Unlock()
		r.queued = append(r.queued, &entry{prompt: p})
		r.queueChanged(emit)
		c.mu.Lock()
		r.advanceLocked(true)
		return nil, nil
	}
	c.mu.Unlock()
	if p.Text != "" {
		r.queued = append(r.queued, &entry{prompt: p})
		r.queueChanged(emit)
	}
	if now && !w.stopped && hasPrompt(r.ready()) {
		// Not stopped: the end of the send continues the Work with the ready input.
		w.cancel()
	}
	return nil, nil
}

// waiting returns the prompts not yet handed to the model, in order, wherever
// they stand behind commands; it must be called with queue held.
func (r *runner) waiting() []pigruntime.Prompt {
	var out []pigruntime.Prompt
	for _, e := range r.queued {
		if !e.handed && e.cmd == nil {
			out = append(out, e.prompt)
		}
	}
	return out
}

// take removes the waiting prompts and returns them; it must be called with
// queue held.
func (r *runner) take(emit func(event.Event)) []pigruntime.Prompt {
	taken := r.waiting()
	if len(taken) > 0 {
		r.queued = slices.DeleteFunc(r.queued, func(e *entry) bool { return !e.handed && e.cmd == nil })
		r.queueChanged(emit)
	}
	return taken
}

// takeHuman removes the human's waiting prompts, leaving what other agents
// sent, and returns them; it must be called with queue held.
func (r *runner) takeHuman(emit func(event.Event)) []pigruntime.Prompt {
	var human []pigruntime.Prompt
	r.queued = slices.DeleteFunc(r.queued, func(e *entry) bool {
		if e.handed || e.cmd != nil || e.prompt.FromAgent {
			return false
		}
		human = append(human, e.prompt)
		return true
	})
	if len(human) > 0 {
		r.queueChanged(emit)
	}
	return human
}

// dropHanded removes the prompts the send that has just ended was handed and
// never read; they are what its Undelivered carries. It must be called with
// queue held.
func (r *runner) dropHanded(emit func(event.Event)) {
	human := false
	r.queued = slices.DeleteFunc(r.queued, func(e *entry) bool {
		drop := e.handed && e.cmd == nil
		human = human || drop && !e.prompt.FromAgent
		return drop
	})
	if human {
		r.queueChanged(emit)
	}
}

// reading returns what sess's events go to instead of emit: the model reading a
// handed prompt, which is the UserMessage Send reports, or a handed command,
// which is its UserCommand, removes its entry from the queue and reports the new
// queue just ahead of it, in one queue section so a frontend sees the two back
// to back. Other events pass untouched. It does nothing for a session that is no
// longer r's, so a straggler of a replaced session never pops the new one's
// queue.
func (r *runner) reading(sess *pigruntime.Session, emit func(event.Event)) func(event.Event) {
	return func(ev event.Event) {
		var command bool
		switch e := ev.(type) {
		case event.UserMessage:
		case event.UserCommand:
			command = true
		case event.TaskEnded:
			// The notice of a subagent's end is now read; it is the main session's
			// event, so a child's own subscriber never sees one.
			r.c.queue.Lock()
			if child := r.c.children[e.ID]; child != nil {
				child.unread = false
			}
			r.c.queue.Unlock()
			emit(ev)
			return
		default:
			emit(ev)
			return
		}
		r.c.queue.Lock()
		defer r.c.queue.Unlock()
		r.c.mu.Lock()
		current := r.sess == sess
		r.c.mu.Unlock()
		if current {
			r.read(emit, command)
		}
		emit(ev)
	}
}

// read removes the first handed command or prompt, in the order the model
// reads them; the kinds never mix up, as a command is recorded or noticed and a
// prompt steered. It must be called with queue held.
func (r *runner) read(emit func(event.Event), command bool) {
	i := slices.IndexFunc(r.queued, func(e *entry) bool { return e.handed && (e.cmd != nil) == command })
	if i < 0 {
		return
	}
	human := command || !r.queued[i].prompt.FromAgent
	r.queued = slices.Delete(r.queued, i, i+1)
	if human {
		r.queueChanged(emit)
	}
}

// queueChanged reports the human's input in queued, handed or not. It must be
// called with queue held, so reports arrive in the order queued changed.
func (r *runner) queueChanged(emit func(event.Event)) {
	pending := make([]event.Queued, 0, len(r.queued))
	for _, e := range r.queued {
		switch {
		case e.cmd != nil:
			pending = append(pending, event.Queued{Command: e.cmd.snapshot()})
		case !e.prompt.FromAgent:
			pending = append(pending, event.Queued{Text: e.prompt.Text})
		}
	}
	emit(event.QueueChanged{Pending: pending})
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
// takes every queued notice and the ready input, after parts, which are earlier
// than anything queued; with no parts it is a wake or a turn of the ready
// input. It returns the error of role.begin, with nothing registered.
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

// start sends the turn r.plan makes of parts on w, which is r.work and has been
// admitted by role.begin or, for a subagent's first run, by its launch. It must
// be called with queue held and mu not.
func (r *runner) start(w *Work, parts []pigruntime.Prompt) {
	c := r.c
	c.mu.Lock()
	sess, emit := r.sess, r.emit
	c.mu.Unlock()
	t := r.plan(parts, emit)
	w.record = t.record
	c.signalLocked()

	emit(event.TurnStart{WorkID: w.id, Prompt: t.text})
	sendCtx := w.sendCtx
	c.wg.Go(func() { r.runTurn(sendCtx, sess, emit, w, t.parts, t.notices) })
}

// turn is what one send of a Work starts with.
type turn struct {
	// parts is the prompt sent, and notices go with it. text is what TurnStart
	// shows of it; a turn that starts with a command shows none, as the prompt is
	// drawn where the model reads it.
	parts   []pigruntime.Prompt
	notices []pigruntime.Notice
	text    string
	// record is done before the send, for commands that must come ahead of parts.
	record *recordStep
}

// recordStep records notices, the commands that precede the prompts in the
// request, before the send. The prompts are in the queue, handed, until then.
type recordStep struct {
	notices []pigruntime.Notice
	cmds    []*entry
	prompts []*entry
	text    string
	// drawn is set once the prompts were shown as read, which the send that
	// follows may still not deliver.
	drawn bool
}

// plan decides how a turn starts and marks what it takes. left is the input
// that was typed before everything queued; the ready prefix is
// [c1..ck, p1..pm, rest…], leading commands then the first run of prompts.
//   - left: it is the prompt, with c1..ck as its notices; the prompts follow at
//     the first boundary.
//   - c and p: the commands are recorded, then the prompts are the prompt, so
//     one request reads them in typing order.
//   - c: the commands alone wake the model.
//   - p: the prompts are the prompt.
//
// The caller has decided that the turn may start. It must be called with queue
// held.
func (r *runner) plan(left []pigruntime.Prompt, emit func(event.Event)) turn {
	prefix := r.ready()
	k := 0
	for k < len(prefix) && prefix[k].cmd != nil {
		k++
	}
	m := k
	for m < len(prefix) && prefix[m].cmd == nil {
		m++
	}
	cmds, run := prefix[:k], prefix[k:m]
	var t turn
	notices := r.commandNotices(cmds)
	switch {
	case len(left) > 0:
		t.parts = r.tell(left)
		t.text = joined(t.parts).Text
		t.notices = notices
	case len(cmds) > 0 && len(run) > 0:
		t.parts = r.tell(prompts(run))
		for _, e := range run {
			e.handed = true
		}
		t.record = &recordStep{notices: notices, cmds: cmds, prompts: run, text: joined(t.parts).Text}
	case len(cmds) > 0:
		t.notices = notices
	case len(run) > 0:
		t.parts = r.tell(prompts(run))
		t.text = joined(t.parts).Text
		r.remove(run)
		r.queueChanged(emit)
	}
	t.notices = append(t.notices, r.takeNotices()...)
	if len(t.parts) > 0 {
		r.hold = false
	}
	return t
}

func prompts(es []*entry) []pigruntime.Prompt {
	var out []pigruntime.Prompt
	for _, e := range es {
		out = append(out, e.prompt)
	}
	return out
}

// remove takes es out of the queue; it must be called with queue held.
func (r *runner) remove(es []*entry) {
	r.queued = slices.DeleteFunc(r.queued, func(e *entry) bool { return slices.Contains(es, e) })
}

// ready returns the entries the model may be given now, in order: those not
// handed, from the head up to the first command that has not finished. Nothing
// is given before an entry typed earlier. It must be called with queue held.
func (r *runner) ready() []*entry {
	var out []*entry
	for _, e := range r.queued {
		if e.cmd != nil && (e.cmd.state == event.CommandQueued || e.cmd.state == event.CommandRunning) {
			break
		}
		if !e.handed {
			out = append(out, e)
		}
	}
	return out
}

func hasPrompt(es []*entry) bool {
	return slices.ContainsFunc(es, func(e *entry) bool { return e.cmd == nil })
}

// replies reports whether es hold a command that asks for a reply; the human's
// stop of a turn makes every command silent until their next input.
func (r *runner) replies(es []*entry) bool {
	return !r.hold && slices.ContainsFunc(es, func(e *entry) bool { return e.cmd != nil && e.cmd.state == event.CommandExited })
}

// lead returns the run of prompts at the head of the ready entries.
func (r *runner) lead() []*entry {
	var out []*entry
	for _, e := range r.ready() {
		if e.cmd != nil {
			break
		}
		out = append(out, e)
	}
	return out
}

// commandNotices marks the commands of es handed and returns what the model
// reads of them.
func (r *runner) commandNotices(es []*entry) []pigruntime.Notice {
	var out []pigruntime.Notice
	for _, e := range es {
		if e.cmd != nil {
			e.handed = true
			out = append(out, e.cmd.notice(r.c.cfg.Wake))
		}
	}
	return out
}

// unhand returns es, which a record handed and did not read, to the queue, in
// place; a record never touches what another hand-over gave the model.
func (r *runner) unhand(es []*entry) {
	for _, e := range es {
		e.handed = false
	}
}

// reclaim settles what the send that has just ended was handed and never read.
// Commands go back to their place in the queue. Prompts do too when the Work
// continues, so the next send is planned from the queue in typing order, and the
// mode line they carried is told again; when it does not, they are what the
// send's Undelivered says, handed back through the turn's end. It must be called
// with queue held.
func (r *runner) reclaim(emit func(event.Event), cont bool, undelivered []pigruntime.Prompt) {
	for _, e := range r.queued {
		if e.cmd != nil || cont {
			e.handed = false
		}
	}
	if cont {
		r.untell(undelivered)
		return
	}
	r.dropHanded(emit)
}

func (r *runner) anyHanded() bool {
	return slices.ContainsFunc(r.queued, func(e *entry) bool { return e.handed })
}

// runTurn sends parts and notices, then keeps sending while settle finds input
// to continue with or the ready input asks for a reply. The decision, the final
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
		var res pigruntime.SendResult
		var err error
		var drawn *recordStep
		if rec := w.record; rec != nil {
			w.record = nil
			drawn = rec
			if err = r.record(ctx, sess, emit, rec); err != nil {
				res.UndeliveredNotices = notices
			}
		}
		if err == nil {
			p := joined(parts)
			p.Continuation = carry
			res, err = sess.Send(ctx, p, notices, r.pull(ctx, emit))
		}
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
			// A command stays queued until the model reads it.
			r.requeue(slices.DeleteFunc(slices.Clone(res.UndeliveredNotices), func(n pigruntime.Notice) bool {
				_, command := n.Shown.(event.UserCommand)
				return command
			}))
		}
		if res.ModeUndelivered {
			r.told = ""
		}
		// A turn that went on may be continued; one that failed or stopped hands
		// back every prompt still waiting.
		cont := !w.stopped && (cancelled || res.Outcome.Status == event.Completed && res.Sent)
		undelivered := res.Undelivered
		if cont {
			undelivered = nil
		}
		r.reclaim(emit, cont, res.Undelivered)
		next, unsent := settle(w.stopped, cancelled, res.Outcome, res.Sent, parts, undelivered, w.handBack, prompts(r.lead()))
		r.untell(next, unsent)
		if !w.stopped {
			lead := r.lead()
			r.remove(lead)
			var rest []pigruntime.Prompt
			if !cont {
				rest = r.take(emit)
				unsent = slices.Concat(unsent, rest)
			}
			if len(lead) > 0 && len(rest) == 0 {
				r.queueChanged(emit)
			}
		}
		// A failed or unsent send never continues for notices alone, so a
		// failing provider cannot be woken in a loop.
		again := cont && (len(next) > 0 || r.opens(r.ready(), true))
		carry = nil
		if cancelled {
			carry = res.Continuation
		}
		if again {
			ctx, w.cancel = context.WithCancel(w.base)
			w.sendCtx = ctx
			t := r.plan(next, emit)
			if drawn != nil && drawn.drawn && !res.Sent {
				// The record showed parts as read and the send never took them; they go
				// again first, so the transcript shows only what follows.
				t.text = joined(next[len(parts):]).Text
			}
			parts, notices, w.record = t.parts, t.notices, t.record
			emit(event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed})
			emit(event.TurnStart{WorkID: w.id, Prompt: t.text})
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
		end := event.TurnEnd{WorkID: w.id, Outcome: res.Outcome, Elapsed: elapsed, Unsent: c.inputs(unsent)}
		if w.switchHold {
			w.terminal = &end
		} else {
			emit(end)
		}
		r.finish(w)
		c.queue.Unlock()
		close(w.done)
		return
	}
}

// opens reports whether the ready entries es, with the queued notices, start or
// continue a turn: a prompt always does, and a command that finished or a
// waking notice does when wake is set and the human has not stopped the model.
// It must be called with queue held.
func (r *runner) opens(es []*entry, wake bool) bool {
	return hasPrompt(es) || wake && !r.hold && (r.replies(es) || r.waking())
}

// record records rec's commands, then removes its prompts from the queue and
// draws them where the model reads them. It runs before the send, with no lock
// held, as recording reads the entries back through reading. A command that was
// not recorded goes back to the queue, and a prompt the send never gets is
// handed back as unsent by the turn's end.
func (r *runner) record(ctx context.Context, sess *pigruntime.Session, emit func(event.Event), rec *recordStep) error {
	err := sess.RecordNotices(rec.notices)
	r.c.queue.Lock()
	defer r.c.queue.Unlock()
	r.unhand(rec.cmds)
	r.remove(rec.prompts)
	r.queueChanged(emit)
	if err == nil && ctx.Err() == nil {
		rec.drawn = true
		emit(event.UserMessage{Text: rec.text})
	}
	return err
}

// pull returns the callback through which the send on ctx is handed the ready
// entries and the queued notices at a boundary. The entries stay queued, marked
// handed, until the model reads them. Entries go only when they hold a prompt
// or a command that asks for a reply, or a waking notice is queued: at a run's
// last boundary a hand-over extends the run by a request, which a command that
// was stopped or moved, or a non-waking notice, alone must not cause. It hands
// nothing over once ctx is cancelled: a stop or a send-now cancels ctx in the
// same queue section as it changes the queue, and input handed to a run being
// cancelled would enter history with no reply.
func (r *runner) pull(ctx context.Context, emit func(event.Event)) func() []pigruntime.Entry {
	return func() []pigruntime.Entry {
		r.c.queue.Lock()
		defer r.c.queue.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		prefix := r.ready()
		if !hasPrompt(prefix) && !r.replies(prefix) && !r.waking() {
			return nil
		}
		var texts []pigruntime.Prompt
		for _, e := range prefix {
			if e.cmd == nil {
				texts = append(texts, e.prompt)
			}
		}
		told := r.tell(texts)
		var entries []pigruntime.Entry
		for _, e := range prefix {
			e.handed = true
			if e.cmd != nil {
				n := e.cmd.notice(r.c.cfg.Wake)
				entries = append(entries, pigruntime.Entry{Notice: &n})
				continue
			}
			entries = append(entries, pigruntime.Entry{Prompt: told[0]})
			told = told[1:]
		}
		for _, n := range r.takeNotices() {
			entries = append(entries, pigruntime.Entry{Notice: &n})
		}
		return entries
	}
}

// finish deregisters the ended w and lets the queue go on: input given after w
// was stopped starts the next turn, unless the session is closing or being
// switched, which keep it queued; a command that finished is recorded without a
// reply, as w failed or was stopped, or it would have continued w. Notices alone
// start no turn here: a waking one would have continued w, unless w failed or
// was stopped. It must be called with queue held.
func (r *runner) finish(w *Work) {
	c := r.c
	c.mu.Lock()
	if r.work == w {
		r.work = nil
		c.signalLocked()
	}
	// w's end freed its slot in this section, so a subagent's begin refuses only
	// for a task a switch or close has forgotten, whose input is dropped.
	r.advanceLocked(false)
}

// advanceLocked starts what the queue calls for while no turn runs: a turn of
// the ready entries when they hold a prompt, or, when wake is set, a wake for a
// command that finished or a waking notice; else it records the ready commands
// without a reply, as nothing finished stays queued with no turn running. It
// does nothing while a turn runs, a switch, a model change or a login is under
// way, or a record is in flight (an entry is handed, or recording is set). It
// must be called with queue and mu held, and releases mu.
func (r *runner) advanceLocked(wake bool) {
	c := r.c
	// A login holds the main session's wakes and silent records; a subagent's run
	// is independent of it.
	if c.closed || r.sess == nil || r.work != nil || c.switching != nil || r.changingModel() || r == c.main && c.login != nil || r.anyHanded() || r.recording {
		if r.recording && wake {
			r.wakeDue = true
		}
		c.mu.Unlock()
		return
	}
	prefix := r.ready()
	switch {
	case r.opens(prefix, wake):
		emit := r.emit
		if _, err := r.startTurn(context.Background(), nil); err != nil {
			r.take(emit)
		}
	case len(prefix) > 0:
		r.recordSilently(prefix, wake)
	default:
		c.mu.Unlock()
	}
}

// recordSilently records the commands of prefix with no request to the model. It
// releases mu and records off the queue lock, as recording reads the entries
// back through reading. A failure is reported and not retried: the next input or
// the end of the next command tries again.
func (r *runner) recordSilently(prefix []*entry, wake bool) {
	c := r.c
	sess, emit := r.sess, r.emit
	notices := r.commandNotices(prefix)
	r.recording = true
	c.mu.Unlock()
	c.wg.Go(func() {
		err := sess.RecordNotices(notices)
		c.queue.Lock()
		defer c.queue.Unlock()
		r.recording = false
		due := r.wakeDue
		r.wakeDue = false
		r.unhand(prefix)
		// An entry still queued was not read, as a session that has been replaced
		// does not report to the queue.
		if err != nil {
			emit(event.Error{Err: err})
			// A prompt typed, or a wake refused, while the record was in flight has no
			// other turn to start it; advancing when neither opens a turn would record
			// the same commands again.
			if hasPrompt(r.ready()) || due && r.opens(r.ready(), true) {
				c.mu.Lock()
				r.advanceLocked(due)
			}
			return
		}
		if slices.ContainsFunc(prefix, func(e *entry) bool { return slices.Contains(r.queued, e) }) {
			return
		}
		c.mu.Lock()
		r.advanceLocked(wake || due)
	})
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
	r.advanceLocked(true)
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
		switch s := n.Shown.(type) {
		case event.TaskEnded:
			return s.Status != string(tool.TaskStopped) && (r.c.cfg.Wake || s.Agent != "")
		case event.UserCommand:
			return s.State == event.CommandExited
		}
		return false
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
// the send was handed but the model never read; handBack is what was waiting
// when the Work was stopped. Input pending after a stop is neither: it starts the
// next turn.
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

// inputs is what prompts are handed back to the human as. Text that opens with
// "!" gets a space first when a shell would take it, so the draft is still the
// prompt it was.
func (c *Controller) inputs(ps []pigruntime.Prompt) []event.Input {
	if len(ps) == 0 {
		return nil
	}
	out := make([]event.Input, 0, len(ps))
	for _, p := range ps {
		in := event.Input{Text: p.Text}
		if c.cfg.UserShell != nil {
			in.Text = AsPrompt(p.Text)
		}
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
