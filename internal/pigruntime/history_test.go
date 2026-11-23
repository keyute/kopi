package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// unstreamed merges consecutive TextDeltas and drops ThinkingDeltas, which
// history does not replay.
func unstreamed(evs []event.Event) []event.Event {
	var out []event.Event
	for _, ev := range evs {
		switch ev := ev.(type) {
		case event.ThinkingDelta:
			continue
		case event.TextDelta:
			if n := len(out); n > 0 {
				if prev, ok := out[n-1].(event.TextDelta); ok {
					out[n-1] = event.TextDelta{Text: prev.Text + ev.Text}
					continue
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

func resume(t *testing.T, r *Runtime, id string, tools ...tool.Tool) *Session {
	t.Helper()
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: id, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	return sess
}

func TestHistoryReplaysStoredTurnAsLiveEvents(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(ai.FauxContentBlocks{
			ai.FauxThinking("considering"),
			ai.FauxText("let me look"),
			ai.FauxThinking("reconsidering"),
			ai.FauxText("and once more"),
			ai.FauxToolCall("Probe", map[string]any{"path": "a b", "n": 2}, &ai.FauxToolCallOptions{ID: "call-1"}),
		}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)}),
		ai.FauxAssistantMessage(ai.FauxContentBlocks{
			ai.FauxToolCall("Fail", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-2"}),
		}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)}),
		reply("all done"),
	})
	tools := []tool.Tool{
		probeTool(func(context.Context, json.RawMessage) (string, error) { return toolOutput, nil }),
		{
			Name: "Fail", Label: "Fail", Description: "Always fails.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
			Capability: policy.Read,
			Run: func(_ context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
				if _, err := admit(policy.Target{}); err != nil {
					return tool.Result{}, err
				}
				return tool.Result{}, errors.New("probe failed")
			},
		},
	}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	assert.Empty(t, sess.History())

	live := []event.Event{event.TurnStart{Prompt: "probe please"}}
	unsubscribe := sess.Subscribe(func(ev event.Event) { live = append(live, ev) })
	res, err := sess.Send(t.Context(), Prompt{Text: "probe please"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	unsubscribe()
	require.NoError(t, sess.Close(t.Context()))

	got := resume(t, r, sess.ID(), tools...).History()
	assert.Equal(t, unstreamed(live), unstreamed(got))
	ends := []event.ToolEnd{}
	for _, ev := range got {
		if end, ok := ev.(event.ToolEnd); ok {
			ends = append(ends, end)
		}
	}
	require.Len(t, ends, 2)
	assert.False(t, ends[0].IsError)
	assert.True(t, ends[1].IsError)
	assert.Equal(t, event.TextDelta{Text: "let me look\n\nand once more"}, unstreamed(got)[1])
	assert.Contains(t, got, event.Event(event.ToolStart{CallID: "call-1", Name: "Probe", Args: `{"n":2,"path":"a b"}`}))
}

func TestHistoryReplaysTextAroundToolCallsAsLiveEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks ai.FauxContentBlocks
	}{
		{
			name: "tool before text",
			blocks: ai.FauxContentBlocks{
				ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"}),
				ai.FauxText("after"),
			},
		},
		{
			name: "tool between text",
			blocks: ai.FauxContentBlocks{
				ai.FauxText("before"),
				ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"}),
				ai.FauxText("after"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			faux.SetResponses([]ai.FauxResponseStep{
				ai.FauxAssistantMessage(tc.blocks, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)}),
				reply("done"),
			})
			probe := probeTool(func(context.Context, json.RawMessage) (string, error) { return toolOutput, nil })
			sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{probe}, Authority: policy.NewAuthority()})
			require.NoError(t, err)
			live := []event.Event{event.TurnStart{Prompt: "go"}}
			unsubscribe := sess.Subscribe(func(ev event.Event) { live = append(live, ev) })
			res, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
			require.NoError(t, err)
			require.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
			unsubscribe()
			require.NoError(t, sess.Close(t.Context()))

			assert.Equal(t, unstreamed(live), unstreamed(resume(t, r, sess.ID(), probe).History()))
		})
	}
}

func TestHistoryRestoresNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("noted"), captureRequest(requests, "again")})
	probe := probeTool(okProbe)
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: []tool.Tool{probe}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	n := testNotice("t1")
	more, _ := handOnce(nil, []Notice{n})

	live := []event.Event{event.TurnStart{Prompt: "go"}}
	unsubscribe := sess.Subscribe(func(ev event.Event) { live = append(live, ev) })
	res, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, more)
	require.NoError(t, err)
	require.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	unsubscribe()
	require.NoError(t, sess.Close(t.Context()))

	resumed := resume(t, r, sess.ID(), probe)
	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "go"},
		event.ToolStart{CallID: "call-1", Name: "Probe", Args: "{}"},
		event.ToolEnd{CallID: "call-1", Name: "Probe", Args: "{}", Output: toolOutput},
		n.Shown,
		event.TextDelta{Text: "noted"},
	}, resumed.History())
	assert.Equal(t, unstreamed(live), unstreamed(resumed.History()))

	_, err = resumed.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"go", n.Text, "next"}, userTexts(<-requests))
}

func TestHistoryRestoresDeliveredNotices(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("done"), reply("noted")})
	sess := openTestSession(t, r)
	n1, n2, n3 := testNotice("t1"), testNotice("t2"), testNotice("t3")

	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, []Notice{n1}, nil)
	require.NoError(t, err)
	_, err = sess.Send(t.Context(), Prompt{}, []Notice{n2, n3}, nil)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "go"},
		n1.Shown,
		event.TextDelta{Text: "done"},
		n2.Shown,
		n3.Shown,
		event.TextDelta{Text: "noted"},
	}, resume(t, r, sess.ID()).History())
}

func TestHistorySkipsForeignCustomMessages(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	appendHistoryMessage(t, manager, `{"role":"user","content":"go","timestamp":1}`)
	_, err := manager.AppendCustomMessageEntry("other.extension", "not for kopi", true, map[string]any{"id": "t9"})
	require.NoError(t, err)
	appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop","timestamp":2}`)
	require.NoError(t, sess.Close(t.Context()))

	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "go"},
		event.TextDelta{Text: "done"},
	}, resume(t, r, sess.ID()).History())
}

func TestHistoryEndsFailedAndInterruptedTurns(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{
			ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}),
		})
		sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		_, err = sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
		require.NoError(t, err)
		require.NoError(t, sess.Close(t.Context()))

		assert.Equal(t, []event.Event{
			event.TurnStart{Prompt: "go"},
			event.TurnEnd{Outcome: event.Outcome{Status: event.Failed, Message: "invalid request: bad thing"}},
		}, resume(t, r, sess.ID()).History())
	})

	t.Run("failed with PiG's text", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{
			ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "not logged in (run `pig login github-copilot`)"}),
		})
		sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		res, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
		require.NoError(t, err)
		require.NoError(t, sess.Close(t.Context()))

		want := event.Outcome{Status: event.Failed, Message: "not logged in (run `kopi login github-copilot`)"}
		assert.Equal(t, want, res.Outcome)
		assert.Equal(t, []event.Event{event.TurnStart{Prompt: "go"}, event.TurnEnd{Outcome: want}}, resume(t, r, sess.ID()).History())
	})

	t.Run("interrupted during a tool", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("should not be requested")})
		started := make(chan struct{})
		block := probeTool(func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
		sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{block}, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		turnCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = sess.Send(turnCtx, Prompt{Text: "block"}, nil, nil)
		}()
		<-started
		cancel()
		<-done
		require.NoError(t, sess.Close(t.Context()))

		assert.Equal(t, []event.Event{
			event.TurnStart{Prompt: "block"},
			event.ToolStart{CallID: "call-1", Name: "Probe", Args: "{}"},
			event.ToolEnd{CallID: "call-1", Name: "Probe", Args: "{}", Output: context.Canceled.Error(), IsError: true},
			event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}},
		}, resume(t, r, sess.ID(), block).History())
	})
}

