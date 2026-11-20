package tool

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

func TestTaskStopDescribesItself(t *testing.T) {
	tool := NewTaskStop(NewTasks())

	assert.Equal(t, "TaskStop", tool.Name)
	assert.Equal(t, "TaskStop", tool.Label)
	assert.Equal(t, "Stops a running background task, a command or a subagent; a command is killed with the processes it started.", tool.Description)
	assert.Equal(t, []string{"task_id"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	assert.Contains(t, tool.Parameters["properties"], "task_id")
	assert.Equal(t, policy.Read, tool.Capability)
	assert.False(t, tool.Ask)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.False(t, tool.Deferred)
	assert.Equal(t, Action{Verb: "Stopped", One: "task", Many: "tasks"}, tool.Display.Action)
}

func TestTaskStopStopsRunningTask(t *testing.T) {
	tasks, events := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "sleep 600")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	tool := NewTaskStop(tasks)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 stopped.", out.Text)
	ended := nextEvent(t, events)
	assert.Equal(t, "t1", ended.ID)
	assert.Equal(t, TaskStopped, ended.Status)

	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 stopped.", out.Text)
}

func TestTaskStopReportsEndedTask(t *testing.T) {
	tasks, events := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "true")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, TaskCompleted, nextEvent(t, events).Status)

	out, err := NewTaskStop(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "Task t1 already completed (exit code 0).", out.Text)
}

func TestTaskStopStopsAgentTask(t *testing.T) {
	tasks, _ := newTasks(t)
	var agent *AgentTask
	agent = tasks.StartAgent("explore", "find it", func() { agent.End(TaskCompleted, "", "late") })
	tool := NewTaskStop(tasks)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 stopped.", out.Text)

	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 stopped.", out.Text)

	done := startAgent(tasks, "again")
	_, ok := done.End(TaskFailed, "boom", "")
	require.True(t, ok)
	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t2"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t2 already failed (boom).", out.Text)
}

func TestTaskStopRefusesUnknownTask(t *testing.T) {
	tasks, _ := newTasks(t)

	_, err := NewTaskStop(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t9"}`), allowTarget)

	require.ErrorIs(t, err, ErrUnknownTask)
	assert.Equal(t, `failed to find task "t9": unknown task`, err.Error())
}

func TestTaskStopDoesNotStopWithoutAdmission(t *testing.T) {
	tasks, _ := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "sleep 600")
	require.NoError(t, err)
	refused := errors.New("refused")

	out, err := NewTaskStop(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), func(target policy.Target) (policy.Mode, error) {
		assert.Equal(t, policy.Target{}, target)
		return policy.Execute, refused
	})

	require.ErrorIs(t, err, refused)
	assert.Empty(t, out.Text)
	info, err := tasks.Output("t1")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, info.Status)
}

func TestTaskStopRejectsInvalidArgs(t *testing.T) {
	tool := NewTaskStop(NewTasks())
	for _, raw := range []string{`{}`, `{"task_id":""}`, `{"task_id":" "}`, `{"task_id":"t1","extra":1}`, `{`} {
		_, err := tool.Run(t.Context(), json.RawMessage(raw), allowTarget)
		assert.ErrorIs(t, err, ErrInvalidArgs, raw)
	}
}

func TestTaskStopDisplay(t *testing.T) {
	tool := NewTaskStop(NewTasks())

	assert.Equal(t, "TaskStop t1", tool.Display.Title(json.RawMessage(`{"task_id":"t1"}`)))
	assert.Equal(t, "TaskStop", tool.Display.Title(json.RawMessage(`{}`)))
	assert.Equal(t, "Stopped task t1", tool.Display.Done(json.RawMessage(`{"task_id":"t1"}`)))
	assert.Empty(t, tool.Display.Body("Task t1 stopped."))
	assert.Equal(t, "stopped", tool.Display.Summary("Task t1 stopped."))
	assert.Equal(t, "already completed (exit code 0)", tool.Display.Summary("Task t1 already completed (exit code 0)."))
}
