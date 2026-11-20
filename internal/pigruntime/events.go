package pigruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/event"
)

// abortedCall is the text PiG's agent loop gives a tool call it abandoned before
// running it because the turn's context was already cancelled (tool_execution.go
// errorToolResult("Operation aborted")); PiG exports no constant for it.
const abortedCall = "Operation aborted"

// toolEnd builds a ToolEnd from a PiG result, live or stored. A call PiG
// aborted before it ran is marked interrupted, as a cancelled call is.
func toolEnd(callID, name string, content []ai.ToolResultMessageContent, isError bool) event.ToolEnd {
	end := event.ToolEnd{CallID: callID, Name: name, Output: toolResultContentText(content), IsError: isError}
	if isError && end.Output == abortedCall {
		end.Kind = event.EndInterrupted
	}
	return end
}

func translate(ev agent.AgentEvent) (event.Event, bool) {
	switch ev := ev.(type) {
	case agent.MessageUpdateEvent:
		switch d := ev.AssistantMessageEvent.(type) {
		case ai.TextDeltaEvent:
			return event.TextDelta{Text: d.Delta}, true
		case ai.ThinkingDeltaEvent:
			return event.ThinkingDelta{Text: d.Delta}, true
		}
	case agent.ToolExecutionStartEvent:
		return event.ToolStart{CallID: ev.ToolCallID, Name: ev.ToolName, Args: compactJSON(ev.Args)}, true
	case agent.ToolExecutionEndEvent:
		return toolEnd(ev.ToolCallID, ev.ToolName, ev.Result.Content, ev.IsError), true
	case agent.AutoRetryStartEvent:
		delay := time.Duration(ev.DelayMs) * time.Millisecond
		return event.Notice{Text: fmt.Sprintf("retrying (attempt %d/%d) in %s: %s", ev.Attempt, ev.MaxAttempts, delay, pigTexts.Replace(ev.ErrorMessage))}, true
	case agent.CompactionStartEvent:
		return event.Notice{Text: fmt.Sprintf("compacting context (%s)", ev.Reason)}, true
	case agent.CompactionEndEvent:
		switch {
		case ev.Aborted:
			return event.Notice{Text: "compaction aborted"}, true
		case ev.ErrorMessage != "":
			return event.Notice{Text: "compaction failed: " + pigTexts.Replace(ev.ErrorMessage)}, true
		}
		return event.Notice{Text: "compaction done"}, true
	}
	return nil, false
}

// textJoiner puts a blank line before a text block that directly follows
// emitted text, as history does with afterText, so the reducer's
// concatenation draws separate paragraphs live and restored. Thinking does not
// separate text; a tool call, notice, user message or new run does. Tool events
// can arrive from concurrent goroutines.
type textJoiner struct {
	mu sync.Mutex
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
	case agent.MessageStartEvent:
		if ev.Message.User != nil || isNotice(ev.Message) {
			j.afterText = false
		}
	case agent.MessageUpdateEvent:
		switch ev.AssistantMessageEvent.(type) {
		case ai.TextStartEvent:
			j.newBlock = true
		case ai.ToolCallStartEvent:
			j.afterText = false
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
	case event.ThinkingDelta, nil:
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
