// Package transcript reduces the event stream into the ordered blocks a
// frontend draws, so a restored session yields the same blocks as a live one.
package transcript

import (
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
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
	taskItem
	commandItem
)

type item struct {
	kind     kind
	text     string
	finished bool
	call     Call
	outcome  event.Outcome
	elapsed  time.Duration
	task     event.TaskEnded
	command  event.UserCommand
}

// visible reports whether the item draws a block; whitespace-only assistant
// text does not, and so does not break a stretch of calls.
func (it item) visible() bool {
	return it.kind != assistantItem || strings.TrimSpace(it.text) != ""
}

func (t *Transcript) Apply(ev event.Event) {
	switch ev := ev.(type) {
	case event.TurnStart:
		// A wake turn, started by a background task's end, has no prompt.
		if ev.Prompt != "" {
			t.items = append(t.items, item{kind: userItem, text: ev.Prompt})
		}
	case event.UserMessage:
		t.finishReply()
		// History restores a steered message as a TurnStart; both draw the
		// same block.
		t.items = append(t.items, item{kind: userItem, text: ev.Text})
	case event.TextDelta:
		if n := len(t.items); n > t.boundary && t.items[n-1].kind == assistantItem {
			t.items[n-1].text += ev.Text
			return
		}
		t.items = append(t.items, item{kind: assistantItem, text: ev.Text})
	case event.ToolStart:
		t.finishReply()
		t.items = append(t.items, item{kind: callItem, call: Call{
			ID:          ev.CallID,
			Title:       ev.Title,
			Command:     ev.Command,
			Action:      ev.Action,
			State:       Running,
			Preview:     ev.Preview,
			Unsandboxed: ev.Unsandboxed,
			solo:        ev.Solo,
			done:        ev.Done,
			change:      ev.Change,
		}})
	case event.ToolEnd:
		c := t.running(ev.CallID)
		if c == nil {
			return
		}
		done, change := c.done, c.change
		c.done, c.change = "", ""
		c.Body = ev.Body
		switch {
		case ev.Kind == event.EndInterrupted:
			c.State = Unfinished
		case ev.Kind == event.EndDenied:
			c.State = Denied
		case ev.Kind == event.EndRejected:
			c.State = Rejected
		case ev.IsError:
			c.State = Failed
		default:
			c.State = OK
			if ev.Done != "" {
				done = ev.Done
			}
			if done != "" {
				c.Title = done
			}
			if ev.Action != (tool.Action{}) {
				c.Action = ev.Action
			}
			c.Subject = ev.Subject
			c.Summary = strings.Join(slices.DeleteFunc([]string{change, ev.Summary}, func(s string) bool { return s == "" }), " · ")
			return
		}
		c.Summary = ev.Summary
		c.Preview = nil
	case event.Notice:
		t.items = append(t.items, item{kind: noticeItem, text: ev.Text})
	case event.Error:
		t.items = append(t.items, item{kind: errorItem, text: ev.Err.Error()})
	case event.TurnEnd:
		t.finishReply()
		t.settleUnfinished()
		t.items = append(t.items, item{kind: turnItem, outcome: ev.Outcome, elapsed: ev.Elapsed})
	case event.TaskEnded:
		t.finishReply()
		t.items = append(t.items, item{kind: taskItem, task: ev})
	case event.UserCommand:
		t.finishReply()
		t.items = append(t.items, item{kind: commandItem, command: ev})
	case event.PlanRequested:
		// The plan's tool is exclusive, so its call is the one solo call running.
		for i := len(t.items) - 1; i >= 0; i-- {
			if it := &t.items[i]; it.kind == callItem && it.call.solo && it.call.State == Running {
				it.call.Plan = ev.Plan
				return
			}
		}
	}
}

// LastReply is the raw text of the last finished assistant reply, or "" when
// there is none. Restored text is eligible even without a reply-ending event.
func (t *Transcript) LastReply() string {
	for i := len(t.items) - 1; i >= 0; i-- {
		it := t.items[i]
		if it.kind != assistantItem || !it.visible() || !it.finished && i >= t.boundary {
			continue
		}
		return it.text
	}
	return ""
}

func (t *Transcript) finishReply() {
	for i := len(t.items) - 1; i >= t.boundary; i-- {
		it := &t.items[i]
		if it.kind == callItem || it.kind == turnItem {
			break
		}
		if it.kind == assistantItem {
			it.finished = true
		}
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

// Running is the title of the latest call still running.
func (t *Transcript) Running() (title string, ok bool) {
	for i := len(t.items) - 1; i >= 0; i-- {
		if it := t.items[i]; it.kind == callItem && it.call.State == Running {
			return it.call.Title, true
		}
	}
	return "", false
}

func (t *Transcript) settleUnfinished() {
	for i := range t.items {
		if t.items[i].kind == callItem && t.items[i].call.State == Running {
			t.items[i].call.State = Unfinished
		}
	}
}
