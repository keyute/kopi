//go:build unix

package tool

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type runResult struct {
	out   string
	moved Moved
	err   error
}

func runAsync(ctx context.Context, m *Tasks, sh *Shell, command string) <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		out, moved, err := m.Run(ctx, sh, command, testTimeout)
		ch <- runResult{out, moved, err}
	}()
	return ch
}

func waitResult(t *testing.T, ch <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Run to return")
		return runResult{}
	}
}

// echoFIFO returns a command that prints a line read from a FIFO, and the
// FIFO's path.
func echoFIFO(t *testing.T) (command, fifo string) {
	t.Helper()
	fifo = mkfifo(t, t.TempDir(), "in")
	return "read x <'" + fifo + "'; echo \"$x\"", fifo
}

// openWriter blocks until the command has opened the FIFO for reading, which
// proves it is running.
func openWriter(t *testing.T, fifo string) *os.File {
	t.Helper()
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func writeLine(t *testing.T, f *os.File, line string) {
	t.Helper()
	_, err := f.WriteString(line)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestTasksTimeoutMovesCommand(t *testing.T) {
	outputs, dir := newSavedOutputs(t)
	sh := NewShell(t.TempDir(), nil).WithOutputs(outputs)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)

	out, moved, err := m.Run(t.Context(), sh, command, time.Millisecond)

	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Equal(t, dir, filepath.Dir(moved.OutputFile))
	assert.Equal(t, Moved{ID: "t1", TimedOut: true, OutputFile: moved.OutputFile}, moved)
	running := nextEvent(t, events)
	assert.Equal(t, TaskRunning, running.Status)
	assert.Equal(t, moved.OutputFile, running.OutputFile)
	writeLine(t, openWriter(t, fifo), "ping\n")
	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "ping\n", end.Output)
	data, err := os.ReadFile(moved.OutputFile)
	require.NoError(t, err)
	assert.Equal(t, "ping\n", string(data))
}

func TestTasksDetachMovesCommand(t *testing.T) {
	outputs, dir := newSavedOutputs(t)
	sh := NewShell(t.TempDir(), nil).WithOutputs(outputs)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := runAsync(ctx, m, sh, command)
	f := openWriter(t, fifo)

	assert.False(t, m.ForegroundSince().IsZero())
	assert.True(t, m.Detach())
	r := waitResult(t, result)

	require.NoError(t, r.err)
	assert.Empty(t, r.out)
	assert.Equal(t, dir, filepath.Dir(r.moved.OutputFile))
	assert.Equal(t, Moved{ID: "t1", OutputFile: r.moved.OutputFile}, r.moved)
	assert.True(t, m.ForegroundSince().IsZero())
	assert.False(t, m.Detach())
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	// A cancel of the original call no longer reaches the moved command.
	cancel()
	writeLine(t, f, "hi\n")
	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "hi\n", end.Output)
}

// waitAsync runs l.Wait without a timeout in the background.
func waitAsync(ctx context.Context, l *Live) <-chan Moved {
	ch := make(chan Moved, 1)
	go func() { ch <- l.Wait(ctx, 0) }()
	return ch
}

func waitMoved(t *testing.T, ch <-chan Moved) Moved {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Wait to return")
		return Moved{}
	}
}

func TestLiveWaitEndsInForeground(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	l, err := m.Begin(t.Context(), sh, command)
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	writeLine(t, openWriter(t, fifo), "ok\n")

	assert.Equal(t, Moved{}, waitMoved(t, done))

	info := l.Info()
	assert.Equal(t, TaskCompleted, info.Status)
	assert.Equal(t, "exit code 0", info.Reason)
	assert.Equal(t, command, info.Command)
	assert.Equal(t, "ok\n", info.Output)
	assert.False(t, info.Ended.IsZero())
	assert.True(t, m.ForegroundSince().IsZero())

	failed, err := m.Begin(t.Context(), sh, "exit 3")
	require.NoError(t, err)
	assert.Equal(t, Moved{}, failed.Wait(t.Context(), 0))
	assert.Equal(t, TaskFailed, failed.Info().Status)
	assert.Equal(t, "exit code 3", failed.Info().Reason)
	assert.Empty(t, events)
}

func TestForegroundSinceIsTheEarliestLiveStart(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, _ := newTasks(t)
	firstCommand, firstFIFO := echoFIFO(t)
	first, err := m.Begin(t.Context(), sh, firstCommand)
	require.NoError(t, err)
	firstDone := waitAsync(t.Context(), first)
	firstSince := m.ForegroundSince()
	assert.False(t, firstSince.IsZero())

	secondCommand, secondFIFO := echoFIFO(t)
	second, err := m.Begin(t.Context(), sh, secondCommand)
	require.NoError(t, err)
	secondDone := waitAsync(t.Context(), second)
	assert.Equal(t, firstSince, m.ForegroundSince(), "a later command leaves the earliest start")

	writeLine(t, openWriter(t, firstFIFO), "a\n")
	assert.Equal(t, Moved{}, waitMoved(t, firstDone))
	assert.True(t, m.ForegroundSince().After(firstSince), "the start moves to the command still live")

	writeLine(t, openWriter(t, secondFIFO), "b\n")
	assert.Equal(t, Moved{}, waitMoved(t, secondDone))
	assert.True(t, m.ForegroundSince().IsZero())
}

