// Package transcript reduces the event stream into the ordered blocks a
// frontend draws, so a restored session yields the same blocks as a live one.
package transcript

import (
	"strings"
	"time"

	"github.com/keyute/kopi/internal/event"
)

type Transcript struct {
	items []item
	// boundary is the number of items restored from history; stretches never
	// cross it and groups before it are sealed.
	boundary int
}

type kind int

const (
	userItem kind = iota
	assistantItem
	callItem
	noticeItem
	errorItem
	turnItem
)

type item struct {
	kind    kind
	text    string
	call    Call
	outcome event.Outcome
	elapsed time.Duration
}

// visible reports whether the item draws a block; whitespace-only assistant
// text does not, and so does not break a stretch of calls.
func (it item) visible() bool {
	return it.kind != assistantItem || strings.TrimSpace(it.text) != ""
}

func (t *Transcript) Apply(ev event.Event) {
	switch ev := ev.(type) {
	case event.TurnStart:
		t.items = append(t.items, item{kind: userItem, text: ev.Prompt})
	case event.TextDelta:
		if n := len(t.items); n > t.boundary && t.items[n-1].kind == assistantItem {
			t.items[n-1].text += ev.Text
			return
		}
		t.items = append(t.items, item{kind: assistantItem, text: ev.Text})
	case event.ToolStart:
		t.items = append(t.items, item{kind: callItem, call: Call{
			ID:     ev.CallID,
			Title:  ev.Title,
			Action: ev.Action,
			State:  Running,
		}})
	case event.ToolEnd:
		c := t.running(ev.CallID)
		if c == nil {
			return
		}
		c.Output = ev.Output
		if ev.IsError {
			c.State = Failed
			return
		}
		c.State = OK
		c.Summary = ev.Summary
	case event.Notice:
		t.items = append(t.items, item{kind: noticeItem, text: ev.Text})
	case event.Error:
		t.items = append(t.items, item{kind: errorItem, text: ev.Err.Error()})
	case event.TurnEnd:
		t.settleUnfinished()
		outcome := ev.Outcome
		// A turn whose Send failed carries the error's text that was just
		// drawn as the Error before it; keep the reason drawn once.
		if n := len(t.items); n > 0 && t.items[n-1].kind == errorItem && t.items[n-1].text == outcome.Message {
			outcome.Message = ""
		}
		t.items = append(t.items, item{kind: turnItem, outcome: outcome, elapsed: ev.Elapsed})
	}
}

// Restore must be called on an empty Transcript.
func (t *Transcript) Restore(history []event.Event) {
	for _, ev := range history {
		t.Apply(ev)
	}
	t.settleUnfinished()
	t.boundary = len(t.items)
}

func (t *Transcript) running(id string) *Call {
	for i := len(t.items) - 1; i >= 0; i-- {
		it := &t.items[i]
		if it.kind == callItem && it.call.ID == id && it.call.State == Running {
			return &it.call
		}
	}
	return nil
}

func (t *Transcript) settleUnfinished() {
	for i := range t.items {
		if t.items[i].kind == callItem && t.items[i].call.State == Running {
			t.items[i].call.State = Unfinished
		}
	}
}
