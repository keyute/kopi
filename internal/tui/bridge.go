package tui

import (
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

// eventsMsg is every event queued since the last delivery, in order.
type eventsMsg []event.Event

// bridge carries Controller events into the program. push never blocks, as
// the Controller requires of a subscriber; events queue until the program
// drains them, so none emitted before it starts are lost.
type bridge struct {
	mu      sync.Mutex
	pending []event.Event
	// signal holds at most one wake-up for any number of pushes.
	signal chan struct{}
	done   chan struct{}
}

func newBridge() *bridge {
	return &bridge{signal: make(chan struct{}, 1), done: make(chan struct{})}
}

func (b *bridge) push(ev event.Event) {
	b.mu.Lock()
	b.pending = append(b.pending, ev)
	b.mu.Unlock()
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

func (b *bridge) drain() []event.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	evs := b.pending
	b.pending = nil
	return evs
}

// wait delivers the next batch; it must be re-armed after each delivery.
func (b *bridge) wait() tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case <-b.done:
				return nil
			case <-b.signal:
			}
			if evs := b.drain(); len(evs) > 0 {
				return eventsMsg(evs)
			}
		}
	}
}

// close releases a pending wait once the program has stopped.
func (b *bridge) close() { close(b.done) }
