//go:build unix

package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/sandbox"
)

type bashResult struct {
	out string
	err error
}

func runBashAsync(ctx context.Context, tool Tool, args string) <-chan bashResult {
	ch := make(chan bashResult, 1)
	go func() {
		out, err := tool.Run(ctx, json.RawMessage(args), allowTarget)
		ch <- bashResult{out.Text, err}
	}()
	return ch
}

func waitBash(t *testing.T, ch <-chan bashResult) bashResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Bash to return")
		return bashResult{}
	}
}

// taskChanges delivers what tasks reports to OnChange.
func taskChanges(tasks *Tasks) <-chan TaskInfo {
	ch := make(chan TaskInfo, 8)
	tasks.OnChange(func(info TaskInfo) { ch <- info })
	return ch
}

func waitChange(t *testing.T, ch <-chan TaskInfo) TaskInfo {
	t.Helper()
	select {
	case info := <-ch:
		return info
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a task change")
		return TaskInfo{}
	}
}

func newTaskBash(t *testing.T, dir string) (Tool, *Tasks) {
	t.Helper()
	tasks := NewTasks()
	t.Cleanup(tasks.StopAll)
	return NewBash(NewShell(dir, nil), NewShell(dir, nil), tasks, true), tasks
}

func TestBashRunInBackgroundStartsTask(t *testing.T) {
	dir := t.TempDir()
	tool, tasks := newTaskBash(t, dir)
	command, started, _ := holdFIFO(t, dir)
	args, err := json.Marshal(map[string]any{"command": command, "run_in_background": true})
	require.NoError(t, err)

	out, err := tool.Run(t.Context(), args, allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "Background task t1 started.", out.Text)
	waitFor(t, started, "the command to start")
	list := tasks.List()
	require.Len(t, list, 1)
	assert.Equal(t, "t1", list[0].ID)
	assert.Equal(t, TaskRunning, list[0].Status)
}

func TestBashRunInBackgroundUsesTheShellOfTheAdmittedMode(t *testing.T) {
	execDir, planDir := t.TempDir(), t.TempDir()
	tasks := NewTasks()
	t.Cleanup(tasks.StopAll)
	changes := taskChanges(tasks)
	tool := NewBash(NewShell(execDir, &sandbox.Sandbox{}), NewShell(planDir, &sandbox.Sandbox{}), tasks, true)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch plan","run_in_background":true}`), func(policy.Target) (policy.Mode, error) {
		return policy.Plan, nil
	})

	require.NoError(t, err)
	assert.Equal(t, "Background task t1 started.", out.Text)
	assert.Equal(t, TaskRunning, waitChange(t, changes).Status)
	assert.Equal(t, TaskCompleted, waitChange(t, changes).Status)
	assert.Equal(t, []string{"plan"}, listDir(t, planDir))
	assert.Empty(t, listDir(t, execDir))
}

func TestBashUnsandboxedBackgroundUsesTheUnsandboxedShell(t *testing.T) {
	dir := t.TempDir()
	tasks := NewTasks()
	t.Cleanup(tasks.StopAll)
	changes := taskChanges(tasks)
	sh := NewShell(dir, &sandbox.Sandbox{})
	sh.shell = filepath.Join(dir, "missing-shell")
	tool := NewBash(sh, NewShell(dir, &sandbox.Sandbox{}), tasks, true)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch outside","unsandboxed":true,"run_in_background":true}`), func(policy.Target) (policy.Mode, error) {
		return policy.Execute, nil
	})

	require.NoError(t, err)
	assert.Equal(t, "Background task t1 started.", out.Text)
	assert.Equal(t, TaskRunning, waitChange(t, changes).Status)
	assert.Equal(t, TaskCompleted, waitChange(t, changes).Status)
	assert.Equal(t, []string{"outside"}, listDir(t, dir))
}

func TestBashUnsandboxedTimeoutMovesCommandToBackground(t *testing.T) {
	dir := t.TempDir()
	tasks := NewTasks()
	t.Cleanup(tasks.StopAll)
	changes := taskChanges(tasks)
	sh := NewShell(dir, &sandbox.Sandbox{})
	sh.shell = filepath.Join(dir, "missing-shell")
	tool := NewBash(sh, NewShell(dir, &sandbox.Sandbox{}), tasks, true)
	gate := mkfifo(t, dir, "gate")
	args, err := json.Marshal(map[string]any{"command": "read x <'" + gate + "'; touch outside", "timeout": 1, "unsandboxed": true})
	require.NoError(t, err)

	out, err := tool.Run(t.Context(), args, func(policy.Target) (policy.Mode, error) { return policy.Execute, nil })

	require.NoError(t, err)
	assert.Equal(t, "Background task t1: this command did not finish within its 1ms timeout and keeps running in the background.", out.Text)
	assert.Equal(t, TaskRunning, waitChange(t, changes).Status)
	require.NoError(t, os.WriteFile(gate, []byte("go\n"), 0o600))
	assert.Equal(t, TaskCompleted, waitChange(t, changes).Status)
	assert.FileExists(t, filepath.Join(dir, "outside"))
}

