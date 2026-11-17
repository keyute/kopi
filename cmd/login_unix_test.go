//go:build darwin || linux

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
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
