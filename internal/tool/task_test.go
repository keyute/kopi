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

// startAgent starts an agent task whose stop func ends it, so StopAll in the
// cleanup does not wait on it forever.
func startAgent(m *Tasks, description string) *AgentTask {
	var a *AgentTask
	a = m.StartAgent("explore", description, func() { a.End(TaskStopped, "", "") })
	return a
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

type startingContext struct {
	context.Context
	entered, release chan struct{}
}

func (c *startingContext) Err() error {
	close(c.entered)
	<-c.release
	return nil
}

func TestTasksStopAllOwnsPendingStart(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	signals := make(chan struct{}, 2)
	m.OnForeground(func() { signals <- struct{}{} })
	ctx := &startingContext{Context: t.Context(), entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := m.Start(ctx, sh, "sleep 600")
		result <- err
	}()
	select {
	case <-ctx.entered:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for Start")
	}

	// A synthetic background process acknowledges StopAll's snapshot before
	// the real pending start is released into the OS.
	p := &process{stopCh: make(chan error), done: make(chan struct{})}
	close(p.done)
	watched := make(chan struct{})
	close(watched)
	m.mu.Lock()
	var pending *task
	for task := range m.pending {
		pending = task
	}
	m.tasks = append(m.tasks, &task{p: p, watched: watched})
	m.mu.Unlock()
	stopped := make(chan struct{})
	go func() {
		m.StopAll()
		close(stopped)
	}()
	select {
	case err := <-p.stopCh:
		assert.ErrorIs(t, err, ErrStopped)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for StopAll's snapshot")
	}
	close(ctx.release)
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrStopped)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for Start to return")
	}
	select {
	case <-stopped:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for StopAll")
	}
	require.NotNil(t, pending)
	assert.True(t, ended(pending.p))
	assert.Empty(t, m.List())
	assert.Empty(t, events)
	assert.Empty(t, signals)
	info, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	assert.Equal(t, "t1", info.ID)
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
		{TaskInfo{Status: TaskCompleted}, "completed"},
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

func TestTasksAgentSharesIDsWithShells(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	_, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	agent := startAgent(m, "find it")
	_, err = m.Start(t.Context(), sh, "true")
	require.NoError(t, err)

	assert.Equal(t, "t2", agent.ID())
	list := m.List()
	require.Len(t, list, 3)
	assert.Equal(t, []string{"t1", "t2", "t3"}, []string{list[0].ID, list[1].ID, list[2].ID})
	assert.Equal(t, "", list[0].Agent)
	assert.Equal(t, "explore", list[1].Agent)
	assert.Equal(t, "find it", list[1].Command)
	assert.Equal(t, TaskRunning, list[1].Status)
	assert.False(t, list[1].Started.IsZero())
	// Only the shells report: two starts and two ends.
	for range 4 {
		assert.NotEqual(t, "t2", nextEvent(t, events).ID)
	}
}

func TestTasksAgentEndNeverCallsOnChange(t *testing.T) {
	m, events := newTasks(t)
	agent := startAgent(m, "find it")

	info, ok := agent.End(TaskCompleted, "", "the answer")
	require.True(t, ok)
	assert.Equal(t, "t1", info.ID)
	assert.Equal(t, "explore", info.Agent)
	assert.Equal(t, TaskCompleted, info.Status)
	assert.Equal(t, "the answer", info.Result)
	assert.False(t, info.Ended.IsZero())
	got, err := m.Output("t1")
	require.NoError(t, err)
	assert.Equal(t, info, got)
	_, ok = agent.End(TaskFailed, "again", "")
	assert.False(t, ok)
	assert.Empty(t, events)
}

