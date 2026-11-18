package tool

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func TestSendMessageDescribesItself(t *testing.T) {
	tool := NewSendMessage(nil)

	assert.Equal(t, "SendMessage", tool.Name)
	assert.Equal(t, "SendMessage", tool.Label)
	assert.Equal(t, "Sends a message to a subagent by its task id. A running subagent reads it when its current step ends; one that has ended starts again with its earlier context and reports again when it ends.", tool.Description)
	assert.Equal(t, []string{"to", "message"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Contains(t, tool.Parameters["properties"], "to")
	assert.Contains(t, tool.Parameters["properties"], "message")
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Deferred)
	assert.Equal(t, Action{Verb: "Sent", One: "message", Many: "messages"}, tool.Display.Action)
}

func TestSendMessageQueuesAndResumes(t *testing.T) {
	for _, tc := range []struct {
		resumed bool
		text    string
	}{{false, "Message queued for t3."}, {true, "Agent t3 continued."}} {
		agents := &fakeAgents{resumed: tc.resumed}
		admit := func(target policy.Target) (policy.Mode, error) {
			assert.Equal(t, policy.Target{}, target)
			assert.Empty(t, agents.messages)
			return policy.Execute, nil
		}

		out, err := NewSendMessage(agents).Run(t.Context(), json.RawMessage(`{"to":"t3","message":"hi"}`), admit)

		require.NoError(t, err)
		assert.Equal(t, tc.text, out.Text)
		assert.Equal(t, [][2]string{{"t3", "hi"}}, agents.messages)
	}
}

func TestSendMessageRejectsInvalidArgsBeforeAdmission(t *testing.T) {
	agents := &fakeAgents{}
	tool := NewSendMessage(agents)
	for _, raw := range []string{`{`, `{"to":"t3"}`, `{"to":" ","message":"hi"}`, `{"to":"t3","message":""}`, `{"to":"t3","message":"hi","extra":1}`} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), func(policy.Target) (policy.Mode, error) {
			t.Error("admit called")
			return policy.Execute, nil
		})
		assert.ErrorIs(t, err, ErrInvalidArgs, raw)
	}
	assert.Empty(t, agents.messages)
}

func TestSendMessageDoesNotSendWithoutAdmission(t *testing.T) {
	agents := &fakeAgents{}
	refused := errors.New("refused")

	_, err := NewSendMessage(agents).Run(t.Context(), json.RawMessage(`{"to":"t3","message":"hi"}`), func(policy.Target) (policy.Mode, error) {
		return policy.Execute, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, agents.messages)
}

func TestSendMessageReturnsMessageError(t *testing.T) {
	_, err := NewSendMessage(&fakeAgents{err: ErrUnknownTask}).Run(t.Context(), json.RawMessage(`{"to":"t9","message":"hi"}`), allowTarget)

	assert.ErrorIs(t, err, ErrUnknownTask)
}

func TestSendMessageDisplay(t *testing.T) {
	tool := NewSendMessage(nil)

	assert.Equal(t, "SendMessage t3", tool.Display.Title(json.RawMessage(`{"to":"t3","message":"hi"}`)))
	assert.Equal(t, "SendMessage", tool.Display.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "queued", tool.Display.Summary("Message queued for t3."))
	assert.Equal(t, "continued", tool.Display.Summary("Agent t3 continued."))
}
