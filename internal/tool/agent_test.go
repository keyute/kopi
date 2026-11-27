package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
)

type fakeAgents struct {
	launched [][3]string
	messages [][2]string
	outcome  MessageOutcome
	to       TaskInfo
	err      error
}

func (f *fakeAgents) Launch(_ context.Context, agent, description, prompt string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.launched = append(f.launched, [3]string{agent, description, prompt})
	return "t3", nil
}

func (f *fakeAgents) Message(_ context.Context, id, message string) (TaskInfo, MessageOutcome, error) {
	if f.err != nil {
		return TaskInfo{}, 0, f.err
	}
	f.messages = append(f.messages, [2]string{id, message})
	return f.to, f.outcome, nil
}

var testDefs = []subagent.Definition{
	{Name: "explore", Description: "Finds things."},
	{Name: "review", Description: "Reviews a change."},
}

func TestAgentDescribesItself(t *testing.T) {
	tool := NewAgent(testDefs, &fakeAgents{})

	assert.Equal(t, "Agent", tool.Name)
	assert.Equal(t, "Agent", tool.Label)
	assert.Equal(t, "A subagent is another agent with its own context, instructions and tools; it sees only the prompt it is given. "+
		"A call returns at once with a task id while the subagent runs in the background; its end is reported to you with its final message. "+
		"TaskStop stops it, and SendMessage sends it a further message. Subagents:\n"+
		"explore: Finds things.\nreview: Reviews a change.", tool.Description)
	assert.Equal(t, []string{"description", "prompt", "subagent_type"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	props := tool.Parameters["properties"].(map[string]any)
	assert.Contains(t, props, "description")
	assert.Contains(t, props, "prompt")
	assert.Equal(t, []string{"explore", "review"}, props["subagent_type"].(map[string]any)["enum"])
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Deferred)
	assert.Equal(t, Action{Verb: "Launched", One: "agent", Many: "agents"}, tool.Display.Action)
}

func TestAgentConstructsWithoutDefinitions(t *testing.T) {
	tool := NewAgent(nil, nil)

	assert.NotContains(t, tool.Description, "Subagents:")
	assert.Empty(t, tool.Parameters["properties"].(map[string]any)["subagent_type"].(map[string]any)["enum"])
}

func TestAgentLaunches(t *testing.T) {
	agents := &fakeAgents{}
	var admitted bool
	admit := func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, policy.Target{}, target)
		assert.Empty(t, agents.launched)
		admitted = true
		return policy.Execute, nil
	}

	out, err := NewAgent(testDefs, agents).Run(t.Context(), json.RawMessage(`{"description":"look","prompt":"find it","subagent_type":"explore"}`), admit)

	require.NoError(t, err)
	assert.True(t, admitted)
	assert.Equal(t, "Agent t3 started. Its end is reported to you and starts a new turn if yours has ended: to wait for it, end your turn, and do not sleep or do its task yourself.", out.Text)
	assert.Equal(t, [][3]string{{"explore", "look", "find it"}}, agents.launched)
}

func TestAgentRejectsInvalidArgsBeforeAdmission(t *testing.T) {
	agents := &fakeAgents{}
	tool := NewAgent(testDefs, agents)
	admit := func(policy.Target) (policy.Mode, error) {
		t.Error("admit called")
		return policy.Execute, nil
	}
	for _, raw := range []string{
		`{`,
		`{"description":"d","prompt":"p","subagent_type":"explore","extra":1}`,
		`{"description":" ","prompt":"p","subagent_type":"explore"}`,
		`{"description":"d","prompt":"","subagent_type":"explore"}`,
		`{"prompt":"p","subagent_type":"explore"}`,
	} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), admit)
		assert.ErrorIs(t, err, ErrInvalidArgs, raw)
	}

	_, err := tool.Run(t.Context(), json.RawMessage(`{"description":"d","prompt":"p","subagent_type":"nope"}`), admit)

	require.ErrorIs(t, err, ErrUnknownAgent)
	assert.Contains(t, err.Error(), "nope")
	assert.Empty(t, agents.launched)
}

func TestAgentDoesNotLaunchWithoutAdmission(t *testing.T) {
	agents := &fakeAgents{}
	refused := errors.New("refused")

	out, err := NewAgent(testDefs, agents).Run(t.Context(), json.RawMessage(`{"description":"d","prompt":"p","subagent_type":"explore"}`), func(policy.Target) (policy.Mode, error) {
		return policy.Execute, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, out.Text)
	assert.Empty(t, agents.launched)
}

func TestAgentReturnsLaunchError(t *testing.T) {
	failed := errors.New("failed to start")

	_, err := NewAgent(testDefs, &fakeAgents{err: failed}).Run(t.Context(), json.RawMessage(`{"description":"d","prompt":"p","subagent_type":"explore"}`), allowTarget)

	assert.ErrorIs(t, err, failed)
}

func TestAgentDisplay(t *testing.T) {
	tool := NewAgent(testDefs, nil)

	assert.Equal(t, "explore › look", tool.Display.Title(json.RawMessage(`{"description":"look","prompt":"p","subagent_type":"explore"}`)))
	assert.Equal(t, "Agent", tool.Display.Title(json.RawMessage(`{`)))
	assert.Equal(t, "launched", tool.Display.Summary("Agent t3 started. Its end is reported to you and starts a new turn if yours has ended: to wait for it, end your turn, and do not sleep or do its task yourself."))
	assert.Equal(t, "launched", tool.Display.Summary("Agent t3 started."), "a session saved before the result said how the end arrives")
	_, lines := tool.Display.Change(json.RawMessage(`{"description":"look","prompt":"a\nb","subagent_type":"explore"}`))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "a"}, {Kind: Context, Text: "b"}}, lines)
	assert.Nil(t, tool.Display.Body)
	assert.Nil(t, tool.Display.Done)
	assert.Empty(t, tool.Display.Summary("failed to start"))
}