func TestBashTimeoutMovesCommandToBackground(t *testing.T) {
	dir := t.TempDir()
	tool, tasks := newTaskBash(t, dir)
	changes := taskChanges(tasks)
	gate := mkfifo(t, dir, "gate")
	args, err := json.Marshal(map[string]any{"command": "read x <'" + gate + "'", "timeout": 1})
	require.NoError(t, err)

	out, err := tool.Run(t.Context(), args, allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "Background task t1: this command did not finish within its 1ms timeout and keeps running in the background.", out.Text)
	running := waitChange(t, changes)
	assert.Equal(t, "t1", running.ID)
	assert.Equal(t, TaskRunning, running.Status)
	// Opening blocks until the shell reads the FIFO.
	require.NoError(t, os.WriteFile(gate, []byte("go\n"), 0o600))
	ended := waitChange(t, changes)
	assert.Equal(t, "t1", ended.ID)
	assert.Equal(t, TaskCompleted, ended.Status)
}

func TestBashDetachMovesCommandToBackground(t *testing.T) {
	dir := t.TempDir()
	tool, tasks := newTaskBash(t, dir)
	command, started, _ := holdFIFO(t, dir)
	args, err := json.Marshal(map[string]any{"command": command})
	require.NoError(t, err)

	result := runBashAsync(t.Context(), tool, string(args))
	waitFor(t, started, "the command to start")
	require.True(t, tasks.Detach())

	r := waitBash(t, result)
	require.NoError(t, r.err)
	assert.Equal(t, "Background task t1: the user moved this command to the background, where it keeps running.", r.out)
	list := tasks.List()
	require.Len(t, list, 1)
	assert.Equal(t, TaskRunning, list[0].Status)
}

func TestBashCancelKillsForegroundCommand(t *testing.T) {
	dir := t.TempDir()
	tool, tasks := newTaskBash(t, dir)
	command, started, dead := holdFIFO(t, dir)
	args, err := json.Marshal(map[string]any{"command": command})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := runBashAsync(ctx, tool, string(args))
	waitFor(t, started, "the command to start")
	cancel()

	r := waitBash(t, result)
	require.ErrorIs(t, r.err, context.Canceled)
	waitFor(t, dead, "the grandchild to die")
	assert.Empty(t, tasks.List())
}

func TestForegroundBashStopsCommandAtTimeout(t *testing.T) {
	dir := t.TempDir()
	tool := NewBash(NewShell(dir, nil), NewShell(dir, nil), nil, true)
	command, started, dead := holdFIFO(t, dir)
	args, err := json.Marshal(map[string]any{"command": "echo partial; " + command, "timeout": 500})
	require.NoError(t, err)

	result := runBashAsync(t.Context(), tool, string(args))
	waitFor(t, started, "the command to start")

	r := waitBash(t, result)
	require.ErrorIs(t, r.err, ErrCommandTimeout)
	assert.Contains(t, r.err.Error(), "partial")
	waitFor(t, dead, "the grandchild to die")
}

func TestForegroundBashCancelKillsCommand(t *testing.T) {
	dir := t.TempDir()
	tool := NewBash(NewShell(dir, nil), NewShell(dir, nil), nil, true)
	command, started, dead := holdFIFO(t, dir)
	args, err := json.Marshal(map[string]any{"command": command})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := runBashAsync(ctx, tool, string(args))
	waitFor(t, started, "the command to start")
	cancel()

	r := waitBash(t, result)
	require.ErrorIs(t, r.err, context.Canceled)
	waitFor(t, dead, "the grandchild to die")
}

func TestBashDescribesTheProxy(t *testing.T) {
	sh := proxyShell(t, t.TempDir())

	tool := NewBash(sh, sh, nil, false)

	assert.Contains(t, tool.Description, "may instead appear empty. The network is reachable only through the HTTP proxy named in HTTP_PROXY and HTTPS_PROXY, "+
		"and only for hosts the user listed; the command's output then ends with the hosts the proxy refused.")
}