func TestLiveOutputWhileRunning(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell(dir, nil)
	m, _ := newTasks(t)
	fifo := mkfifo(t, dir, "in")
	l, err := m.Begin(t.Context(), sh, "echo first; read x <'"+fifo+"'")
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	require.Eventually(t, func() bool { return l.Output() == "first\n" }, waitLimit, time.Millisecond)

	writeLine(t, openWriter(t, fifo), "go\n")

	assert.Equal(t, Moved{}, waitMoved(t, done))
}

func TestLiveStop(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	l, err := m.Begin(t.Context(), sh, command)
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	openWriter(t, fifo)

	l.Stop()
	l.Stop()

	assert.Equal(t, Moved{}, waitMoved(t, done))
	assert.Equal(t, TaskStopped, l.Info().Status)
	l.Stop()
	assert.Empty(t, events)
	assert.Empty(t, m.List())
}

func TestLiveDetachMovesCommand(t *testing.T) {
	outputs, dir := newSavedOutputs(t)
	sh := NewShell(t.TempDir(), nil).WithOutputs(outputs)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	l, err := m.Begin(t.Context(), sh, command)
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	f := openWriter(t, fifo)

	assert.True(t, m.Detach())
	moved := waitMoved(t, done)

	assert.Equal(t, Moved{ID: "t1", OutputFile: moved.OutputFile}, moved)
	assert.Equal(t, dir, filepath.Dir(moved.OutputFile))
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	writeLine(t, f, "hi\n")
	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "hi\n", end.Output)
}

func TestLiveStopAfterMoveLeavesTask(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	l, err := m.Begin(t.Context(), sh, command)
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	f := openWriter(t, fifo)

	assert.True(t, m.Detach())
	waitMoved(t, done)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	l.Stop()
	writeLine(t, f, "hi\n")

	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "hi\n", end.Output)
}

func TestLiveStopAllEndsWait(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, _ := newTasks(t)
	command, started, dead := holdFIFO(t, t.TempDir())
	l, err := m.Begin(t.Context(), sh, command)
	require.NoError(t, err)
	done := waitAsync(t.Context(), l)
	waitFor(t, started, "the grandchild to start")

	m.StopAll()

	assert.Equal(t, Moved{}, waitMoved(t, done))
	assert.Equal(t, TaskStopped, l.Info().Status)
	waitFor(t, dead, "the grandchild to die")
}

func TestTasksCancelKillsForegroundGroup(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell(dir, nil)
	m, events := newTasks(t)
	command, started, dead := holdFIFO(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	out, moved, err := m.Run(ctx, sh, command, testTimeout)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, context.Canceled.Error(), err.Error())
	assert.Empty(t, out)
	assert.Equal(t, Moved{}, moved)
	waitFor(t, dead, "the grandchild to die")
	assert.Empty(t, events)
	assert.Empty(t, m.List())
}

type cancelCleanupContext struct {
	context.Context
	entered, release chan struct{}
}

func (c *cancelCleanupContext) Err() error {
	err := c.Context.Err()
	if err != nil {
		close(c.entered)
		<-c.release
	}
	return err
}

func TestTasksStopAllOwnsCancellationCleanup(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	signals := foregroundSignals(m)
	command, started, dead := holdFIFO(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cleanup := &cancelCleanupContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(cleanup.release) })
	defer release()
	result := runAsync(cleanup, m, sh, command)
	waitFor(t, started, "the grandchild to start")
	assert.True(t, nextSignal(t, signals))
	m.mu.Lock()
	var p *process
	for task := range m.fg {
		p = task.p
	}
	m.mu.Unlock()
	require.NotNil(t, p)
	cancel()
	// Pause cancellation immediately before Run requests the process stop.
	waitFor(t, cleanup.entered, "Run to enter cancellation cleanup")
	stopped := make(chan struct{})
	go func() {
		m.StopAll()
		close(stopped)
	}()
	waitFor(t, stopped, "StopAll to finish cancellation cleanup")
	assert.True(t, ended(p), "StopAll must wait for the cancelling process")
	assert.False(t, m.ForegroundSince().IsZero(), "Run retains ownership until cleanup finishes")
	release()
	r := waitResult(t, result)
	waitFor(t, dead, "the grandchild to die")
	require.ErrorIs(t, r.err, ErrStopped)
	assert.Equal(t, Moved{}, r.moved)
	assert.False(t, nextSignal(t, signals))
	assert.Empty(t, signals)
	assert.Empty(t, events)
	assert.Empty(t, m.List())
}

