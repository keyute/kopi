package pigruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/event"
)

func translate(ev agent.AgentEvent) (event.Event, bool) {
	switch ev := ev.(type) {
	case agent.MessageUpdateEvent:
		switch d := ev.AssistantMessageEvent.(type) {
		case ai.TextDeltaEvent:
			return event.TextDelta{Text: d.Delta}, true
		case ai.ThinkingDeltaEvent:
			return event.ThinkingDelta{Text: d.Delta}, true
		}
	case agent.MessageStartEvent:
		if ev.Message.User != nil {
			return event.UserMessage{Text: userText(ev.Message.User.Content)}, true
		}
	case agent.QueueUpdateEvent:
		return event.QueueChanged{Pending: ev.Steering}, true
	case agent.ToolExecutionStartEvent:
		return event.ToolStart{CallID: ev.ToolCallID, Name: ev.ToolName, Args: compactJSON(ev.Args)}, true
	case agent.ToolExecutionEndEvent:
		return event.ToolEnd{CallID: ev.ToolCallID, Name: ev.ToolName, Output: toolResultContentText(ev.Result.Content), IsError: ev.IsError}, true
	case agent.AutoRetryStartEvent:
		delay := time.Duration(ev.DelayMs) * time.Millisecond
		return event.Notice{Text: fmt.Sprintf("retrying (attempt %d/%d) in %s: %s", ev.Attempt, ev.MaxAttempts, delay, ev.ErrorMessage)}, true
	case agent.CompactionStartEvent:
		return event.Notice{Text: fmt.Sprintf("compacting context (%s)", ev.Reason)}, true
	case agent.CompactionEndEvent:
		switch {
		case ev.Aborted:
			return event.Notice{Text: "compaction aborted"}, true
		case ev.ErrorMessage != "":
			return event.Notice{Text: "compaction failed: " + ev.ErrorMessage}, true
		}
		return event.Notice{Text: "compaction done"}, true
	}
	return nil, false
}

// textJoiner puts a blank line before a text block that directly follows
// emitted text, as history does with afterText, so the reducer's
// concatenation draws separate paragraphs live and restored. Thinking does not
// separate text, nor does a queue update; a tool call, notice, user message or
// new run does. Tool and queue events can arrive from concurrent goroutines.
//
// It also drops each prompt's own user message, which TurnStart already
// carries, so only steered messages become UserMessage.
type textJoiner struct {
	mu sync.Mutex
	// sends is the session's prompt count; seen is its value when the last
	// prompt's user message was dropped.
	sends *atomic.Uint64
	seen  uint64
	// afterText: the last event emitted was text. newBlock: a text block has
	// started and none of its text has been emitted yet.
	afterText, newBlock bool
}

func (j *textJoiner) translate(ev agent.AgentEvent) (event.Event, bool) {
	e, ok := translate(ev)
	j.mu.Lock()
	defer j.mu.Unlock()
	switch ev := ev.(type) {
	case agent.AgentStartEvent:
		j.afterText = false
	case agent.MessageUpdateEvent:
		switch ev.AssistantMessageEvent.(type) {
		case ai.TextStartEvent:
			j.newBlock = true
		case ai.ToolCallStartEvent:
			j.afterText = false
		}
	}
	if _, user := e.(event.UserMessage); user {
		if n := j.sends.Load(); n != j.seen {
			j.seen = n
			e, ok = nil, false
		}
	}
	switch d := e.(type) {
	case event.TextDelta:
		// History skips empty text, so an empty delta neither takes nor
		// leaves the separator.
		if d.Text == "" {
			break
		}
		if j.newBlock && j.afterText {
			e = event.TextDelta{Text: "\n\n" + d.Text}
		}
		j.newBlock, j.afterText = false, true
	case event.ThinkingDelta, event.QueueChanged, nil:
	default:
		j.afterText = false
	}
	return e, ok
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func toolResultContentText(content []ai.ToolResultMessageContent) string {
	var parts []string
	for _, c := range content {
		if text, ok := c.(ai.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}
