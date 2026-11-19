package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// pending is the one exclusive tool call waiting for a human answer of type T.
// The tools are Exclusive, so their calls run one at a time and one slot per
// tool is enough.
type pending[T any] struct {
	token   uint64
	work    *Work
	sendCtx context.Context
	ctx     context.Context
	emit    func(event.Event)
	// answer has room for the one answer, so resolving never blocks.
	answer  chan T
	claimed bool // guarded by Controller.mu; cancellation must wait for a claimed answer
}

// live reports whether the call can still be answered. It must be called with
// Controller.mu held.
func (p *pending[T]) live() bool {
	return !p.work.stopped && p.sendCtx.Err() == nil && p.ctx.Err() == nil
}

// request registers a pending call in slot, emits requested(token) and waits for
// resolve to answer it, returning the context's error or context.Canceled when
// the call is cancelled first.
func request[T any](c *Controller, ctx context.Context, slot **pending[T], seq *uint64, requested, ended func(token uint64) event.Event) (answer T, err error) {
	p := &pending[T]{answer: make(chan T, 1)}
	c.queue.Lock()
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return answer, err
	}
	if c.main.work == nil || c.main.work.stopped || c.main.work.sendCtx.Err() != nil {
		c.mu.Unlock()
		c.queue.Unlock()
		return answer, context.Canceled
	}
	*seq++
	p.token = *seq
	p.work, p.ctx, p.sendCtx, p.emit = c.main.work, ctx, c.main.work.sendCtx, c.main.emit
	*slot = p
	emit := p.emit
	c.mu.Unlock()
	emit(requested(p.token))
	c.queue.Unlock()
	defer emit(ended(p.token))
	select {
	case a := <-p.answer:
		return a, nil
	case <-ctx.Done():
	case <-p.sendCtx.Done():
	}
	c.queue.Lock()
	c.mu.Lock()
	claimed := p.claimed
	if *slot == p {
		*slot = nil
	}
	c.mu.Unlock()
	c.queue.Unlock()
	if claimed {
		// resolve claimed the slot first and its answer is on the way; a
		// dismissal interrupts before it is sent, and must still reach the model
		// as a dismissal rather than as a cancel.
		return <-p.answer, nil
	}
	if err := ctx.Err(); err != nil {
		return answer, err
	}
	return answer, context.Canceled
}

// resolve answers the call pending in slot for token and reports whether one was
// pending. A dismissal also stops the turn, which hands queued input back; the
// model still sees it as the call's result. onAnswer runs in the same queue
// section as the claim, so what it changes is never seen half-done.
func resolve[T any](c *Controller, slot **pending[T], token uint64, a T, dismissed bool, onAnswer func()) bool {
	c.queue.Lock()
	c.mu.Lock()
	p := *slot
	if p == nil || p.token != token {
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	if !p.live() {
		*slot = nil
		c.mu.Unlock()
		c.queue.Unlock()
		return false
	}
	p.claimed = true
	*slot = nil
	c.mu.Unlock()
	if dismissed {
		c.main.stop(p.work, p.emit)
	}
	if onAnswer != nil {
		onAnswer()
	}
	c.queue.Unlock()
	if dismissed {
		// Cancellation forwarding is asynchronous. Stop PiG synchronously
		// before releasing the dismissal, without holding controller locks.
		pigruntime.InterruptTurn(p.ctx)
	}
	p.answer <- a
	return true
}

type (
	pendingQuestion = pending[[]tool.Answer]
	pendingPlan     = pending[tool.PlanAnswer]
)
