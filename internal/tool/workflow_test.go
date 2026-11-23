package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type fakeWorkflows struct {
	started [][2]string
	err     error
}

func (f *fakeWorkflows) Start(_ context.Context, description, script string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.started = append(f.started, [2]string{description, script})
	return "t3", nil
}

func TestWorkflowDescribesItself(t *testing.T) {
	tool := NewWorkflow(testDefs, &fakeWorkflows{}, 3, 7)

	assert.Equal(t, "Workflow", tool.Name)
	assert.Equal(t, []string{"description", "script"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	props := tool.Parameters["properties"].(map[string]any)
	assert.Contains(t, props, "description")
	assert.Contains(t, props, "script")
	assert.Equal(t, policy.Read, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.True(t, tool.Deferred)
	assert.Equal(t, Action{Verb: "Launched", One: "workflow", Many: "workflows"}, tool.Display.Action)
	assert.Contains(t, tool.Description, "agentType names one of the agent types below")
	assert.Contains(t, tool.Description, "agent(prompt, {agentType, schema, label, phase})")
	assert.Contains(t, tool.Description, "- phase(title): sets the heading the user sees the agents under that agent() starts after it; the phase option of agent() sets it for that one agent.")
	assert.Contains(t, tool.Description, `phase: "Find"}`)
	assert.Contains(t, tool.Description, `phase: "Check"}`)
	assert.Contains(t, tool.Description, "Agent types:\nexplore: Finds things.\nreview: Reviews a change.\nExample")
	assert.Contains(t, tool.Description, "At most 7 of a script's agents run at once and the rest wait; an agent() call beyond the first 1000 fails the script. At most 3 scripts run at once.")
}

func TestWorkflowDescriptionWithoutDefinitions(t *testing.T) {
	tool := NewWorkflow(nil, nil, 4, 16)

	assert.Contains(t, tool.Description, "agent(prompt, {schema, label, phase})")
	assert.Contains(t, tool.Description, "The agent has the tools a subagent can have.")
	assert.NotContains(t, tool.Description, "agentType")
	assert.NotContains(t, tool.Description, "Agent types:")
	assert.Contains(t, tool.Description, "At most 16 of a script's agents run at once")
	assert.Contains(t, tool.Description, "At most 4 scripts run at once.")
}

func TestWorkflowStarts(t *testing.T) {
	w := &fakeWorkflows{}
	var admitted bool
	admit := func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, policy.Target{}, target)
		assert.Empty(t, w.started)
		admitted = true
		return policy.Execute, nil
	}

	out, err := NewWorkflow(nil, w, 4, 16).Run(t.Context(), json.RawMessage(`{"description":"audit","script":"return 1"}`), admit)

	require.NoError(t, err)
	assert.True(t, admitted)
	assert.Equal(t, "Workflow t3 started.", out.Text)
	assert.Equal(t, [][2]string{{"audit", "return 1"}}, w.started)
}

func TestWorkflowRejectsInvalidArgsBeforeAdmission(t *testing.T) {
	w := &fakeWorkflows{}
	tool := NewWorkflow(nil, w, 4, 16)
	admit := func(policy.Target) (policy.Mode, error) {
		t.Error("admit called")
		return policy.Execute, nil
	}
	for _, raw := range []string{
		`{`,
		`{"description":"d","script":"s","extra":1}`,
		`{"description":" ","script":"s"}`,
		`{"description":"d","script":" "}`,
		`{"script":"s"}`,
	} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), admit)
		assert.ErrorIs(t, err, ErrInvalidArgs, raw)
	}
	assert.Empty(t, w.started)
}

func TestWorkflowDoesNotStartWithoutAdmission(t *testing.T) {
	w := &fakeWorkflows{}
	refused := errors.New("refused")

	_, err := NewWorkflow(nil, w, 4, 16).Run(t.Context(), json.RawMessage(`{"description":"d","script":"s"}`), func(policy.Target) (policy.Mode, error) {
		return policy.Execute, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, w.started)
}

func TestWorkflowReturnsStartError(t *testing.T) {
	failed := errors.New("too many")

	_, err := NewWorkflow(nil, &fakeWorkflows{err: failed}, 4, 16).Run(t.Context(), json.RawMessage(`{"description":"d","script":"s"}`), allowTarget)

	assert.ErrorIs(t, err, failed)
}

func TestWorkflowDisplay(t *testing.T) {
	tool := NewWorkflow(nil, nil, 4, 16)

	assert.Equal(t, "Workflow › audit", tool.Display.Title(json.RawMessage(`{"description":"audit","script":"s"}`)))
	assert.Equal(t, "Workflow", tool.Display.Title(json.RawMessage(`{`)))
	assert.Equal(t, "launched", tool.Display.Summary("Workflow t3 started."))
	assert.Empty(t, tool.Display.Summary("too many"))
	assert.Nil(t, tool.Display.Done)
}

func TestStructuredOutputRecordsLastCall(t *testing.T) {
	schema := map[string]any{"type": "object"}
	var recorded []json.RawMessage
	tool := NewStructuredOutput(schema, func(raw json.RawMessage) { recorded = append(recorded, raw) })

	assert.Equal(t, "StructuredOutput", tool.Name)
	assert.Equal(t, schema, tool.Parameters)
	assert.Equal(t, policy.Read, tool.Capability)
	assert.True(t, tool.Exclusive)
	assert.False(t, tool.Deferred)
	for _, raw := range []string{`{"a":1}`, `{"a":2}`} {
		out, err := tool.Run(t.Context(), json.RawMessage(raw), allowTarget)
		require.NoError(t, err)
		assert.Equal(t, "Recorded.", out.Text)
	}

	require.Len(t, recorded, 2)
	assert.JSONEq(t, `{"a":2}`, string(recorded[len(recorded)-1]))
}

func TestStructuredOutputDoesNotRecordWithoutAdmission(t *testing.T) {
	refused := errors.New("refused")
	tool := NewStructuredOutput(map[string]any{"type": "object"}, func(json.RawMessage) { t.Error("recorded") })

	_, err := tool.Run(t.Context(), json.RawMessage(`{}`), func(policy.Target) (policy.Mode, error) {
		return policy.Execute, refused
	})

	assert.ErrorIs(t, err, refused)
}

func TestWorkflowDisplayOpensToTheScript(t *testing.T) {
	display := NewWorkflow(nil, nil, 4, 16).Display

	summary, lines := display.Change(json.RawMessage(`{"description":"audit","script":"const a = 1\r\nreturn a\n"}`))

	assert.Empty(t, summary)
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "const a = 1"}, {Kind: Context, Text: "return a"}}, lines)
	_, lines = display.Change(json.RawMessage(`not json`))
	assert.Empty(t, lines)
}

func TestStructuredOutputDisplay(t *testing.T) {
	display := NewStructuredOutput(map[string]any{"type": "object"}, func(json.RawMessage) {}).Display

	assert.Equal(t, "Returned result", display.Done(json.RawMessage(`{"a":1}`)))
	assert.Nil(t, display.Summary)
	assert.Equal(t, Action{Verb: "Returned", One: "result", Many: "results"}, display.Action)
	assert.Nil(t, display.Body)
	summary, lines := display.Change(json.RawMessage(`{"a":1}`))
	assert.Empty(t, summary)
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "{"}, {Kind: Context, Text: `  "a": 1`}, {Kind: Context, Text: "}"}}, lines)
	_, lines = display.Change(json.RawMessage(`not json`))
	assert.Equal(t, []PreviewLine{{Kind: Context, Text: "not json"}}, lines)
}
