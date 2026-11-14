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
	// outcome is written before done is closed.
	outcome event.Outcome

	// The fields below are guarded by Controller.queue.

	// cancel stops the running send only.
	cancel context.CancelFunc
	// next is input taken out of PiG's queue for this Work: sent as the next
	// send unless stopped, otherwise handed back as unsent unless discarded.
	next []pigruntime.Prompt
	// stopped is set by Interrupt, a session switch or Close; new input then
	// waits for the next turn rather than being steered into this one.
	stopped bool
	// discard is set by a session switch or Close: input left over when the
	// Work ends is dropped rather than handed back.
	discard bool
	// steered is whether PiG's queue may hold input since it was last taken.
	steered bool
	// ending is set once the Work has decided not to continue; input can no
	// longer be steered into it.
	ending bool
}

func (w *Work) ID() string { return w.id }

func (w *Work) Done() <-chan struct{} { return w.done }

// Outcome is valid once Done is closed.
func (w *Work) Outcome() event.Outcome { return w.outcome }