func TestHistoryRetainsCompactedConversationOnActiveBranch(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	appendMessage := func(raw string) string {
		t.Helper()
		var msg agent.AgentMessage
		require.NoError(t, json.Unmarshal([]byte(raw), &msg))
		id, err := manager.AppendMessage(msg)
		require.NoError(t, err)
		return id
	}
	appendMessage(`{"role":"user","content":"old prompt","timestamp":1}`)
	appendMessage(`{"role":"assistant","content":[{"type":"text","text":"old answer"},{"type":"toolCall","id":"old-call","name":"Probe","arguments":{}}],"stopReason":"toolUse","timestamp":2}`)
	appendMessage(`{"role":"toolResult","toolCallId":"old-call","toolName":"Probe","content":[{"type":"text","text":"old output"}],"isError":false,"timestamp":3}`)
	forkPoint := appendMessage(`{"role":"assistant","content":[{"type":"text","text":"old conclusion"}],"stopReason":"stop","timestamp":4}`)
	appendMessage(`{"role":"user","content":"sibling prompt","timestamp":5}`)
	appendMessage(`{"role":"assistant","content":[{"type":"text","text":"sibling answer"}],"stopReason":"stop","timestamp":6}`)
	require.NoError(t, sess.inner.Fork(forkPoint))
	_, err := manager.AppendCompaction("summary, not conversation", "", 1000, nil, false, nil)
	require.NoError(t, err)
	appendMessage(`{"role":"user","content":"new prompt","timestamp":7}`)
	appendMessage(`{"role":"assistant","content":[{"type":"text","text":"new answer"}],"stopReason":"stop","timestamp":8}`)
	require.NoError(t, sess.Close(t.Context()))

	resumed := resume(t, r, sess.ID())
	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "old prompt"},
		event.TextDelta{Text: "old answer"},
		event.ToolStart{CallID: "old-call", Name: "Probe", Args: "{}"},
		event.ToolEnd{CallID: "old-call", Name: "Probe", Args: "{}", Output: "old output"},
		event.TextDelta{Text: "old conclusion"},
		event.TurnStart{Prompt: "new prompt"},
		event.TextDelta{Text: "new answer"},
	}, resumed.History())
	assert.NotContains(t, history(resumed.inner.Messages()), event.Event(event.TurnStart{Prompt: "old prompt"}))
}

func appendHistoryMessage(t *testing.T, manager *coding.SessionManager, raw string) string {
	t.Helper()
	var msg agent.AgentMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &msg))
	id, err := manager.AppendMessage(msg)
	require.NoError(t, err)
	return id
}

func TestHistoryOmitsFailedRetryAttempt(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	appendHistoryMessage(t, manager, `{"role":"user","content":"earlier prompt","timestamp":1}`)
	appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"earlier answer"}],"stopReason":"stop","timestamp":2}`)
	appendHistoryMessage(t, manager, `{"role":"user","content":"try again","timestamp":3}`)
	appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"Probe","arguments":{}}],"stopReason":"toolUse","timestamp":4}`)
	appendHistoryMessage(t, manager, `{"role":"toolResult","toolCallId":"call-1","toolName":"Probe","content":[{"type":"text","text":"kept output"}],"isError":false,"timestamp":5}`)
	failed := appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"failed partial"}],"stopReason":"error","errorMessage":"overloaded","timestamp":6}`)
	_, err := manager.AppendContextEdit(failed, nil)
	require.NoError(t, err)
	appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"recovered"}],"stopReason":"stop","timestamp":7}`)
	require.NoError(t, sess.Close(t.Context()))

	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "earlier prompt"},
		event.TextDelta{Text: "earlier answer"},
		event.TurnStart{Prompt: "try again"},
		event.ToolStart{CallID: "call-1", Name: "Probe", Args: "{}"},
		event.ToolEnd{CallID: "call-1", Name: "Probe", Args: "{}", Output: "kept output"},
		event.TextDelta{Text: "recovered"},
	}, resume(t, r, sess.ID()).History())
}

