package pigruntime

import (
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"

	"github.com/keyute/kopi/internal/event"
)

func TestTranslate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   agent.AgentEvent
		want event.Event
	}{
		{
			name: "text delta",
			in:   agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: "hi"}},
			want: event.TextDelta{Text: "hi"},
		},
		{
			name: "thinking delta",
			in:   agent.MessageUpdateEvent{AssistantMessageEvent: ai.ThinkingDeltaEvent{Delta: "hmm"}},
			want: event.ThinkingDelta{Text: "hmm"},
		},
		{
			name: "tool start compacts args",
			in:   agent.ToolExecutionStartEvent{ToolCallID: "c1", ToolName: "Read", Args: json.RawMessage("{ \"file_path\": \"a.go\" }")},
			want: event.ToolStart{CallID: "c1", Name: "Read", Args: `{"file_path":"a.go"}`},
		},
		{
			name: "tool end flattens text",
			in: agent.ToolExecutionEndEvent{
				ToolCallID: "c1", ToolName: "Read", IsError: true,
				Result: agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "a"}, ai.ImageContent{}, ai.TextContent{Text: "b"}}},
			},
			want: event.ToolEnd{CallID: "c1", Name: "Read", Output: "a\nb", IsError: true},
		},
		{
			name: "tool end aborted before it ran",
			in: agent.ToolExecutionEndEvent{
				ToolCallID: "c1", ToolName: "Read", IsError: true,
				Result: agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "Operation aborted"}}},
			},
			want: event.ToolEnd{CallID: "c1", Name: "Read", Output: "Operation aborted", IsError: true, Kind: event.EndInterrupted},
		},
		{
			name: "tool end aborted text without error",
			in: agent.ToolExecutionEndEvent{
				ToolCallID: "c1", ToolName: "Read",
				Result: agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "Operation aborted"}}},
			},
			want: event.ToolEnd{CallID: "c1", Name: "Read", Output: "Operation aborted"},
		},
		{
			name: "auto retry",
			in:   agent.AutoRetryStartEvent{Attempt: 2, MaxAttempts: 3, DelayMs: 1500, ErrorMessage: "overloaded"},
			want: event.Notice{Text: "Retrying (attempt 2/3) in 1.5s: overloaded"},
		},
		{
			name: "auto retry names kopi",
			in:   agent.AutoRetryStartEvent{Attempt: 1, MaxAttempts: 3, DelayMs: 1000, ErrorMessage: "not logged in (run `pig login github-copilot`)"},
			want: event.Notice{Text: "Retrying (attempt 1/3) in 1s: not logged in (run `kopi login github-copilot`)"},
		},
		{
			name: "Compaction start",
			in:   agent.CompactionStartEvent{Reason: "threshold"},
			want: event.Notice{Text: "Compacting context (threshold)"},
		},
		{name: "Compaction done", in: agent.CompactionEndEvent{}, want: event.Notice{Text: "Compaction done"}},
		{name: "Compaction aborted", in: agent.CompactionEndEvent{Aborted: true}, want: event.Notice{Text: "Compaction aborted"}},
		{
			name: "Compaction failed",
			in:   agent.CompactionEndEvent{ErrorMessage: "too big"},
			want: event.Notice{Text: "Compaction failed: too big"},
		},
		{
			name: "Compaction failed names kopi",
			in:   agent.CompactionEndEvent{ErrorMessage: "not logged in (run `pig login github-copilot`)"},
			want: event.Notice{Text: "Compaction failed: not logged in (run `kopi login github-copilot`)"},
		},
		{name: "assistant message end", in: agent.MessageEndEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{}}}, want: event.ReplyEnd{}},
		{name: "mixed text and tools", in: agent.MessageEndEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{Content: []ai.AssistantContentBlock{ai.TextContent{Text: "before"}, ai.ToolCall{}, ai.TextContent{}, ai.TextContent{Text: "after"}}}}}, want: event.ReplyEnd{Text: "before\n\nafter"}},
		{name: "error provisional", in: agent.MessageEndEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{StopReason: ai.StopReasonError}}}},
		{name: "length provisional", in: agent.MessageEndEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{StopReason: ai.StopReasonLength}}}},
		{name: "user message end dropped", in: agent.MessageEndEvent{Message: agent.AgentMessage{User: &agent.UserMessage{}}}},
		{name: "custom message end dropped", in: agent.MessageEndEvent{Message: agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom}}}},
		{name: "queue update dropped", in: agent.QueueUpdateEvent{Steering: []string{"a", "b"}, FollowUp: []string{"c"}}},
		{name: "user message start dropped", in: agent.MessageStartEvent{Message: agent.AgentMessage{User: &agent.UserMessage{Content: ai.UserContentBlocks{ai.TextContent{Text: "steer"}}}}}},
		{name: "assistant message start dropped", in: agent.MessageStartEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{}}}},
		{name: "message update without delta dropped", in: agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextStartEvent{}}},
		{name: "agent end dropped", in: agent.AgentEndEvent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translate(tc.in)
			assert.Equal(t, tc.want != nil, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTextJoinerSeparatesAdjacentTextBlocks(t *testing.T) {
	update := func(e ai.AssistantMessageEvent) agent.AgentEvent {
		return agent.MessageUpdateEvent{AssistantMessageEvent: e}
	}
	textStart := update(ai.TextStartEvent{})
	text := func(s string) agent.AgentEvent { return update(ai.TextDeltaEvent{Delta: s}) }
	for _, tc := range []struct {
		name string
		in   []agent.AgentEvent
		want []event.Event
	}{
		{
			name: "second block after thinking",
			in:   []agent.AgentEvent{textStart, text("one"), text(" more"), update(ai.ThinkingDeltaEvent{Delta: "hmm"}), textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: " more"}, event.ThinkingDelta{Text: "hmm"}, event.TextDelta{Text: "\n\ntwo"}},
		},
		{
			name: "reply end between",
			in:   []agent.AgentEvent{textStart, text("one"), agent.MessageEndEvent{Message: agent.AgentMessage{Assistant: &agent.AssistantMessage{}}}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.ReplyEnd{}, event.TextDelta{Text: "\n\ntwo"}},
		},
		{
			name: "queue update between",
			in:   []agent.AgentEvent{textStart, text("one"), agent.QueueUpdateEvent{}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "\n\ntwo"}},
		},
		{
			name: "tool call block between",
			in:   []agent.AgentEvent{textStart, text("one"), update(ai.ToolCallStartEvent{}), textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "two"}},
		},
		{
			name: "new run",
			in:   []agent.AgentEvent{textStart, text("one"), agent.AgentStartEvent{}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "two"}},
		},
		{
			name: "notice between",
			in:   []agent.AgentEvent{textStart, text("one"), agent.AutoRetryStartEvent{Attempt: 1, MaxAttempts: 2}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.Notice{Text: "Retrying (attempt 1/2) in 0s: "}, event.TextDelta{Text: "two"}},
		},
		{
			name: "kopi notice between",
			in:   []agent.AgentEvent{textStart, text("one"), agent.MessageStartEvent{Message: agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom, "customType": noticeType}}}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "two"}},
		},
		{
			name: "foreign custom message between",
			in:   []agent.AgentEvent{textStart, text("one"), agent.MessageStartEvent{Message: agent.AgentMessage{Custom: map[string]any{"role": agent.RoleCustom, "customType": "other"}}}, textStart, text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "\n\ntwo"}},
		},
		{
			name: "empty delta waits for text",
			in:   []agent.AgentEvent{textStart, text("one"), textStart, text(""), text("two")},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: ""}, event.TextDelta{Text: "\n\ntwo"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var j textJoiner
			var got []event.Event
			for _, ev := range tc.in {
				if e, ok := j.translate(ev); ok {
					got = append(got, e)
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Send emits UserMessage; the joiner only takes a user message as a separator.
func TestTextJoinerDropsUserMessageAsSeparator(t *testing.T) {
	textStart := agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextStartEvent{}}
	text := func(s string) agent.AgentEvent {
		return agent.MessageUpdateEvent{AssistantMessageEvent: ai.TextDeltaEvent{Delta: s}}
	}
	user := agent.MessageStartEvent{Message: agent.AgentMessage{User: &agent.UserMessage{Content: ai.UserContentBlocks{ai.TextContent{Text: "steered"}}}}}
	var j textJoiner
	var got []event.Event
	for _, ev := range []agent.AgentEvent{textStart, text("one"), user, textStart, text("two")} {
		if e, ok := j.translate(ev); ok {
			got = append(got, e)
		}
	}
	assert.Equal(t, []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "two"}}, got)
}

func TestTextJoinerGivesToolEndItsStartArgs(t *testing.T) {
	start := func(id string) agent.AgentEvent {
		return agent.ToolExecutionStartEvent{ToolCallID: id, ToolName: "Read", Args: json.RawMessage(`{ "file_path": "a.go" }`)}
	}
	end := func(id string) agent.AgentEvent { return agent.ToolExecutionEndEvent{ToolCallID: id, ToolName: "Read"} }
	var j textJoiner
	var got []event.Event
	for _, ev := range []agent.AgentEvent{start("c1"), start("c2"), end("c2"), end("c1"), end("c3")} {
		if e, ok := j.translate(ev); ok {
			got = append(got, e)
		}
	}
	assert.Equal(t, []event.Event{
		event.ToolStart{CallID: "c1", Name: "Read", Args: `{"file_path":"a.go"}`},
		event.ToolStart{CallID: "c2", Name: "Read", Args: `{"file_path":"a.go"}`},
		event.ToolEnd{CallID: "c2", Name: "Read", Args: `{"file_path":"a.go"}`},
		event.ToolEnd{CallID: "c1", Name: "Read", Args: `{"file_path":"a.go"}`},
		event.ToolEnd{CallID: "c3", Name: "Read"},
	}, got)
}
