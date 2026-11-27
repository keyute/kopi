package pigruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// Saved is a session's transcript file. It outlives the session, which holds
// no file once closed.
type Saved struct{ path string }

// Saved returns the handle of the session's transcript file.
func (s *Session) Saved() Saved { return Saved{path: s.inner.Path()} }

// SavedHistory replays the transcript sv names as History does for a live
// session. A transcript the session never wrote wraps ErrNotSaved.
func (r *Runtime) SavedHistory(sv Saved) ([]event.Event, error) {
	// PiG creates an empty log when it opens a path that does not exist, so
	// the file is checked first. The log PiG loads also holds a snapshot file
	// beside the transcript until it is garbage-collected, and PiG has no public
	// way to close it.
	if _, err := os.Stat(sv.path); err != nil {
		return nil, fmt.Errorf("failed to read saved transcript: %w: %w", ErrNotSaved, err)
	}
	manager, err := coding.SessionManagerFor(r.svcs, coding.SessionStartOptions{ResumePath: sv.path})
	if err != nil {
		return nil, fmt.Errorf("failed to read saved transcript: %w", reword(err))
	}
	return historyEntries(manager.GetBranch()), nil
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
			// Only history reads the message, so it carries details decoded.
			switch record.CustomType {
			case noticeType:
				details, err := storedDetails[noticeDetails](entry)
				if err != nil {
					return msgs, fmt.Errorf("failed to decode history notice %s: %w", entry.Base().ID, err)
				}
				msgs = append(msgs, agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom, "customType": noticeType, "details": details}})
			case commandType:
				details, err := storedDetails[commandDetails](entry)
				if err != nil {
					return msgs, fmt.Errorf("failed to decode history command %s: %w", entry.Base().ID, err)
				}
				msgs = append(msgs, agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom, "customType": commandType, "details": details}})
			}
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

func branchCopyTarget(entries []coding.SessionEntry, boundary *string) (string, error) {
	if boundary != nil {
		// During Send, provisional messages after its starting leaf have not settled.
		filtered := make([]coding.SessionEntry, 0, len(entries))
		active := *boundary == ""
		for _, entry := range entries {
			if active && entry.Base().Type == "message" {
				var record struct {
					Message agent.AgentMessage `json:"message"`
				}
				if err := json.Unmarshal(entry.Raw(), &record); err != nil {
					return "", fmt.Errorf("failed to decode history message %s: %w", entry.Base().ID, err)
				}
				if a := record.Message.Assistant; a != nil && provisionalReply(a) {
					continue
				}
			}
			filtered = append(filtered, entry)
			if entry.Base().ID == *boundary {
				active = true
			}
		}
		entries = filtered
	}
	msgs, err := branchMessages(entries)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if a := msgs[i].Assistant; a != nil {
			if text := assistantText(a); strings.TrimSpace(text) != "" {
				return text, nil
			}
		}
	}
	return "", nil
}

func storedDetails[T any](entry coding.SessionEntry) (T, error) {
	var stored struct {
		Details T `json:"details"`
	}
	err := json.Unmarshal(entry.Raw(), &stored)
	return stored.Details, err
}

// history skips system messages and custom messages other than notices and user commands; the
// skipped include compaction and branch summaries.
func history(msgs []agent.AgentMessage) []event.Event {
	var out []event.Event
	// afterText reports that the last event emitted was a TextDelta, so the
	// next text needs a separating blank line once the reducer concatenates;
	// textJoiner applies the same rule live.
	afterText := false
	callArgs := map[string]string{}
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
					compact := compactJSON(args)
					callArgs[b.ID] = compact
					toolStarts = append(toolStarts, event.ToolStart{CallID: b.ID, Name: b.Name, Args: compact})
					afterText = false
				}
			}
			out = append(out, event.ReplyEnd{Text: assistantText(m.Assistant)})
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
			end := toolEnd(r.ToolCallID, r.ToolName, r.Content, r.IsError)
			end.Args = callArgs[r.ToolCallID]
			out = append(out, end)
			afterText = false
		case isNotice(m):
			switch d := m.Custom["details"].(type) {
			case commandDetails:
				out = append(out, event.UserCommand{
					Command: d.Command, State: event.CommandState(d.State), Elapsed: time.Duration(d.ElapsedMs) * time.Millisecond,
					Exit: d.Exit, Failed: d.Failed, Output: d.Output, TaskID: d.Task,
				})
			case noticeDetails:
				out = append(out, event.TaskEnded{
					ID: d.ID, Command: d.Command, Agent: d.Agent, Status: d.Status, Elapsed: time.Duration(d.ElapsedMs) * time.Millisecond,
					Agents: d.Agents, Failed: d.Failed, Reason: d.Reason, Result: d.Result,
				})
			}
			afterText = false
		}
	}
	return out
}

// storedTurnEnd mirrors Send's outcome for a stored message. A transcript
// written before PiG v0.8.0 holds an abort during a tool as StopReason "error"
// with "context canceled", not "aborted".
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