func TestHistoryLatestContextEditWins(t *testing.T) {
	replacement := &extension.ContextEditReplacement{Content: json.RawMessage(`[{"type":"text","text":"model-only replacement"}]`)}
	for _, tc := range []struct {
		name  string
		edits []*extension.ContextEditReplacement
		want  []event.Event
	}{
		{
			name:  "non-null edit restores original display content",
			edits: []*extension.ContextEditReplacement{nil, replacement},
			want:  []event.Event{event.TurnStart{Prompt: "go"}, event.TextDelta{Text: "original"}},
		},
		{
			name:  "null edit omits replaced message",
			edits: []*extension.ContextEditReplacement{replacement, nil},
			want:  []event.Event{event.TurnStart{Prompt: "go"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, _ := newTestRuntime(t, sb)
			sess := openTestSession(t, r)
			manager := sess.inner.SessionManager()
			appendHistoryMessage(t, manager, `{"role":"user","content":"go","timestamp":1}`)
			target := appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"original"}],"stopReason":"stop","timestamp":2}`)
			for _, edit := range tc.edits {
				_, err := manager.AppendContextEdit(target, edit)
				require.NoError(t, err)
			}
			require.NoError(t, sess.Close(t.Context()))
			assert.Equal(t, tc.want, resume(t, r, sess.ID()).History())
		})
	}
}

func TestHistoryIgnoresSiblingContextEdits(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	appendHistoryMessage(t, manager, `{"role":"user","content":"go","timestamp":1}`)
	target := appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"text","text":"shared answer"}],"stopReason":"stop","timestamp":2}`)
	_, err := manager.AppendContextEdit(target, nil)
	require.NoError(t, err)
	require.NoError(t, sess.inner.Fork(target))
	appendHistoryMessage(t, manager, `{"role":"user","content":"active prompt","timestamp":3}`)
	require.NoError(t, sess.Close(t.Context()))

	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "go"},
		event.TextDelta{Text: "shared answer"},
		event.TurnStart{Prompt: "active prompt"},
	}, resume(t, r, sess.ID()).History())
}

type corruptHistoryEntry struct {
	coding.SessionEntry
	raw json.RawMessage
}

func (e corruptHistoryEntry) Raw() json.RawMessage { return e.raw }

func TestHistoryStopsOnDecodeError(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	for _, prompt := range []string{"before", "corrupt", "after"} {
		_, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: agent.RoleUser, Content: ai.UserText(prompt)}})
		require.NoError(t, err)
	}
	entries := manager.GetBranch()
	var messages []coding.SessionEntry
	for _, entry := range entries {
		if entry.Base().Type == "message" {
			messages = append(messages, entry)
		}
	}
	require.Len(t, messages, 3)
	for _, raw := range []string{`{"message":`, `{"message":{"role":"user","content":"unterminated}}`} {
		t.Run(raw, func(t *testing.T) {
			got := historyEntries([]coding.SessionEntry{messages[0], corruptHistoryEntry{SessionEntry: messages[1], raw: json.RawMessage(raw)}, messages[2]})
			require.Len(t, got, 2)
			assert.Equal(t, event.TurnStart{Prompt: "before"}, got[0])
			failure, ok := got[1].(event.Error)
			require.True(t, ok)
			require.ErrorContains(t, failure.Err, "failed to decode history message "+messages[1].Base().ID)
			assert.Error(t, errors.Unwrap(failure.Err))
		})
	}
}

