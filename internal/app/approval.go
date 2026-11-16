package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
)

// pendingApproval is the one call waiting for a human answer. PiG runs a
// session's hooks one at a time and only the current session sends, so one
// slot is enough.
type pendingApproval struct {
	token uint64
	work  *Work
	ctx   context.Context
	emit  func(event.Event)
	// PiG forwards send cancellation asynchronously; retain the originating
	// send even when the Work continues with a new context.
	sendCtx context.Context
	// answer has room for the one answer, so Answer never blocks.
	answer  chan policy.Answer
	claimed bool // guarded by Controller.mu; cancellation must wait for a claimed answer
}

// approver returns the Approver for a session displayed by d; nil when no
// human can answer.
func (c *Controller) approver(d *display) policy.Approver {
	if c.cfg.Unattended {
		return nil
	}
	return func(ctx context.Context, r policy.Request) (policy.Answer, error) {
		p := &pendingApproval{answer: make(chan policy.Answer, 1)}
		c.queue.Lock()
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			c.queue.Unlock()
			return policy.Reject, err
		}
		if c.turn == nil || c.turn.stopped || c.turn.sendCtx.Err() != nil {
			c.mu.Unlock()
			c.queue.Unlock()
			return policy.Reject, context.Canceled
		}
		c.approvalSeq++
		p.token = c.approvalSeq
		// The approver runs only during a send on the current session.
		p.work, p.ctx, p.sendCtx, p.emit = c.turn, ctx, c.turn.sendCtx, c.emit
		c.approval = p
		emit := p.emit
		c.mu.Unlock()
		req := d.approval(r)
		req.Token = p.token
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
			if c.approval == p {
				c.approval = nil
			}
			c.mu.Unlock()
			c.queue.Unlock()
			if !claimed {
				return policy.Reject, ctx.Err()
			}
			// Answer claimed the slot first and its answer is on the way; a
			// rejection interrupts before it is sent, and must still reach the
			// model as a rejection rather than as a cancel.
			return <-p.answer, nil
		}
	}
}

// Answer resolves the pending approval for token. It returns false when no
// approval for token is pending. A rejection also interrupts the turn, which
// hands queued input back; the model still sees the rejection as the call's
// result.
func (c *Controller) Answer(token uint64, a policy.Answer) bool {
	c.queue.Lock()
	c.mu.Lock()
	p := c.approval
	if p == nil || p.token != token {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	if p.work.stopped || p.sendCtx.Err() != nil || p.ctx.Err() != nil {
		c.approval = nil
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p.claimed = true
	c.approval = nil
	c.mu.Unlock()
	if a == policy.Reject {
		c.stop(p.work, p.emit)
	}
	c.queue.Unlock()
	if a == policy.Reject {
		// Cancellation forwarding is asynchronous. Stop PiG synchronously
		// before releasing the rejection, without holding controller locks.
		pigruntime.InterruptTurn(p.ctx)
	}
	p.answer <- a
	return true
}
