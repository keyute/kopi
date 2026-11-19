package app

import (
	"context"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
)

// pendingApproval is a call waiting for a human answer. A session runs its
// hooks one at a time, so it has at most one; the root's and the subagents' wait
// together, first in first out.
type pendingApproval struct {
	token uint64
	// work is the run that asked, on runner's session.
	work   *Work
	runner *runner
	ctx    context.Context
	// emit is the runner's, as it was when the ask began.
	emit func(event.Event)
	// PiG forwards send cancellation asynchronously; retain the originating
	// send even when the Work continues with a new context.
	sendCtx context.Context
	// answer has room for the one answer, so Answer never blocks.
	answer  chan policy.Decision
	claimed bool // guarded by Controller.mu; cancellation must wait for a claimed answer
}

// stale reports that the send the ask belongs to is stopping; it must be
// called with queue and mu held.
func (p *pendingApproval) stale() bool {
	return p.work.stopped || p.sendCtx.Err() != nil || p.ctx.Err() != nil
}

// approver returns the Approver for the session of run, displayed by d; nil
// when no human can answer.
func (c *Controller) approver(d *display, run *runner) policy.Approver {
	if c.cfg.Unattended {
		return nil
	}
	return func(ctx context.Context, call policy.Request) (policy.Decision, error) {
		return c.ask(ctx, d, call, run)
	}
}

// ask registers call, reports it and waits for its answer. It needs a running,
// unstopped run on run's session.
func (c *Controller) ask(ctx context.Context, d *display, call policy.Request, run *runner) (policy.Decision, error) {
	p := &pendingApproval{answer: make(chan policy.Decision, 1), runner: run, ctx: ctx}
	c.queue.Lock()
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return policy.Decision{}, err
	}
	w := run.work
	if c.closed || w == nil || w.stopped || w.sendCtx.Err() != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return policy.Decision{}, context.Canceled
	}
	// The approver runs only during a send on run's session.
	p.work, p.sendCtx, p.emit = w, w.sendCtx, run.emit
	emit := run.bound
	c.approvalSeq++
	p.token = c.approvalSeq
	c.approvals = append(c.approvals, p)
	c.mu.Unlock()
	req := d.approval(call)
	req.Token = p.token
	req.Agent, req.TaskID = run.name, run.id
	emit(req)
	c.queue.Unlock()
	defer emit(event.ApprovalEnded{Token: p.token})
	select {
	case a := <-p.answer:
		return a, nil
	case <-ctx.Done():
		c.queue.Lock()
		c.mu.Lock()
		claimed := p.claimed
		c.approvals = slices.DeleteFunc(c.approvals, func(q *pendingApproval) bool { return q == p })
		c.mu.Unlock()
		c.queue.Unlock()
		if !claimed {
			return policy.Decision{}, ctx.Err()
		}
		// Answer claimed the ask first and its answer is on the way; a
		// rejection without a reason interrupts before it is sent, and must
		// still reach the model as a rejection rather than as a cancel.
		return <-p.answer, nil
	}
}

// reviewer returns the Reviewer for a session whose calls rec records; nil
// when no classifier is configured.
func (c *Controller) reviewer(rec *classifier.Record) policy.Reviewer {
	if c.cfg.Classifier == nil {
		return nil
	}
	return func(ctx context.Context, r policy.Request) (policy.Review, error) {
		return c.cfg.Classifier.Review(ctx, rec.Conversation(r.CallID), r)
	}
}

// Answer resolves the pending approval for token. It returns false when no
// approval for token is pending. A rejection without a reason also interrupts
// the root's turn, which hands queued input back, or stops the subagent that
// asked; with one the turn or subagent goes on. Either way the model sees the
// rejection as the call's result.
func (c *Controller) Answer(token uint64, d policy.Decision) bool {
	d.Reason = strings.TrimSpace(d.Reason)
	stop := d.Answer == policy.Reject && d.Reason == ""
	c.queue.Lock()
	c.mu.Lock()
	i := slices.IndexFunc(c.approvals, func(p *pendingApproval) bool { return p.token == token })
	if i < 0 {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p := c.approvals[i]
	c.approvals = slices.Delete(c.approvals, i, i+1)
	if p.stale() {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p.claimed = true
	c.mu.Unlock()
	if stop {
		p.work.byHuman = true
		p.runner.stop(p.work, p.emit)
	}
	c.queue.Unlock()
	if stop {
		// Cancellation forwarding is asynchronous. Stop PiG synchronously
		// before releasing the rejection, without holding controller locks.
		pigruntime.InterruptTurn(p.ctx)
	}
	p.answer <- d
	return true
}