func TestHistoryRejectsMalformedContextEdits(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	target := appendHistoryMessage(t, manager, `{"role":"user","content":"kept","timestamp":1}`)
	editID, err := manager.AppendContextEdit(target, nil)
	require.NoError(t, err)
	entry, ok := manager.GetEntry(editID)
	require.True(t, ok)
	for _, raw := range []string{
		`{"targetId":`,
		`{"targetId":"` + target + `"}`,
		`{"replacement":null}`,
		`{"targetId":42,"replacement":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			got := historyEntries([]coding.SessionEntry{corruptHistoryEntry{SessionEntry: entry, raw: json.RawMessage(raw)}})
			require.Len(t, got, 1)
			failure, ok := got[0].(event.Error)
			require.True(t, ok)
			require.ErrorContains(t, failure.Err, "failed to decode history context edit "+editID)
			assert.Error(t, errors.Unwrap(failure.Err))
		})
	}
}

// The faux provider cannot store a message with StopReason "aborted", user
// content blocks, or custom entries, so those shapes are mapped directly.
func TestHistoryMapsStoredShapes(t *testing.T) {
	assistant := func(stop ai.StopReason, errMsg string, blocks ...ai.AssistantContentBlock) agent.AgentMessage {
		return agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: agent.RoleAssistant, Content: blocks, StopReason: stop, ErrorMessage: errMsg}}
	}
	text := func(s string) ai.AssistantContentBlock { return ai.TextContent{Text: s} }
	for _, tc := range []struct {
		name string
		msgs []agent.AgentMessage
		want []event.Event
	}{
		{
			name: "user content blocks join their text and skip images",
			msgs: []agent.AgentMessage{{User: &agent.UserMessage{Role: agent.RoleUser, Content: ai.UserContentBlocks{
				ai.TextContent{Text: "look"}, ai.ImageContent{Data: "AA==", MimeType: "image/png"}, ai.TextContent{Text: "here"},
			}}}},
			want: []event.Event{event.TurnStart{Prompt: "look\nhere"}},
		},
		{
			name: "system and custom entries are skipped",
			msgs: []agent.AgentMessage{
				{System: &ai.SystemMessage{}},
				{Custom: map[string]any{"role": agent.RoleCompactionSummary, "summary": "earlier"}},
				{Custom: map[string]any{"role": agent.RoleBranchSummary, "summary": "elsewhere"}},
				assistant(ai.StopReasonStop, "", text("kept")),
			},
			want: []event.Event{event.TextDelta{Text: "kept"}},
		},
		{
			name: "adjacent text is separated by a blank line",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonStop, "", text("one"), ai.ThinkingContent{Thinking: "hmm"}, text("two")),
				assistant(ai.StopReasonStop, "", text("three")),
			},
			want: []event.Event{event.TextDelta{Text: "one"}, event.TextDelta{Text: "\n\ntwo"}, event.TextDelta{Text: "\n\nthree"}},
		},
		{
			name: "text after a tool call is not separated",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonToolUse, "", text("before"), ai.ToolCall{ID: "c", Name: "Probe"}),
				assistant(ai.StopReasonStop, "", text("after")),
			},
			want: []event.Event{
				event.TextDelta{Text: "before"},
				event.ToolStart{CallID: "c", Name: "Probe", Args: "{}"},
				event.TextDelta{Text: "after"},
			},
		},
		{
			name: "tool before text executes after text",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonToolUse, "", ai.ToolCall{ID: "c", Name: "Probe"}, text("after")),
			},
			want: []event.Event{
				event.TextDelta{Text: "after"},
				event.ToolStart{CallID: "c", Name: "Probe", Args: "{}"},
			},
		},
		{
			name: "tool between text resets separator before execution",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonToolUse, "", text("before"), ai.ToolCall{ID: "c", Name: "Probe"}, text("after")),
				assistant(ai.StopReasonStop, "", text("next message")),
			},
			want: []event.Event{
				event.TextDelta{Text: "before"},
				event.TextDelta{Text: "after"},
				event.ToolStart{CallID: "c", Name: "Probe", Args: "{}"},
				event.TextDelta{Text: "next message"},
			},
		},
		{
			name: "multiple tools retain order with empty text and failure",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonError, "failed", text("before"), ai.ToolCall{ID: "c1", Name: "Probe"}, text(""), ai.ToolCall{ID: "c2", Name: "Probe"}, text("after")),
				assistant(ai.StopReasonStop, "", text("next message")),
			},
			want: []event.Event{
				event.TextDelta{Text: "before"},
				event.TextDelta{Text: "after"},
				event.ToolStart{CallID: "c1", Name: "Probe", Args: "{}"},
				event.ToolStart{CallID: "c2", Name: "Probe", Args: "{}"},
				event.TurnEnd{Outcome: event.Outcome{Status: event.Failed, Message: "failed"}},
				event.TextDelta{Text: "next message"},
			},
		},
		{
			name: "notice separates text",
			msgs: []agent.AgentMessage{
				assistant(ai.StopReasonStop, "", text("before")),
				{Custom: map[string]any{"role": agent.RoleCustom, "customType": noticeType, "details": noticeDetails{ID: "t1", Command: "make", Status: "failed", ElapsedMs: 2500}}},
				assistant(ai.StopReasonStop, "", text("after")),
			},
			want: []event.Event{
				event.TextDelta{Text: "before"},
				event.TaskEnded{ID: "t1", Command: "make", Status: "failed", Elapsed: 2500 * time.Millisecond},
				event.TextDelta{Text: "after"},
			},
		},
		{
			name: "a call PiG aborted before it ran is interrupted, other errors are not",
			msgs: []agent.AgentMessage{
				{ToolResult: &agent.ToolResultMessage{ToolCallID: "c1", ToolName: "Probe", IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "Operation aborted"}}}},
				{ToolResult: &agent.ToolResultMessage{ToolCallID: "c2", ToolName: "Probe", IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "Operation aborted by user"}}}},
			},
			want: []event.Event{
				event.ToolEnd{CallID: "c1", Name: "Probe", Output: "Operation aborted", IsError: true, Kind: event.EndInterrupted},
				event.ToolEnd{CallID: "c2", Name: "Probe", Output: "Operation aborted by user", IsError: true},
			},
		},
		{
			name: "aborted",
			msgs: []agent.AgentMessage{assistant(ai.StopReasonAborted, "", text("partial"))},
			want: []event.Event{event.TextDelta{Text: "partial"}, event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, history(tc.msgs))
		})
	}
}

// The faux model declares a 128000-token window, and PiG projects usage
// from the stored transcript even before the first turn.
func TestContextUsageMirrorsPiG(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("counted")})
	sess := openTestSession(t, r)

	mirrors := func() int {
		t.Helper()
		tokens, window, ok := sess.ContextUsage()
		want := sess.inner.ContextUsage()
		require.NotNil(t, want)
		require.NotNil(t, want.Tokens)
		assert.True(t, ok)
		assert.Equal(t, *want.Tokens, tokens)
		assert.Equal(t, want.ContextWindow, window)
		assert.Equal(t, 128000, window)
		return tokens
	}
	before := mirrors()
	_, err := sess.Send(t.Context(), Prompt{Text: "count me"}, nil, nil)
	require.NoError(t, err)
	assert.Greater(t, mirrors(), before)
}

func TestEffortNamesTheLevelOnlyForAReasoningModel(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	assert.Empty(t, openTestSession(t, r).Effort(), "faux-1 does not reason")

	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	sess, err := r.Open(t.Context(), OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	assert.Equal(t, "low", sess.Effort())
}

func TestSetEffortChangesTheLevelForTheSessionAndItsResume(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	assert.Nil(t, openTestSession(t, r).Efforts(), "faux-1 does not reason")

	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	opts := OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()}
	sess, err := r.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	require.NoError(t, sess.SetEffort(HighEffort))
	assert.Equal(t, "high", sess.Effort())
	assert.Contains(t, sess.Efforts(), "high")
	for _, name := range sess.Efforts() {
		_, err := ParseEffort(name)
		assert.NoError(t, err, name)
	}

	thinker.SetResponses([]ai.FauxResponseStep{reply("noted")})
	_, err = sess.Send(t.Context(), Prompt{Text: "hello"}, nil, nil)
	require.NoError(t, err)
	opts.Effort, opts.ResumeID = DefaultEffort, sess.ID()
	resumed, err := r.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, resumed.Close(context.Background())) })
	assert.Equal(t, "high", resumed.Effort())
}

func TestEffortTreatsAMaxThinkingModelAsReasoning(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker"}}})
	thinker.Models()[0].Capabilities.MaxThinking = ai.ThinkingLevelHigh
	require.False(t, thinker.Models()[0].ProviderMeta.Reasoning)
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	sess, err := r.Open(t.Context(), OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	assert.Equal(t, "low", sess.Effort())
	assert.Contains(t, sess.Efforts(), "high")
}

func TestUsageMirrorsPiGsSessionStats(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	thinker.SetResponses([]ai.FauxResponseStep{callTool("Probe", "call-1"), reply("counted")})
	probe := probeTool(func(context.Context, json.RawMessage) (string, error) { return "probed", nil })
	sess, err := r.Open(t.Context(), OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Tools: []tool.Tool{probe}, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	_, err = sess.Send(t.Context(), Prompt{Text: "count me"}, nil, nil)
	require.NoError(t, err)

	toolCalls, cost := sess.Usage()
	want := sess.inner.GetSessionStats()
	assert.Equal(t, want.ToolCalls, toolCalls)
	assert.Equal(t, want.Cost, cost)
	assert.Equal(t, 1, toolCalls)
	assert.Equal(t, "low", sess.Effort())
}

func TestHistoryAndSessionListOmitSkills(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("done"), captureRequest(requests, "again")})
	sess := openTestSession(t, r)

	_, err := sess.Send(t.Context(), Prompt{Text: "go", Skills: []string{"SKILL-TEXT"}}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	resumed := resume(t, r, sess.ID())
	assert.Equal(t, []event.Event{
		event.TurnStart{Prompt: "go"},
		event.TextDelta{Text: "done"},
	}, resumed.History())
	infos, err := r.Sessions()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, "go", infos[0].FirstMessage)

	_, err = resumed.Send(t.Context(), Prompt{Text: "next"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, countOf(userTexts(<-requests), "SKILL-TEXT"))
}

func TestHistoryRestoresNoticeAgent(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("noted")})
	sess := openTestSession(t, r)
	n := testNotice("t1")
	n.Shown.Agent = "explore"
	more, _ := handOnce(nil, []Notice{n})
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, more)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	assert.Contains(t, unstreamed(resume(t, r, sess.ID()).History()), event.Event(n.Shown))
}

func TestHistoryNoticeWithoutAgentKeyRestoresEmpty(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("noted")})
	sess := openTestSession(t, r)
	n := testNotice("t1")
	more, _ := handOnce(nil, []Notice{n})
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, more)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	raw, err := os.ReadFile(sess.inner.Path())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"agent"`)
	got := unstreamed(resume(t, r, sess.ID()).History())
	assert.Contains(t, got, event.Event(event.TaskEnded{ID: "t1", Command: n.Shown.Command, Status: n.Shown.Status, Elapsed: n.Shown.Elapsed}))
}

