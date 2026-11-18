package tool

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTasks(t *testing.T) (*Tasks, chan TaskInfo) {
	t.Helper()
	m := NewTasks()
	events := make(chan TaskInfo, 16)
	m.OnChange(func(i TaskInfo) { events <- i })
	t.Cleanup(m.StopAll)
	return m, events
}

func nextEvent(t *testing.T, events <-chan TaskInfo) TaskInfo {
	t.Helper()
	select {
	case i := <-events:
		return i
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a task event")
		return TaskInfo{}
	}
}

func TestTasksRunPassesForegroundResult(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)

	out, moved, err := m.Run(t.Context(), sh, "echo hi", testTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hi\n", out)
	assert.Equal(t, Moved{}, moved)

	_, moved, err = m.Run(t.Context(), sh, "echo oops; exit 3", testTimeout)
	require.ErrorIs(t, err, ErrExitStatus)
	assert.Equal(t, "oops\n\ncommand failed: exit code 3", err.Error())
	assert.Equal(t, Moved{}, moved)

	assert.Empty(t, events)
	assert.Empty(t, m.List())
	assert.False(t, m.Foreground())
}

func TestTasksDoNotStartCanceled(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, _ := newTasks(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, moved, err := m.Run(ctx, sh, "echo unexpected", testTimeout)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, out)
	assert.Equal(t, Moved{}, moved)
	_, err = m.Start(ctx, sh, "echo unexpected")
	require.ErrorIs(t, err, context.Canceled)

	info, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	assert.Equal(t, "t1", info.ID)
}

func TestTasksStartReportsEnd(t *testing.T) {
	for _, tc := range []struct {
		command string
		status  TaskStatus
		reason  string
	}{
		{"true", TaskCompleted, "exit code 0"},
		{"echo oops; exit 3", TaskFailed, "exit code 3"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			sh := NewShell(t.TempDir(), nil)
			m, events := newTasks(t)

			info, err := m.Start(t.Context(), sh, tc.command)

			require.NoError(t, err)
			assert.Equal(t, "t1", info.ID)
			assert.Equal(t, tc.command, info.Command)
			assert.Equal(t, TaskRunning, info.Status)
			assert.Equal(t, info, nextEvent(t, events))
			end := nextEvent(t, events)
			assert.Equal(t, "t1", end.ID)
			assert.Equal(t, tc.status, end.Status)
			assert.Equal(t, tc.reason, end.Reason)
			assert.False(t, end.Ended.IsZero())
			got, err := m.Output("t1")
			require.NoError(t, err)
			assert.Equal(t, end, got)
			listed := end
			listed.Output = ""
			assert.Equal(t, []TaskInfo{listed}, m.List())
		})
	}
}

func TestTasksRejectUnknownTask(t *testing.T) {
	m, _ := newTasks(t)

	_, err := m.Output("t1")
	require.ErrorIs(t, err, ErrUnknownTask)
	_, err = m.Stop("t1")
	require.ErrorIs(t, err, ErrUnknownTask)
}

func TestTasksStopAllForgetsTasks(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	_, err := m.Start(t.Context(), sh, "sleep 600")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)

	m.StopAll()

	assert.Empty(t, m.List())
	_, err = m.Output("t1")
	require.ErrorIs(t, err, ErrUnknownTask)
	assert.Empty(t, events)
	info, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	assert.Equal(t, "t2", info.ID)
}

func TestTaskInfoStatusText(t *testing.T) {
	for _, tc := range []struct {
		info TaskInfo
		want string
	}{
		{TaskInfo{Status: TaskRunning, Started: time.Now().Add(-42 * time.Second)}, "running for 42s"},
		{TaskInfo{Status: TaskCompleted, Reason: "exit code 0"}, "completed (exit code 0)"},
		{TaskInfo{Status: TaskFailed, Reason: "exit code 3"}, "failed (exit code 3)"},
		{TaskInfo{Status: TaskStopped}, "stopped"},
	} {
		assert.Equal(t, tc.want, tc.info.StatusText())
	}
}

func TestTasksStopAllWaitsForEndedCallback(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m := NewTasks()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m.OnChange(func(i TaskInfo) {
		if i.Status == TaskRunning {
			return
		}
		close(entered)
		<-release
		close(finished)
	})
	_, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the ended callback")
	}
	returned := make(chan bool, 1)
	go func() {
		m.StopAll()
		select {
		case <-finished:
			returned <- true
		default:
			returned <- false
		}
	}()

	close(release)

	select {
	case callbackDone := <-returned:
		assert.True(t, callbackDone, "StopAll returned while the ended callback was still running")
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for StopAll")
	}
}
