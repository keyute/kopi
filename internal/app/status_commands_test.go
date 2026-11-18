package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

type runResult struct {
	out string
	err error
}

type fakeRunner struct {
	release chan runResult
	mu      sync.Mutex
	calls   []string
}

func (r *fakeRunner) run(_ context.Context, dir, command string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, dir+": "+command)
	r.mu.Unlock()
	res := <-r.release
	return res.out, res.err
}

func (r *fakeRunner) started() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type contextRunner struct{ started chan context.Context }

func (r *contextRunner) run(ctx context.Context, _, _ string) (string, error) {
	r.started <- ctx
	<-ctx.Done()
	return "", ctx.Err()
}

func newTestStatusCommands(segment config.StatusSegment) (*StatusCommands, *fakeRunner, chan event.StatusCommandResult) {
	s := NewStatusCommands("/work", config.StatusLine{Left: []string{"git", "git", "mode"}, Right: []string{"git"}, Segments: map[string]config.StatusSegment{"git": segment, "unused": {Command: "never"}, "mode": {Command: "never"}}})
	r := &fakeRunner{release: make(chan runResult)}
	s.runner = r
	return s, r, make(chan event.StatusCommandResult, 16)
}

func startStatusCommands(t *testing.T, s *StatusCommands, results chan event.StatusCommandResult) {
	t.Helper()
	s.Start(t.Context(), func(ev event.Event) { results <- ev.(event.StatusCommandResult) })
	t.Cleanup(s.Close)
}

func TestStatusCommandsStartAndRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r, results := newTestStatusCommands(config.StatusSegment{Command: "git-info"})
		s.Observe(event.TurnEnd{})
		synctest.Wait()
		assert.Empty(t, r.started(), "construction and events before Start do not execute commands")
		startStatusCommands(t, s, results)
		synctest.Wait()
		assert.Equal(t, []string{"/work: git-info"}, r.started(), "selected commands are deduplicated")
		s.Observe(event.TurnEnd{})
		synctest.Wait()
		assert.Len(t, r.started(), 1, "a run is outstanding")
		r.release <- runResult{out: "raw\nsecond line"}
		assert.Equal(t, "raw\nsecond line", (<-results).Output)
		synctest.Wait()
		assert.Len(t, r.started(), 1, "busy refresh does not queue catchup")
		for i, ev := range []event.Event{event.SessionChanged{}, event.ModeChanged{}, event.ToolEnd{}, event.TurnEnd{}, event.TasksChanged{}} {
			s.Observe(event.Notice{})
			s.Observe(event.TextDelta{})
			synctest.Wait()
			assert.Len(t, r.started(), i+1, "unrelated events do not refresh")
			s.Observe(ev)
			synctest.Wait()
			assert.Len(t, r.started(), i+2)
			r.release <- runResult{out: "ok"}
			assert.NoError(t, (<-results).Err)
		}
	})
}

func TestStatusCommandsDeadlineRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, results := newTestStatusCommands(config.StatusSegment{Command: "blocked"})
		r := &contextRunner{started: make(chan context.Context, 1)}
		s.runner = r
		start := time.Now()
		startStatusCommands(t, s, results)
		ctx := <-r.started
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.Equal(t, start.Add(5*time.Second), deadline)
		assert.ErrorIs(t, (<-results).Err, context.DeadlineExceeded)
		assert.Equal(t, 5*time.Second, time.Since(start))
		s.mu.Lock()
		assert.False(t, s.commands[0].running)
		assert.True(t, s.commands[0].failing)
		s.mu.Unlock()
		s.Observe(event.TurnEnd{})
		<-r.started
		assert.ErrorIs(t, (<-results).Err, context.DeadlineExceeded)
		s.mu.Lock()
		assert.False(t, s.commands[0].running)
		s.mu.Unlock()
	})
}

func TestStatusCommandsParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, results := newTestStatusCommands(config.StatusSegment{Command: "blocked"})
		r := &contextRunner{started: make(chan context.Context, 1)}
		s.runner = r
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.Start(ctx, func(ev event.Event) { results <- ev.(event.StatusCommandResult) })
		defer s.Close()
		<-r.started
		cancel()
		assert.ErrorIs(t, (<-results).Err, context.Canceled)
		s.Observe(event.TurnEnd{})
		synctest.Wait()
		assert.Empty(t, r.started)
	})
}

func TestStatusCommandsIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r, results := newTestStatusCommands(config.StatusSegment{Command: "date", Interval: "1s"})
		assert.Equal(t, time.Second, s.commands[0].interval)
		startStatusCommands(t, s, results)
		synctest.Wait()
		<-time.After(2 * time.Second)
		synctest.Wait()
		assert.Len(t, r.started(), 1, "ticks skip busy commands")
		r.release <- runResult{out: "one"}
		<-results
		synctest.Wait()
		assert.Len(t, r.started(), 1, "no catchup")
		for i := 2; i <= 3; i++ {
			<-time.After(time.Second)
			synctest.Wait()
			assert.Len(t, r.started(), i, "interval re-arms")
			r.release <- runResult{out: "two"}
			<-results
		}
	})
}

func TestStatusCommandsFailureTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		defer slog.SetDefault(previous)
		s, r, results := newTestStatusCommands(config.StatusSegment{Command: "git-info"})
		startStatusCommands(t, s, results)
		for i, res := range []runResult{{err: errors.New("failed")}, {err: errors.New("again")}, {out: "back"}, {err: errors.New("failed")}} {
			if i > 0 {
				s.Observe(event.TurnEnd{})
			}
			synctest.Wait()
			r.release <- res
			result := <-results
			assert.Equal(t, res.err, result.Err)
			s.mu.Lock()
			assert.Equal(t, res.err != nil, s.commands[0].failing)
			s.mu.Unlock()
		}
		synctest.Wait()
		assert.Equal(t, 2, strings.Count(logs.String(), "status line command failed"))
	})
}

func TestStatusCommandsCloseRejectsLateLaunchAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r, results := newTestStatusCommands(config.StatusSegment{Command: "held"})
		startStatusCommands(t, s, results)
		synctest.Wait()
		done := make(chan struct{})
		go func() { s.Close(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Close did not join execution")
		default:
		}
		s.Observe(event.TurnEnd{})
		s.Start(t.Context(), func(event.Event) { t.Error("late Start") })
		r.release <- runResult{}
		<-done
		s.Observe(event.TurnEnd{})
		synctest.Wait()
		assert.Len(t, r.started(), 1)
		s.Close()
	})
	s, r, _ := newTestStatusCommands(config.StatusSegment{Command: "never"})
	s.Close()
	s.Start(t.Context(), func(event.Event) { t.Error("closed Start") })
	assert.Empty(t, r.started(), "an unscheduled command must not hold shutdown open")
}

func TestStatusCommandsRejectsQueuedExecutionAfterClose(t *testing.T) {
	s, r, _ := newTestStatusCommands(config.StatusSegment{Command: "never"})
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(t.Context())
	s.publish = func(event.Event) { t.Error("closed execution published a result") }
	s.launch(s.commands[0])
	// Hold admission until the queued goroutine exists, then close it before
	// that goroutine can enter execution.
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.Close()
	assert.Empty(t, r.started())
}
