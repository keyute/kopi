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
	// work is the root turn that asked; child is the subagent that asked when
	// work is nil.
	work  *Work
	child *child
	ctx   context.Context
	emit  func(event.Event)
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
	return p.work != nil && p.work.stopped || p.sendCtx.Err() != nil || p.ctx.Err() != nil
}

// approver returns the Approver for the root session displayed by d; nil when
// no human can answer.
func (c *Controller) approver(d *display) policy.Approver {
	if c.cfg.Unattended {
		return nil
	}
	return func(ctx context.Context, r policy.Request) (policy.Decision, error) {
		return c.ask(ctx, d, r, nil)
	}
}

// childApprover is approver for the subagent ch.
func (c *Controller) childApprover(ch *child) policy.Approver {
	if c.cfg.Unattended {
		return nil
	}
	return func(ctx context.Context, r policy.Request) (policy.Decision, error) {
		return c.ask(ctx, ch.d, r, ch)
	}
}

// ask registers r, reports it and waits for its answer. The root's call needs a
// running root turn; a subagent's only an open controller and a live run.
func (c *Controller) ask(ctx context.Context, d *display, r policy.Request, ch *child) (policy.Decision, error) {
	p := &pendingApproval{answer: make(chan policy.Decision, 1), child: ch, ctx: ctx}
	c.queue.Lock()
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return policy.Decision{}, err
	}
	var emit func(event.Event)
	if ch == nil {
		if c.turn == nil || c.turn.stopped || c.turn.sendCtx.Err() != nil {
			c.mu.Unlock()
			c.queue.Unlock()
			return policy.Decision{}, context.Canceled
		}
		// The approver runs only during a send on the current session.
		p.work, p.sendCtx, emit = c.turn, c.turn.sendCtx, c.emit
	} else {
		if c.closed || !ch.running || ch.runCtx.Err() != nil {
			c.mu.Unlock()
			c.queue.Unlock()
			return policy.Decision{}, context.Canceled
		}
		// A subagent's events never go through the root session's bound emit.
		p.sendCtx, emit = ch.runCtx, c.relay.emit
	}
	c.approvalSeq++
	p.token = c.approvalSeq
	p.emit = emit
	c.approvals = append(c.approvals, p)
	c.mu.Unlock()
	req := d.approval(r)
	req.Token = p.token
	if ch != nil {
		req.Agent, req.TaskID = ch.spec.def.Name, ch.task.ID()
	}
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
		if p.child != nil {
			p.child.userStopped = true
			p.child.cancel()
		} else {
			c.stop(p.work, p.emit)
		}
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
