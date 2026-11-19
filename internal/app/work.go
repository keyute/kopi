package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

// Work is one run of a session: a sequence of sends sharing an ID, from the
// first prompt until no input is left to continue with.
type Work struct {
	id string
	// base is the context every send of the Work derives from; it is never
	// cancelled.
	base context.Context
	done chan struct{}
	// outcome and reply are written before done is closed.
	outcome event.Outcome
	reply   string

	// The fields below are guarded by Controller.queue.

	// sendCtx and cancel belong to the running send, not the whole Work.
	sendCtx context.Context
	cancel  context.CancelFunc
	// stopped is set by Interrupt, a session switch or Close; queued input is
	// then no longer handed to this Work, and input given afterwards starts the
	// next turn.
	stopped bool
	// discard is set permanently by Close, including when joining times out.
	discard bool
	// switchHold defers only the terminal event until the switch commits or
	// rolls back; done still closes so the switch can join the Work.
	switchHold bool
	terminal   *event.TurnEnd
	// handBack is the queued input taken when the Work was stopped, handed
	// back as unsent unless discarded.
	handBack []pigruntime.Prompt
	// byHuman is set when the human stopped the Work: by Interrupt, StopTask or
	// rejecting an ask without a reason.
	byHuman bool
}

func (w *Work) ID() string { return w.id }

func (w *Work) Done() <-chan struct{} { return w.done }

// Outcome is valid once Done is closed.
func (w *Work) Outcome() event.Outcome { return w.outcome }

// Reply is the final assistant message's text when the Work completed, else
// "". It is valid once Done is closed.
func (w *Work) Reply() string { return w.reply }

// WorkQueue retains root turns in start order until consumed or closed.
// Only frontends that need every turn's result should create one.
// Its state is guarded by Controller.queue.
type WorkQueue struct {
	c     *Controller
	works []*Work
}

// FollowWork starts retaining turns, including the current turn if any.
func (c *Controller) FollowWork() *WorkQueue {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	q := &WorkQueue{c: c}
	if c.main.work != nil {
		q.works = append(q.works, c.main.work)
	}
	if c.followers == nil {
		c.followers = map[*WorkQueue]struct{}{}
	}
	c.followers[q] = struct{}{}
	return q
}

// Next consumes the oldest retained turn, or reports whether subagents remain.
// Consumption and the idle check are atomic with turn and subagent transitions.
// The returned channel closes when another check may give a different result.
func (q *WorkQueue) Next() (w *Work, live bool, changed <-chan struct{}) {
	c := q.c
	c.queue.Lock()
	defer c.queue.Unlock()
	if len(q.works) > 0 {
		w = q.works[0]
		q.works[0] = nil
		q.works = q.works[1:]
	}
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return w, c.live > 0, c.changed
}

// Close releases all retained turns and stops retaining new ones.
func (q *WorkQueue) Close() {
	q.c.queue.Lock()
	defer q.c.queue.Unlock()
	delete(q.c.followers, q)
	q.works = nil
}
