//go:build darwin || linux

package pigruntime

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

func loginTestTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})
	return master, slave
}

type loginPromptOutput struct {
	mu      sync.Mutex
	text    strings.Builder
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *loginPromptOutput) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.ready)
		if w.release != nil {
			<-w.release
		}
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.Write(p)
}

func (w *loginPromptOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

func waitLoginPrompt(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal prompt")
	}
}

func waitLoginResult(t *testing.T, done <-chan readResult) readResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal input to stop")
		return readResult{}
	}
}

func TestLoginTerminalSecretPreCancelled(t *testing.T) {
	_, slave := loginTestTerminal(t)
	before, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	var out strings.Builder
	l := &loginIO{in: slave, lines: bufio.NewReader(slave), out: &out}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = l.prompt(ctx, ai.AuthSecretPrompt{Message: "Secret:"})

	require.ErrorIs(t, err, context.Canceled)
	after, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Empty(t, out.String())
	assert.Nil(t, l.pending)
}

func TestLoginTerminalSecretCancellation(t *testing.T) {
	for _, blockOutput := range []bool{false, true} {
		name := "waiting for input"
		if blockOutput {
			name = "writing prompt"
		}
		t.Run(name, func(t *testing.T) {
			master, slave := loginTestTerminal(t)
			before, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			out := &loginPromptOutput{ready: make(chan struct{})}
			if blockOutput {
				out.release = make(chan struct{})
			}
			l := &loginIO{in: slave, lines: bufio.NewReader(slave), out: out}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan readResult, 1)
			go func() {
				text, err := l.prompt(ctx, ai.AuthSecretPrompt{Message: "Secret:"})
				done <- readResult{text: text, err: err}
			}()
			waitLoginPrompt(t, out.ready)
			raw, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			assert.NotEqual(t, before, raw)
			cancel()
			if blockOutput {
				select {
				case <-done:
					t.Fatal("prompt returned before its writer stopped")
				default:
				}
				close(out.release)
			}
			res := waitLoginResult(t, done)
			require.ErrorIs(t, res.err, context.Canceled)
			after, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Nil(t, l.pending)
			printed := out.String()

			// A second prompt exercises the same caller-owned input after cancellation.
			nextOut := &loginPromptOutput{ready: make(chan struct{})}
			l.out = nextOut
			go func() {
				text, err := l.prompt(t.Context(), ai.AuthSecretPrompt{Message: "Again:"})
				done <- readResult{text: text, err: err}
			}()
			waitLoginPrompt(t, nextOut.ready)
			_, err = io.WriteString(master, "next-key\r")
			require.NoError(t, err)
			res = waitLoginResult(t, done)
			require.NoError(t, res.err)
			assert.Equal(t, "next-key", res.text)
			assert.Equal(t, printed, out.String(), "cancelled editor writes nothing later")
			after, err = term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestLoginTerminalSecretEditingAndAbort(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		wantErr           error
	}{
		{name: "hidden editing", input: "synthetic-keX\x7fy\r", want: "synthetic-key"},
		{name: "ctrl-c", input: "\x03", wantErr: io.EOF},
		{name: "ctrl-d", input: "\x04", wantErr: io.EOF},
		{name: "empty", input: "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := loginTestTerminal(t)
			before, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			out := &loginPromptOutput{ready: make(chan struct{})}
			l := &loginIO{in: slave, lines: bufio.NewReader(slave), out: out}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan readResult, 1)
			go func() {
				text, err := l.prompt(ctx, ai.AuthSecretPrompt{Message: "Secret:", Placeholder: "API key"})
				done <- readResult{text: text, err: err}
			}()
			waitLoginPrompt(t, out.ready)
			_, err = io.WriteString(master, tc.input)
			require.NoError(t, err)
			res := waitLoginResult(t, done)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, res.err, tc.wantErr)
			case tc.name == "empty":
				require.ErrorContains(t, res.err, "empty answer")
			default:
				require.NoError(t, res.err)
				assert.Equal(t, tc.want, res.text)
			}
			assert.Contains(t, out.String(), "Secret: [API key] ")
			assert.NotContains(t, out.String(), "synthetic")
			after, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
