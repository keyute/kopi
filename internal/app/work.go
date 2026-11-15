package app

import (
	"context"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

// Work is one root turn: a sequence of sends sharing an ID, from the first
// prompt until no input is left to continue with.
type Work struct {
	id   string
	done chan struct{}
	// outcome and reply are written before done is closed.
	outcome event.Outcome
	reply   string

	// The fields below are guarded by Controller.queue.

	// cancel stops the running send only.
	cancel context.CancelFunc
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
}

func (w *Work) ID() string { return w.id }

func (w *Work) Done() <-chan struct{} { return w.done }

// Outcome is valid once Done is closed.
func (w *Work) Outcome() event.Outcome { return w.outcome }

// Reply is the final assistant message's text when the Work completed, else
// "". It is valid once Done is closed.
func (w *Work) Reply() string { return w.reply }
