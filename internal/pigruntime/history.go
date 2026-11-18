package pigruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"

	"github.com/keyute/kopi/internal/event"
)

// History replays the stored conversation as the events a live session
// would have delivered for it. Durations and work IDs are not stored, and a
// normally completed turn ends without a TurnEnd.
func (s *Session) History() []event.Event {
	return historyEntries(s.inner.SessionManager().GetBranch())
}

func historyEntries(entries []coding.SessionEntry) []event.Event {
	msgs, err := branchMessages(entries)
	out := history(msgs)
	if err != nil {
		out = append(out, event.Error{Err: err})
	}
	return out
}

// branchMessages decodes the messages and notices of entries that no context
// edit omitted. On a failed message decode it returns those before it; on a
// failed context edit decode, none.
func branchMessages(entries []coding.SessionEntry) ([]agent.AgentMessage, error) {
	omitted := make(map[string]bool)
	for _, entry := range entries {
		if entry.Base().Type != "context_edit" {
			continue
		}
		var edit struct {
			TargetID    string          `json:"targetId"`
			Replacement json.RawMessage `json:"replacement"`
		}
		err := json.Unmarshal(entry.Raw(), &edit)
		if err == nil && (edit.TargetID == "" || len(edit.Replacement) == 0) {
			err = fmt.Errorf("missing targetId or replacement")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode history context edit %s: %w", entry.Base().ID, err)
		}
		// Non-null replacements change model context, not display content.
		omitted[edit.TargetID] = strings.TrimSpace(string(edit.Replacement)) == "null"
	}

	var msgs []agent.AgentMessage
	for _, entry := range entries {
		if omitted[entry.Base().ID] {
			continue
		}
		if entry.Base().Type == "custom_message" {
			var record struct {
				CustomType string `json:"customType"`
			}
			if err := json.Unmarshal(entry.Raw(), &record); err != nil {
				return msgs, fmt.Errorf("failed to decode history custom message %s: %w", entry.Base().ID, err)
			}
			if record.CustomType != noticeType {
				continue
			}
			var notice struct {
				Details noticeDetails `json:"details"`
			}
			if err := json.Unmarshal(entry.Raw(), &notice); err != nil {
				return msgs, fmt.Errorf("failed to decode history notice %s: %w", entry.Base().ID, err)
			}
			// Only history reads the message, so it carries details decoded.
			msgs = append(msgs, agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom, "customType": noticeType, "details": notice.Details}})
			continue
		}
		if entry.Base().Type != "message" {
			continue
		}
		var record struct {
			Message agent.AgentMessage `json:"message"`
		}
		if err := json.Unmarshal(entry.Raw(), &record); err != nil {
			return msgs, fmt.Errorf("failed to decode history message %s: %w", entry.Base().ID, err)
		}
		msgs = append(msgs, record.Message)
	}
	return msgs, nil
}

// history skips system messages and custom messages other than notices; the
// skipped include compaction and branch summaries.
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
			var toolStarts []event.Event
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
					toolStarts = append(toolStarts, event.ToolStart{CallID: b.ID, Name: b.Name, Args: compactJSON(args)})
					afterText = false
				}
			}
			// PiG streams the entire assistant message before executing tools.
			if len(toolStarts) > 0 {
				out = append(out, toolStarts...)
				afterText = false
			}
			if end, ok := storedTurnEnd(m.Assistant); ok {
				out = append(out, end)
				afterText = false
			}
		case m.ToolResult != nil:
			r := m.ToolResult
			out = append(out, event.ToolEnd{CallID: r.ToolCallID, Name: r.ToolName, Output: toolResultContentText(r.Content), IsError: r.IsError})
			afterText = false
		case isNotice(m):
			d, _ := m.Custom["details"].(noticeDetails)
			out = append(out, event.TaskEnded{ID: d.ID, Command: d.Command, Status: d.Status, Elapsed: time.Duration(d.ElapsedMs) * time.Millisecond})
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
		return event.TurnEnd{Outcome: event.Outcome{Status: event.Failed, Message: pigTexts.Replace(m.ErrorMessage)}}, true
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
