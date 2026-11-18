package tui

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

func TestNewQueuesEventsBeforeRun(t *testing.T) {
	c := &fakeController{}
	f := New(c, strings.NewReader(""), &bytes.Buffer{}, testOptions())
	require.NotNil(t, c.subscriber)

	c.subscriber(event.SessionChanged{SessionID: "s1"})
	c.subscriber(event.Notice{Text: "hi"})

	assert.Equal(t, eventsMsg{event.SessionChanged{SessionID: "s1"}, event.Notice{Text: "hi"}}, f.bridge.wait()())
}

type quitController struct {
	fakeController
	ctx chan context.Context
}

func (c *quitController) Submit(ctx context.Context, _ string, _ ...string) (*app.Work, error) {
	c.ctx <- ctx
	return nil, app.ErrQuit
}

func TestRunNormalQuitCancelsOwnedContext(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &quitController{ctx: make(chan context.Context, 1)}
	f := New(c, strings.NewReader("/quit\r"), &lockedBuffer{}, testOptions())
	done := make(chan error, 1)
	go func() { done <- f.Run(parent, nil) }()
	var owned context.Context
	select {
	case owned = <-c.ctx:
	case <-time.After(waitLimit):
		t.Fatal("quit was not submitted")
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("Run did not return")
	}
	assert.ErrorIs(t, owned.Err(), context.Canceled)
	assert.NoError(t, parent.Err())
}

func TestExitHint(t *testing.T) {
	assert.Empty(t, exitHint(app.Status{SessionID: "s1", Model: "anthropic/claude-sonnet-4-5"}))
	for _, tt := range []struct {
		model string
		want  string
	}{
		{"anthropic/claude-sonnet-4-5", "'anthropic/claude-sonnet-4-5'"},
		{"custom/model name", "'custom/model name'"},
		{"custom/model's-name", "'custom/model'\"'\"'s-name'"},
		{"custom/$MODEL`id`$(id);&|<>*?\"name", "'custom/$MODEL`id`$(id);&|<>*?\"name'"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			assert.Equal(t, "Resume with: kopi -r s1 --model "+tt.want,
				exitHint(app.Status{SessionID: "s1", Model: tt.model, MessageCount: 2}))
		})
	}
}

// waitLimit only guards against a hung test; no test relies on it elapsing.
const waitLimit = 30 * time.Second

// lockedBuffer is the program's output; the renderer writes from its own
// goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// runFrontend runs a Frontend over in-memory input and output; the input is
// at EOF, which Bubble Tea treats as no more keys rather than an error.
func runFrontend(t *testing.T, c *fakeController, stop func(cancel context.CancelFunc, interrupts chan<- os.Signal)) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := &lockedBuffer{}
	f := New(c, strings.NewReader(""), out, testOptions())
	interrupts := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, interrupts) }()

	stop(cancel, interrupts)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("Run did not return")
	}
	return out.String()
}

func TestRunReturnsOnContextCancel(t *testing.T) {
	out := runFrontend(t, &fakeController{}, func(cancel context.CancelFunc, _ chan<- os.Signal) { cancel() })
	assert.NotContains(t, out, "Resume with")
}

func TestRunQuitsOnSecondInterruptAtIdleAndPrintsResumeHint(t *testing.T) {
	c := &fakeController{status: app.Status{SessionID: "s1", Model: "anthropic/claude-sonnet-4-5", MessageCount: 1}}
	out := runFrontend(t, c, func(_ context.CancelFunc, interrupts chan<- os.Signal) {
		interrupts <- os.Interrupt
		interrupts <- os.Interrupt
	})
	assert.True(t, strings.HasSuffix(out, "Resume with: kopi -r s1 --model 'anthropic/claude-sonnet-4-5'\n"), "output ends %q", out[max(0, len(out)-100):])
}
