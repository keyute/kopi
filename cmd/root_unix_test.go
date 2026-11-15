//go:build darwin || linux

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/history"
)

func openTestTerminal(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	require.NoError(t, err)
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		_ = slave.Close()
		_ = master.Close()
		require.NoError(t, err)
	}
	return master, slave
}

type terminalDrainResult struct {
	text string
	err  error
}

func TestRootTerminalDispatch(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		terminalIn, terminalOut bool
		printOnly               bool
	}{
		{"both terminals", true, true, false},
		{"redirected stdin", false, true, false},
		{"redirected stdout", true, false, false},
		{"both redirected", false, false, false},
		{"print with both terminals", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			logger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(logger) })
			master, slave := openTestTerminal(t)
			root := newRoot()
			var out, stderr bytes.Buffer
			root.SetIn(strings.NewReader("piped context"))
			root.SetOut(&out)
			root.SetErr(&stderr)
			if tc.terminalIn {
				root.SetIn(slave)
			}
			var drained chan struct{}
			var result terminalDrainResult
			t.Cleanup(func() {
				_ = slave.Close()
				if drained != nil {
					select {
					case <-drained:
						assert.True(t, errors.Is(result.err, io.EOF) || runtime.GOOS == "linux" && errors.Is(result.err, syscall.EIO), "terminal read: %v; output: %q", result.err, result.text)
					case <-time.After(waitLimit):
						t.Error("timed out cleaning up terminal drain")
						return
					}
				}
				_ = master.Close()
			})
			responseRead := make(chan struct{})
			if tc.terminalOut {
				root.SetOut(slave)
				drained = make(chan struct{})
				go func() {
					var b strings.Builder
					buf := make([]byte, 4096)
					seen := false
					for {
						n, err := master.Read(buf)
						b.Write(buf[:n])
						if !seen && strings.Contains(b.String(), "dispatch-response") {
							seen = true
							close(responseRead)
						}
						if err != nil {
							result = terminalDrainResult{text: b.String(), err: err}
							close(drained)
							return
						}
					}
				}()
			}
			fullscreen := tc.terminalIn && tc.terminalOut && !tc.printOnly
			if !fullscreen {
				server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "dispatch-response") })
				e.writeMockModels(t, server.URL)
				defer func() {
					want := "matrix prompt"
					if !tc.terminalIn {
						want = "piped context\n\nmatrix prompt"
					}
					assert.Equal(t, want, lastUserText(server.next(t).body))
				}()
			}
			args := []string{"--model", mockModel}
			if tc.printOnly {
				args = append(args, "-p")
			}
			root.SetArgs(append(args, "matrix prompt"))
			err := root.ExecuteContext(t.Context())
			if fullscreen {
				require.EqualError(t, err, `unexpected argument "matrix prompt": pass -p to print the response to a prompt`)
			} else {
				require.NoError(t, err)
			}
			if drained != nil {
				if !fullscreen {
					select {
					case <-responseRead:
					case <-drained:
						t.Fatalf("terminal drain ended before response: %v; output: %q", result.err, result.text)
					case <-time.After(waitLimit):
						t.Fatal("timed out reading headless terminal response")
					}
				}
				select {
				case <-drained:
					t.Fatalf("terminal drain ended before slave closure: %v; output: %q", result.err, result.text)
				default:
				}
				require.NoError(t, slave.Close())
				select {
				case <-drained:
					out.WriteString(result.text)
				case <-time.After(waitLimit):
					t.Fatal("timed out draining terminal")
				}
			}
			if !fullscreen {
				assert.Contains(t, out.String(), "dispatch-response")
			}
			assert.NoFileExists(t, filepath.Join(e.kopiHome, "history.jsonl"))
		})
	}
}

