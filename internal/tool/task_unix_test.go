//go:build unix

package tool

import (
	"context"
	"os"
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
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)

	out, moved, err := m.Run(t.Context(), sh, command, time.Millisecond)

	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Equal(t, Moved{ID: "t1", TimedOut: true}, moved)
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	writeLine(t, openWriter(t, fifo), "ping\n")
	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "ping\n", end.Output)
}

func TestTasksDetachMovesCommand(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	m, events := newTasks(t)
	command, fifo := echoFIFO(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := runAsync(ctx, m, sh, command)
	f := openWriter(t, fifo)

	assert.True(t, m.Foreground())
	assert.True(t, m.Detach())
	r := waitResult(t, result)

	require.NoError(t, r.err)
	assert.Empty(t, r.out)
	assert.Equal(t, Moved{ID: "t1"}, r.moved)
	assert.False(t, m.Foreground())
	assert.False(t, m.Detach())
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	// A cancel of the original call no longer reaches the moved command.
	cancel()
	writeLine(t, f, "hi\n")
	end := nextEvent(t, events)
	assert.Equal(t, TaskCompleted, end.Status)
	assert.Equal(t, "hi\n", end.Output)
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

func TestTasksStopKillsGroup(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell(dir, nil)
	m, events := newTasks(t)
	command, started, dead := holdFIFO(t, dir)
	_, err := m.Start(t.Context(), sh, command)
	require.NoError(t, err)
	waitFor(t, started, "the grandchild to start")

	info, err := m.Stop("t1")

	require.NoError(t, err)
	assert.Equal(t, TaskStopped, info.Status)
	assert.Empty(t, info.Reason)
	assert.False(t, info.Ended.IsZero())
	waitFor(t, dead, "the grandchild to die")
	assert.Equal(t, TaskRunning, nextEvent(t, events).Status)
	assert.Equal(t, info, nextEvent(t, events))
	again, err := m.Stop("t1")
	require.NoError(t, err)
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
		if !m.Foreground() {
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

// foregroundSignals records Foreground as each foreground signal observes it.
func foregroundSignals(m *Tasks) <-chan bool {
	ch := make(chan bool, 16)
	m.OnForeground(func() { ch <- m.Foreground() })
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
