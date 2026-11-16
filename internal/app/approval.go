package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
)

// pendingApproval is the one call waiting for a human answer. PiG runs a
// session's hooks one at a time and only the current session sends, so one
// slot is enough.
type pendingApproval struct {
	callID string
	// answer has room for the one answer, so Answer never blocks.
	answer chan policy.Answer
}

// approver returns the Approver for a session displayed by d; nil when no
// human can answer.
func (c *Controller) approver(d *display) policy.Approver {
	if c.cfg.Unattended {
		return nil
	}
	return func(ctx context.Context, r policy.Request) (policy.Answer, error) {
		p := &pendingApproval{callID: r.CallID, answer: make(chan policy.Answer, 1)}
		c.mu.Lock()
		c.approval = p
		// The approver runs only during a send on the current session, so
		// emit is that session's.
		emit := c.emit
		c.mu.Unlock()
		emit(d.approval(r))
		defer emit(event.ApprovalEnded{CallID: r.CallID})
		select {
		case a := <-p.answer:
			return a, nil
		case <-ctx.Done():
			c.mu.Lock()
			unanswered := c.approval == p
			if unanswered {
				c.approval = nil
			}
			c.mu.Unlock()
			if unanswered {
				return policy.Reject, ctx.Err()
			}
			// Answer claimed the slot first and its answer is on the way; a
			// rejection interrupts before it is sent, and must still reach the
			// model as a rejection rather than as a cancel.
			return <-p.answer, nil
		}
	}
}

// Answer resolves the pending approval for callID. It returns false when no
// approval for callID is pending. A rejection also interrupts the turn, which
// hands queued input back; the model still sees the rejection as the call's
// result.
func (c *Controller) Answer(callID string, a policy.Answer) bool {
	c.mu.Lock()
	p := c.approval
	if p == nil || p.callID != callID {
		c.mu.Unlock()
		return false
	}
	c.approval = nil
	c.mu.Unlock()
	if a == policy.Reject {
		// Interrupting before delivering means the send is already cancelled
		// when the rejection returns, so the turn cannot go on to another
		// model request.
		c.Interrupt()
	}
	p.answer <- a
	return true
}