func TestRootFullscreenStartupSubmitExit(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("TERM", "xterm-256color")
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	respond := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := newMockProvider(t, func(w http.ResponseWriter) {
		select {
		case <-respond:
			streamText(w, "fullscreen-response")
		case <-ctx.Done():
		}
	})
	e.writeMockModels(t, server.URL)
	master, slave := openTestTerminal(t)
	ready, answered, resumed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	drained := make(chan struct{})
	var result terminalDrainResult
	go func() {
		var output strings.Builder
		buf := make([]byte, 4096)
		var sawReady, sawAnswer, sawResume bool
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			plain := ansi.Strip(output.String())
			if !sawReady && strings.Contains(plain, "kopi") {
				sawReady = true
				close(ready)
			}
			if !sawAnswer && strings.Contains(plain, "fullscreen-response") {
				sawAnswer = true
				close(answered)
			}
			if !sawResume && strings.Contains(plain, "Resume with:") {
				sawResume = true
				close(resumed)
			}
			if err != nil {
				result = terminalDrainResult{text: output.String(), err: err}
				close(drained)
				return
			}
		}
	}()
	root := newRoot()
	root.SetIn(slave)
	root.SetOut(slave)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetArgs([]string{"--model", mockModel})
	var beforeFullscreen *slog.Logger
	preRun := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := preRun(cmd, args); err != nil {
			return err
		}
		beforeFullscreen = slog.Default()
		return nil
	}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- root.ExecuteContext(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up fullscreen command")
			return
		}
		// Close the last slave first: closing a master with a blocked read can hang on Darwin.
		_ = slave.Close()
		select {
		case <-drained:
			assert.True(t, errors.Is(result.err, io.EOF) || runtime.GOOS == "linux" && errors.Is(result.err, syscall.EIO), "terminal read: %v; output: %q", result.err, result.text)
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up fullscreen terminal drain")
			return
		}
		_ = master.Close()
	})
	select {
	case <-ready:
	case <-drained:
		t.Fatalf("terminal drain ended before readiness: %v; output: %q", result.err, result.text)
	case err := <-done:
		t.Fatalf("fullscreen exited before readiness: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for fullscreen readiness")
	}
	_, err := io.WriteString(master, "fullscreen prompt\r")
	require.NoError(t, err)
	assert.Equal(t, "fullscreen prompt", lastUserText(server.next(t).body))
	slog.Warn("fullscreen-log-marker")
	close(respond)
	select {
	case <-answered:
	case <-drained:
		t.Fatalf("terminal drain ended before rendered response: %v; output: %q", result.err, result.text)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for rendered response")
	}
	_, err = master.Write([]byte{4}) // ctrl-d quits with an empty composer.
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for fullscreen exit")
	}
	assert.Same(t, beforeFullscreen, slog.Default())
	slog.Warn("restored-log-marker")
	assert.NotContains(t, stderr.String(), "fullscreen-log-marker")
	assert.Contains(t, stderr.String(), "restored-log-marker")
	log, err := os.ReadFile(filepath.Join(e.kopiHome, "kopi.log"))
	require.NoError(t, err)
	assert.Contains(t, string(log), "fullscreen-log-marker")
	assert.NotContains(t, string(log), "restored-log-marker")
	store, err := history.Open(filepath.Join(e.kopiHome, "history.jsonl"), e.workspace, historyLimit)
	require.NoError(t, err)
	assert.Equal(t, []string{"fullscreen prompt"}, store.Texts())
	select {
	case <-resumed:
	case <-drained:
		t.Fatalf("terminal drain ended before resume hint: %v; output: %q", result.err, result.text)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for resume hint")
	}
	select {
	case <-drained:
		t.Fatalf("terminal drain ended before slave closure: %v; output: %q", result.err, result.text)
	default:
	}
	require.NoError(t, slave.Close())
	select {
	case <-drained:
		assert.Contains(t, result.text, "Resume with:")
		assert.NotContains(t, result.text, syntheticKey)
	case <-time.After(waitLimit):
		t.Fatal("timed out draining fullscreen terminal")
	}
}
