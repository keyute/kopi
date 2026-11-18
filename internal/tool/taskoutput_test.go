package tool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func TestTaskOutputDescribesItself(t *testing.T) {
	tool := NewTaskOutput(NewTasks())

	assert.Equal(t, "TaskOutput", tool.Name)
	assert.Equal(t, "TaskOutput", tool.Label)
	assert.Equal(t, "Returns a background task's status and its combined output so far; long output keeps its start and end.", tool.Description)
	assert.Equal(t, []string{"task_id"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Contains(t, tool.Parameters["properties"], "task_id")
	assert.Equal(t, policy.Read, tool.Capability)
	assert.False(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.False(t, tool.Deferred)
	assert.Equal(t, Action{Verb: "Checked", One: "task", Many: "tasks"}, tool.Display.Action)
}

func TestTaskOutputReportsRunningTask(t *testing.T) {
	tasks, _ := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "sleep 600")
	require.NoError(t, err)

	out, err := NewTaskOutput(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "Task t1 running for "), out)
	assert.True(t, strings.HasSuffix(out, "\n(no output)"), out)
}

func TestTaskOutputReportsEndedTask(t *testing.T) {
	tasks, events := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "echo hi; exit 3")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, TaskFailed, nextEvent(t, events).Status)

	out, err := NewTaskOutput(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "Task t1 failed (exit code 3).\nhi\n", out)
}

func TestTaskOutputRefusesUnknownTask(t *testing.T) {
	tasks, _ := newTasks(t)

	_, err := NewTaskOutput(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t9"}`), allowTarget)

	require.ErrorIs(t, err, ErrUnknownTask)
	assert.Equal(t, `failed to find task "t9": unknown task`, err.Error())
}

func TestTaskOutputDoesNotReadWithoutAdmission(t *testing.T) {
	refused := errors.New("refused")
	// A nil manager panics if the tool reads it.
	tool := NewTaskOutput(nil)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, policy.Target{}, target)
		return policy.Plan, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, out)
}

func TestTaskOutputRejectsInvalidArgs(t *testing.T) {
	tool := NewTaskOutput(NewTasks())
	for _, raw := range []string{`{}`, `{"task_id":""}`, `{"task_id":" "}`, `{"task_id":"t1","extra":1}`, `{`} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), allowTarget)
		assert.ErrorIs(t, err, ErrInvalidArgs, raw)
	}
}

func TestTaskOutputDisplay(t *testing.T) {
	tool := NewTaskOutput(NewTasks())

	assert.Equal(t, "TaskOutput t1", tool.Display.Title(json.RawMessage(`{"task_id":"t1"}`)))
	assert.Equal(t, "TaskOutput", tool.Display.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "running for 42s", tool.Display.Summary("Task t1 running for 42s.\n(no output)"))
	assert.Equal(t, "completed (exit code 0)", tool.Display.Summary("Task t1 completed (exit code 0).\nhi\n"))
	assert.Equal(t, "stopped", tool.Display.Summary("Task t1 stopped.\n(no output)"))
}