func TestHistoryRestoresWorkflowNoticeFields(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("noted")})
	sess := openTestSession(t, r)
	n := testNotice("t1")
	n.Shown.Agent = "workflow"
	n.Shown.Agents, n.Shown.Failed, n.Shown.Reason, n.Shown.Result = 9, 2, "boom", `{"ok":true}`
	more, _ := handOnce(nil, []Notice{n})
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, more)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))

	assert.Contains(t, unstreamed(resume(t, r, sess.ID()).History()), event.Event(n.Shown))
}

func TestSavedHistoryReplaysAClosedSession(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("noted")})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	saved := sess.Saved()
	want := sess.History()
	require.NoError(t, sess.Close(t.Context()))

	got, err := r.SavedHistory(saved)

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Contains(t, got, event.Event(event.TextDelta{Text: "noted"}))
}

func TestSavedHistoryOfAMissingFileIsNotSavedAndCreatesNothing(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	path := filepath.Join(t.TempDir(), "gone.jsonl")

	_, err := r.SavedHistory(Saved{path: path})

	require.ErrorIs(t, err, ErrNotSaved)
	assert.NoFileExists(t, path)
}

func TestHistoryRestoresTaskResultEnvelopes(t *testing.T) {
	for _, output := range []string{
		"Task t1 completed.\nCommand: make\nx\n",
		"Task t1 completed.\nAgent: explore › look\nx\n",
		"Task t1 completed.\nWorkflow: audit\nx\n",
		"Task t1 completed.\nordinary output\nx\n",
		"Task t1 completed. [named task]\nCommand: make\nx\n",
	} {
		t.Run(output, func(t *testing.T) {
			r, _ := newTestRuntime(t, newSandbox(t))
			sess := openTestSession(t, r)
			manager := sess.inner.SessionManager()
			appendHistoryMessage(t, manager, `{"role":"user","content":"go","timestamp":1}`)
			appendHistoryMessage(t, manager, `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"TaskOutput","arguments":{"task_id":"t1"}}],"stopReason":"toolUse","timestamp":2}`)
			encoded, err := json.Marshal(output)
			require.NoError(t, err)
			appendHistoryMessage(t, manager, `{"role":"toolResult","toolCallId":"call-1","toolName":"TaskOutput","content":[{"type":"text","text":`+string(encoded)+`}],"isError":false,"timestamp":3}`)
			require.NoError(t, sess.Close(t.Context()))

			assert.Equal(t, []event.Event{
				event.TurnStart{Prompt: "go"},
				event.ToolStart{CallID: "call-1", Name: "TaskOutput", Args: `{"task_id":"t1"}`},
				event.ToolEnd{CallID: "call-1", Name: "TaskOutput", Args: `{"task_id":"t1"}`, Output: output},
			}, resume(t, r, sess.ID(), tool.NewTaskOutput(nil)).History())
		})
	}
}
