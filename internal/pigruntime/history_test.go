package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
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
			Run:        func(context.Context, json.RawMessage) (string, error) { return "", errors.New("probe failed") },
		},
	}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	assert.Empty(t, sess.History())

	live := []event.Event{event.TurnStart{Prompt: "probe please"}}
	unsubscribe := sess.Subscribe(func(ev event.Event) { live = append(live, ev) })
	outcome, err := sess.Send(t.Context(), "probe please")
	require.NoError(t, err)
	require.Equal(t, event.Outcome{Status: event.Completed}, outcome)
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

func TestHistoryEndsFailedAndInterruptedTurns(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		sb := newSandbox(t)
		r, faux := newTestRuntime(t, sb)
		faux.SetResponses([]ai.FauxResponseStep{
			ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}),
		})
		sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		_, err = sess.Send(t.Context(), "go")
		require.NoError(t, err)
		require.NoError(t, sess.Close(t.Context()))

		assert.Equal(t, []event.Event{
			event.TurnStart{Prompt: "go"},
			event.TurnEnd{Outcome: event.Outcome{Status: event.Failed, Message: "invalid request: bad thing"}},
		}, resume(t, r, sess.ID()).History())
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
			_, _ = sess.Send(turnCtx, "block")
		}()
		<-started
		cancel()
		<-done
		require.NoError(t, sess.Close(t.Context()))

		assert.Equal(t, []event.Event{
			event.TurnStart{Prompt: "block"},
			event.ToolStart{CallID: "call-1", Name: "Probe", Args: "{}"},
			event.ToolEnd{CallID: "call-1", Name: "Probe", Output: context.Canceled.Error(), IsError: true},
			event.TurnEnd{Outcome: event.Outcome{Status: event.Interrupted}},
		}, resume(t, r, sess.ID(), block).History())
	})
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
	_, err := sess.Send(t.Context(), "count me")
	require.NoError(t, err)
	assert.Greater(t, mirrors(), before)
}