func TestTasksStopKillsGroup(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell(dir, nil)
	m, events := newTasks(t)
	command, started, dead := holdFIFO(t, dir)
	_, err := m.Start(t.Context(), sh, command)
	require.NoError(t, err)
	waitFor(t, started, "the grandchild to start")

	info, stopped, err := m.Stop("t1")

	require.NoError(t, err)
	assert.True(t, stopped)
	assert.Equal(t, TaskStopped, info.Status)
	assert.Empty(t, info.Reason)
	assert.False(t, info.Ended.IsZero())
	waitFor(t, dead, "the grandchild to die")
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, info, nextEvent(t, events))
	again, stopped, err := m.Stop("t1")
	require.NoError(t, err)
	assert.False(t, stopped)
	assert.Equal(t, info, again)
}

func TestTasksStopAllKillsEveryCommand(t *testing.T) {
	bgDir, fgDir := t.TempDir(), t.TempDir()
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	bgCommand, bgStarted, bgDead := holdFIFO(t, bgDir)
	fgCommand, fgStarted, fgDead := holdFIFO(t, fgDir)
	_, err := m.Start(t.Context(), sh, bgCommand)
	require.NoError(t, err)
	result := runAsync(t.Context(), m, sh, fgCommand)
	waitFor(t, bgStarted, "the background grandchild to start")
	waitFor(t, fgStarted, "the foreground grandchild to start")

	m.StopAll()

	r := waitResult(t, result)
	require.ErrorIs(t, r.err, ErrStopped)
	assert.Equal(t, Moved{}, r.moved)
	waitFor(t, bgDead, "the background grandchild to die")
	waitFor(t, fgDead, "the foreground grandchild to die")
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Empty(t, events)
	assert.Empty(t, m.List())
}

func TestTasksStopAllOwnsPromotion(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, started, dead := holdFIFO(t, t.TempDir())
	leaving, release := make(chan struct{}), make(chan struct{})
	m.OnForeground(func() {
		if m.ForegroundSince().IsZero() {
			close(leaving)
			<-release
		}
	})
	result := runAsync(t.Context(), m, sh, command)
	waitFor(t, started, "the grandchild to start")
	require.True(t, m.Detach())
	waitFor(t, leaving, "the foreground exit signal")
	stopped := make(chan struct{})
	go func() {
		m.StopAll()
		close(stopped)
	}()

	// Release the deliberately paused callback while StopAll drains the
	// background watch; waiting for StopAll first would deadlock.
	select {
	case <-dead:
	case <-stopped:
		t.Error("StopAll returned while the promoted command was still live")
	case <-time.After(waitLimit):
		t.Error("timed out waiting for the promoted process group to die")
	}
	close(release)
	r := waitResult(t, result)
	require.NoError(t, r.err)
	assert.Equal(t, Moved{ID: "t1"}, r.moved)
	waitFor(t, stopped, "StopAll to drain the watch")
	assert.Empty(t, m.List())
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Empty(t, events, "a forgotten task must not report its end")
}

// foregroundSignals records whether a foreground command runs as each foreground signal observes it.
func foregroundSignals(m *Tasks) <-chan bool {
	ch := make(chan bool, 16)
	m.OnForeground(func() { ch <- !m.ForegroundSince().IsZero() })
	return ch
}

func nextSignal(t *testing.T, signals <-chan bool) bool {
	t.Helper()
	select {
	case fg := <-signals:
		return fg
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a foreground signal")
		return false
	}
}

func TestTasksSignalForegroundChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, m *Tasks, f *os.File, cancel context.CancelFunc)
	}{
		{"ends", func(t *testing.T, _ *Tasks, f *os.File, _ context.CancelFunc) { writeLine(t, f, "hi\n") }},
		{"detached", func(_ *testing.T, m *Tasks, _ *os.File, _ context.CancelFunc) { m.Detach() }},
		{"cancelled", func(_ *testing.T, _ *Tasks, _ *os.File, cancel context.CancelFunc) { cancel() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := NewShell(t.TempDir(), nil)
			m, _ := newTasks(t)
			signals := foregroundSignals(m)
			command, fifo := echoFIFO(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := runAsync(ctx, m, sh, command)
			f := openWriter(t, fifo)

			assert.True(t, nextSignal(t, signals), "the signal comes once the command is live")
			tc.end(t, m, f, cancel)
			waitResult(t, result)
			assert.False(t, nextSignal(t, signals))
			assert.Empty(t, signals)
		})
	}
}

func TestTasksStartSignalsNoForeground(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	signals := foregroundSignals(m)

	_, err := m.Start(t.Context(), sh, "true")
	require.NoError(t, err)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, TaskCompleted, nextEvent(t, events).Status)
	assert.Empty(t, signals)
}

func TestTasksStopAllStopsShellAndAgentTogether(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, started, dead := holdFIFO(t, t.TempDir())
	_, err := m.Start(t.Context(), sh, command)
	require.NoError(t, err)
	waitFor(t, started, "the grandchild to start")
	var agent *AgentTask
	agentStopped := make(chan struct{})
	agent = m.StartAgent("explore", "find it", func() {
		go func() {
			agent.End(TaskStopped, "", "")
			close(agentStopped)
		}()
	})

	m.StopAll()

	waitFor(t, dead, "the grandchild to die")
	waitFor(t, agentStopped, "the agent task to end")
	assert.Empty(t, m.List())
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Empty(t, events)
}
