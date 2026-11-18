//go:build darwin || linux

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/keyute/kopi/internal/pigruntime"
)

func TestLoginInterrupt(t *testing.T) {
	const childEnv = "KOPI_TEST_LOGIN_INTERRUPT"
	if os.Getenv(childEnv) == "1" {
		root := newRoot()
		root.SetIn(os.Stdin)
		root.SetOut(os.Stdout)
		root.SetErr(os.Stderr)
		root.SetArgs([]string{"login", "mistral"})
		require.ErrorIs(t, root.ExecuteContext(context.Background()), context.Canceled)
		return
	}

	e := newTestEnv(t)
	master, slave := openTestTerminal(t)
	t.Cleanup(func() { _ = master.Close() })
	t.Cleanup(func() { _ = slave.Close() })
	before, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	child := exec.Command(binary, "-test.run=^TestLoginInterrupt$")
	child.Dir = e.workspace
	child.Env = []string{
		childEnv + "=1",
		"HOME=" + e.home,
		"KOPI_HOME=" + e.kopiHome,
		"PATH=",
		"TERM=xterm-256color",
		"TMPDIR=" + e.root,
	}
	child.Stdin, child.Stdout = slave, slave
	var stderr bytes.Buffer
	child.Stderr = &stderr
	require.NoError(t, child.Start())
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = child.Wait()
		close(done)
	}()
	prompt := make(chan struct{})
	drained := make(chan struct{})
	var result terminalDrainResult
	go func() {
		var output strings.Builder
		buf := make([]byte, 4096)
		seen := false
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			if !seen && strings.Contains(output.String(), "Enter Mistral API key") {
				seen = true
				close(prompt)
			}
			if err != nil {
				result = terminalDrainResult{text: output.String(), err: err}
				close(drained)
				return
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = child.Process.Kill()
			select {
			case <-done:
			case <-time.After(waitLimit):
				t.Error("timed out cleaning up login subprocess")
				return
			}
		}
		// Close the last slave before joining the reader: master.Close can block on Darwin.
		_ = slave.Close()
		select {
		case <-drained:
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up login terminal drain")
		}
	})

	select {
	case <-prompt:
	case <-done:
		t.Fatalf("login exited before secret prompt: %v; stderr: %s", waitErr, stderr.String())
	case <-drained:
		t.Fatalf("terminal ended before secret prompt: %v; output: %q", result.err, result.text)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for login secret prompt")
	}
	raw, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	require.NotEqual(t, before, raw, "secret input must be pending in raw mode")
	// Signal only the child; a PTY Ctrl-C would exercise the editor, not signal.NotifyContext.
	require.NoError(t, child.Process.Signal(os.Interrupt))
	select {
	case <-done:
		require.NoError(t, waitErr, "stderr: %s", stderr.String())
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for interrupted login to return cancellation")
	}
	after, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	content, err := os.ReadFile(filepath.Join(e.agentDir, "auth.json"))
	require.NoError(t, err)
	var credentials map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(content, &credentials))
	assert.Empty(t, credentials)
	require.NoError(t, slave.Close())
	select {
	case <-drained:
		assert.Contains(t, result.text, "PASS")
		assert.NotContains(t, result.text, "Logged in to mistral.")
	case <-time.After(waitLimit):
		t.Fatal("timed out draining login terminal")
	}
}

func loginTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, slave := openTestTerminal(t)
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
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for terminal prompt")
	}
}

func waitLoginResult(t *testing.T, done <-chan readResult) readResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for terminal input to stop")
		return readResult{}
	}
}

func TestTerminalLoginSecretPreCancelled(t *testing.T) {
	_, slave := loginTerminal(t)
	before, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	var out strings.Builder
	l := &terminalLogin{in: slave, lines: bufio.NewReader(slave), out: &out}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = l.ask(ctx, pigruntime.LoginPrompt{Message: "Secret:", Secret: true})

	require.ErrorIs(t, err, context.Canceled)
	after, err := term.GetState(int(slave.Fd()))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Empty(t, out.String())
	assert.Nil(t, l.pending)
}

func TestTerminalLoginSecretCancellation(t *testing.T) {
	for _, blockOutput := range []bool{false, true} {
		name := "waiting for input"
		if blockOutput {
			name = "writing prompt"
		}
		t.Run(name, func(t *testing.T) {
			master, slave := loginTerminal(t)
			before, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			out := &loginPromptOutput{ready: make(chan struct{})}
			if blockOutput {
				out.release = make(chan struct{})
			}
			l := &terminalLogin{in: slave, lines: bufio.NewReader(slave), out: out}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan readResult, 1)
			go func() {
				text, err := l.ask(ctx, pigruntime.LoginPrompt{Message: "Secret:", Secret: true})
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
				text, err := l.ask(t.Context(), pigruntime.LoginPrompt{Message: "Again:", Secret: true})
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

func TestTerminalLoginSecretEditingAndAbort(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		wantErr           error
	}{
		{name: "hidden editing", input: "synthetic-keX\x7fy\r", want: "synthetic-key"},
		{name: "ctrl-c", input: "\x03", wantErr: context.Canceled},
		{name: "ctrl-d", input: "\x04", wantErr: context.Canceled},
		{name: "empty", input: "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := loginTerminal(t)
			before, err := term.GetState(int(slave.Fd()))
			require.NoError(t, err)
			out := &loginPromptOutput{ready: make(chan struct{})}
			l := &terminalLogin{in: slave, lines: bufio.NewReader(slave), out: out}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan readResult, 1)
			go func() {
				text, err := l.ask(ctx, pigruntime.LoginPrompt{Message: "Secret:", Placeholder: "API key", Secret: true})
				done <- readResult{text: text, err: err}
			}()
			waitLoginPrompt(t, out.ready)
			_, err = io.WriteString(master, tc.input)
			require.NoError(t, err)
			res := waitLoginResult(t, done)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, res.err, tc.wantErr)
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
