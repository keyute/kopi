package pigruntime

import (
	"context"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/keyute/kopi/internal/event"
)

// History replays the stored conversation as the events a live session
// would have delivered for it. Durations and work IDs are not stored, and a
// normally completed turn ends without a TurnEnd.
func (s *Session) History() []event.Event {
	return history(s.inner.Messages())
}

// history skips system and custom messages, which include compaction and
// branch summaries.
func history(msgs []agent.AgentMessage) []event.Event {
	var out []event.Event
	// afterText reports that the last event emitted was a TextDelta, so the
	// next text needs a separating blank line once the reducer concatenates;
	// textJoiner applies the same rule live.
	afterText := false
	for _, m := range msgs {
		switch {
		case m.User != nil:
			out = append(out, event.TurnStart{Prompt: userText(m.User.Content)})
			afterText = false
		case m.Assistant != nil:
			for _, block := range m.Assistant.Content {
				switch b := block.(type) {
				case ai.TextContent:
					if b.Text == "" {
						continue
					}
					text := b.Text
					if afterText {
						text = "\n\n" + text
					}
					out = append(out, event.TextDelta{Text: text})
					afterText = true
				case ai.ToolCall:
					args, _ := b.ArgumentsJSON()
					out = append(out, event.ToolStart{CallID: b.ID, Name: b.Name, Args: compactJSON(args)})
					afterText = false
				}
			}
			if end, ok := storedTurnEnd(m.Assistant); ok {
				out = append(out, end)
				afterText = false
			}
		case m.ToolResult != nil:
			r := m.ToolResult
			out = append(out, event.ToolEnd{CallID: r.ToolCallID, Name: r.ToolName, Output: toolResultContentText(r.Content), IsError: r.IsError})
			afterText = false
		}
	}
	return out
}

// storedTurnEnd mirrors Send's outcome for a stored message. PiG stores an
// abort during a tool as StopReason "error" with "context canceled", which a
// live turn reports as Interrupted.
func storedTurnEnd(m *agent.AssistantMessage) (event.TurnEnd, bool) {
	switch {
	case m.StopReason == ai.StopReasonAborted,
		m.StopReason == ai.StopReasonError && m.ErrorMessage == context.Canceled.Error():
		return event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}}, true
	case m.StopReason == ai.StopReasonError:
		return event.TurnEnd{Outcome: event.Outcome{Status: event.Failed, Message: m.ErrorMessage}}, true
	}
	return event.TurnEnd{}, false
}

func userText(content ai.UserContent) string {
	switch c := content.(type) {
	case ai.UserText:
		return string(c)
	case ai.UserContentBlocks:
		var parts []string
		for _, b := range c {
			if text, ok := b.(ai.TextContent); ok {
				parts = append(parts, text.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
