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
	assert.Equal(t, "Task t1 stopped. [named task]\nCommand: sleep 600", out.Text)
	ended := nextEvent(t, events)
	assert.Equal(t, "t1", ended.ID)
	assert.Equal(t, TaskStopped, ended.Status)

	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 already stopped. [named task]\nCommand: sleep 600", out.Text)
}

func TestTaskStopReportsEndedTask(t *testing.T) {
	tasks, events := newTasks(t)
	_, err := tasks.Start(t.Context(), NewShell(t.TempDir(), nil), "true")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, TaskCompleted, nextEvent(t, events).Status)

	out, err := NewTaskStop(tasks).Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "Task t1 already completed (exit code 0). [named task]\nCommand: true", out.Text)
}

func TestTaskStopStopsAgentTask(t *testing.T) {
	tasks, _ := newTasks(t)
	var agent *AgentTask
	agent = tasks.StartAgent("explore", "find it", func() { agent.End(TaskCompleted, "", "late") })
	tool := NewTaskStop(tasks)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 stopped. [named task]\nAgent: explore › find it", out.Text)

	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t1"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t1 already stopped. [named task]\nAgent: explore › find it", out.Text)

	done := startAgent(tasks, "again")
	_, ok := done.End(TaskFailed, "boom", "")
	require.True(t, ok)
	out, err = tool.Run(t.Context(), json.RawMessage(`{"task_id":"t2"}`), allowTarget)
	require.NoError(t, err)
	assert.Equal(t, "Task t2 already failed (boom). [named task]\nAgent: explore › again", out.Text)
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
	assert.Empty(t, tool.Display.Summary("Task t1 stopped."))
	assert.Equal(t, "already completed (exit code 0)", tool.Display.Summary("Task t1 already completed (exit code 0)."))
}

func TestTaskStopOutcome(t *testing.T) {
	tool := NewTaskStop(NewTasks())
	args := json.RawMessage(`{"task_id":"t1"}`)
	for _, tc := range []struct {
		name, output, done string
		action             Action
	}{
		{"stopped agent", "Task t1 stopped. [named task]\nAgent: explore › look", "Stopped explore › look", Action{Verb: "Stopped", One: "agent", Many: "agents"}},
		{"stopped shell", "Task t1 stopped. [named task]\nCommand: make", "Stopped shell make", Action{Verb: "Stopped", One: "shell", Many: "shells"}},
		{"stopped workflow", "Task t1 stopped. [named task]\nWorkflow: audit", "Stopped workflow › audit", Action{Verb: "Stopped", One: "workflow", Many: "workflows"}},
		{"stopped without name", "Task t1 stopped.", "", Action{}},
		{"over agent", "Task t1 already failed (boom). [named task]\nAgent: explore › look", "TaskStop explore › look", Action{Verb: "Checked on", One: "agent", Many: "agents"}},
		{"over shell", "Task t1 already completed (exit code 0). [named task]\nCommand: make", "TaskStop shell make", Action{Verb: "Checked on", One: "shell", Many: "shells"}},
		{"over workflow", "Task t1 already completed. [named task]\nWorkflow: audit", "TaskStop workflow › audit", Action{Verb: "Checked on", One: "workflow", Many: "workflows"}},
		{"over without name", "Task t1 already completed.", "TaskStop t1", Action{Verb: "Checked", One: "task", Many: "tasks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, action := tool.Display.Outcome(args, tc.output)
			assert.Equal(t, tc.done, done)
			assert.Equal(t, tc.action, action)
		})
	}
}