func TestTasksAgentKeepsOutputAcrossRuns(t *testing.T) {
	m, _ := newTasks(t)
	agent := startAgent(m, "find it")
	_, err := agent.Write([]byte("first\n"))
	require.NoError(t, err)
	assert.False(t, agent.Run(func() {}), "a running task cannot run again")
	ended, ok := agent.End(TaskCompleted, "", "one")
	require.True(t, ok)

	require.True(t, agent.Run(func() { agent.End(TaskStopped, "", "") }))

	_, err = agent.Write([]byte("second\n"))
	require.NoError(t, err)
	info, err := m.Output("t1")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, info.Status)
	assert.True(t, info.Ended.IsZero())
	assert.Empty(t, info.Result)
	assert.Equal(t, "first\nsecond\n", info.Output)
	assert.False(t, info.Started.Before(ended.Started))
	assert.Equal(t, "t1", info.ID)
	assert.Equal(t, "t2", startAgent(m, "other").ID())
}

func TestTasksAgentStopWaitsForEnd(t *testing.T) {
	m, _ := newTasks(t)
	stopping, release := make(chan struct{}), make(chan struct{})
	var agent *AgentTask
	agent = m.StartAgent("explore", "find it", func() {
		close(stopping)
		<-release
		agent.End(TaskCompleted, "", "finished anyway")
	})
	type stopped struct {
		info TaskInfo
		err  error
	}
	result := make(chan stopped, 1)
	go func() {
		info, err := m.Stop("t1")
		result <- stopped{info, err}
	}()
	select {
	case <-stopping:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the stop func")
	}
	select {
	case <-result:
		t.Fatal("Stop returned before End")
	default:
	}

	close(release)

	select {
	case r := <-result:
		require.NoError(t, r.err)
		assert.Equal(t, TaskStopped, r.info.Status)
		assert.Empty(t, r.info.Result)
		assert.False(t, r.info.Ended.IsZero())
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for Stop")
	}
	again, err := m.Stop("t1")
	require.NoError(t, err)
	assert.Equal(t, TaskStopped, again.Status)
}

func TestTasksStopOfEndedAgentOnlyReturnsIt(t *testing.T) {
	m, _ := newTasks(t)
	calls := 0
	agent := m.StartAgent("explore", "find it", func() { calls++ })
	_, ok := agent.End(TaskCompleted, "", "done")
	require.True(t, ok)

	info, err := m.Stop("t1")

	require.NoError(t, err)
	assert.Equal(t, TaskCompleted, info.Status)
	assert.Equal(t, "done", info.Result)
	assert.Zero(t, calls)
}

func TestTasksStopAllForgetsAgentTask(t *testing.T) {
	m, events := newTasks(t)
	stopping := make(chan struct{})
	var agent *AgentTask
	reported := make(chan bool, 1)
	agent = m.StartAgent("explore", "find it", func() {
		close(stopping)
		_, ok := agent.End(TaskCompleted, "", "late")
		reported <- ok
	})

	m.StopAll()

	select {
	case <-stopping:
	default:
		t.Fatal("StopAll did not call the stop func")
	}
	assert.False(t, <-reported, "End after StopAll forgot the task")
	assert.False(t, agent.Run(func() {}))
	assert.Empty(t, m.List())
	_, err := m.Output("t1")
	require.ErrorIs(t, err, ErrUnknownTask)
	assert.Empty(t, events)
	assert.Equal(t, "t2", startAgent(m, "next").ID())
}

func TestTasksAdoptReportsEndedProcessAsRunning(t *testing.T) {
	m := NewTasks()
	p := &process{out: &outputBuffer{}, stopCh: make(chan error), done: make(chan struct{})}
	close(p.done)
	started := time.Now()
	tk := &task{command: "true", started: started, p: p}

	m.mu.Lock()
	info := m.adopt(tk)
	m.mu.Unlock()

	assert.Equal(t, "t1", info.ID)
	assert.Equal(t, TaskRunning, info.Status)
	assert.True(t, info.Ended.IsZero())
	assert.Empty(t, info.Reason)
	assert.Equal(t, started, info.Started)
	assert.Equal(t, TaskCompleted, tk.info().Status)
}
