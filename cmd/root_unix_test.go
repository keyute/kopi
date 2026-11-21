//go:build darwin || linux

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
)

func TestRootSIGINTDuringMCPStartup(t *testing.T) {
	const helperEnv = "KOPI_STARTUP_INTERRUPT_HELPER"
	switch os.Getenv(helperEnv) {
	case "cli":
		os.Args = []string{"kopi", "--model", mockModel, "-p", "unused prompt"}
		Execute()
		os.Exit(0)
	case "server":
		var request struct{ Method string }
		require.NoError(t, json.NewDecoder(os.Stdin).Decode(&request))
		require.Equal(t, "initialize", request.Method)
		ready, err := os.OpenFile(os.Getenv("KOPI_STARTUP_READY"), os.O_WRONLY, 0)
		require.NoError(t, err)
		require.NoError(t, json.NewEncoder(ready).Encode(os.Getpid()))
		require.NoError(t, ready.Close())
		// Never answer initialization or exit on stdin closure: Kopi must stop us.
		hang := make(chan os.Signal, 1)
		signal.Notify(hang, syscall.SIGUSR1)
		<-hang
		os.Exit(0)
	}

	e := newTestEnv(t)
	binary, err := os.Executable()
	require.NoError(t, err)
	fifo := filepath.Join(e.root, "ready")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	ready, err := os.OpenFile(fifo, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ready.Close() })
	e.writeMockModels(t, "http://unused.invalid")
	e.writeServers(t, filepath.Join(e.kopiHome, "config.json"), map[string]any{"hanging": map[string]any{
		"command": binary,
		"args":    []string{"-test.run=^TestRootSIGINTDuringMCPStartup$"},
		"env":     map[string]string{helperEnv: "server", "KOPI_STARTUP_READY": fifo},
	}})

	cli := exec.Command(binary, "-test.run=^TestRootSIGINTDuringMCPStartup$")
	cli.Env = []string{"HOME=" + e.home, "KOPI_HOME=" + e.kopiHome, "PATH=", helperEnv + "=cli"}
	cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cli.Stdout, cli.Stderr = &output, &output
	require.NoError(t, cli.Start())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- cli.Wait()
		close(stopped)
	}()
	serverPID := 0
	t.Cleanup(func() {
		if serverPID != 0 {
			_ = syscall.Kill(serverPID, syscall.SIGKILL)
		}
		_ = cli.Process.Kill()
		select {
		case <-stopped:
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up CLI subprocess")
		}
	})
	type readiness struct {
		pid int
		err error
	}
	started := make(chan readiness, 1)
	go func() {
		var pid int
		err := json.NewDecoder(ready).Decode(&pid)
		started <- readiness{pid, err}
	}()
	select {
	case r := <-started:
		require.NoError(t, r.err)
		serverPID = r.pid
	case err := <-done:
		t.Fatalf("CLI exited before MCP initialization: %v; output: %s", err, output.String())
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for MCP initialization")
	}
	pgid, err := syscall.Getpgid(serverPID)
	require.NoError(t, err)
	require.Equal(t, serverPID, pgid, "stdio server must have its own process group")
	require.NotEqual(t, cli.Process.Pid, pgid)
	require.NoError(t, cli.Process.Signal(os.Interrupt))
	select {
	case err := <-done:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "output: %s", output.String())
		assert.Equal(t, 1, exit.ExitCode(), "SIGINT must unwind startup, not terminate Kopi directly")
		assert.Contains(t, output.String(), "startup was interrupted")
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not stop MCP startup before its normal timeout")
	}
	require.ErrorIs(t, syscall.Kill(serverPID, 0), syscall.ESRCH, "Kopi must stop and reap its stdio server")
	t.Logf("Kopi exited after SIGINT; independently grouped MCP server %d was reaped (ESRCH)", serverPID)
	serverPID = 0
}

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
					texts := messageTexts(t, server.next(t))
					require.Len(t, texts, 3, texts)
					assert.Equal(t, want, texts[1])
					assert.Equal(t, "The session is in plan mode.", texts[2], "print mode names no plan file")
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
	texts := messageTexts(t, server.next(t))
	require.Len(t, texts, 3, texts)
	assert.Equal(t, "fullscreen prompt", texts[1])
	assert.Regexp(t, "^"+regexp.QuoteMeta("The session is in plan mode. Its plan file is "+filepath.Join(e.kopiHome, "plans")+string(filepath.Separator))+`[^/]+\.md`+regexp.QuoteMeta("; Read, Write and Edit work on that file in both modes.")+"$", texts[2])
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

func TestRootFullscreenSameProviderModelWithoutCredentialReachesLogin(t *testing.T) {
	for _, tc := range []struct {
		name, config string
	}{
		{name: "smallModel", config: `{"smallModel":%q}`},
		{name: "classifier.model", config: `{"classifier":{"enabled":true,"model":%q}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("MISTRAL_API_KEY", "")
			logger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(logger) })
			model := builtinModel(t, "mistral")
			e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), fmt.Sprintf(tc.config, model))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			master, slave := openTestTerminal(t)
			asked, drained := make(chan struct{}), make(chan struct{})
			var result terminalDrainResult
			go func() {
				var output strings.Builder
				buf := make([]byte, 4096)
				sawAsk := false
				for {
					n, err := master.Read(buf)
					output.Write(buf[:n])
					plain := ansi.Strip(output.String())
					if !sawAsk && strings.Contains(plain, "mistral") && strings.Contains(strings.ToLower(plain), "api key") && strings.Contains(plain, "❭") {
						sawAsk = true
						close(asked)
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
			root.SetArgs([]string{"--model", model})
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
				// Close the slave first: a blocked master read can hang on Darwin.
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
			case <-asked:
			case <-drained:
				t.Fatalf("terminal drain ended before login prompt: %v; output: %q", result.err, result.text)
			case err := <-done:
				t.Fatalf("fullscreen exited before login prompt: %v; stderr: %s", err, stderr.String())
			case <-time.After(waitLimit):
				t.Fatal("timed out waiting for first-run login prompt")
			}
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(waitLimit):
				t.Fatal("timed out waiting for fullscreen exit")
			}
			credentials, err := os.ReadFile(filepath.Join(e.agentDir, "auth.json"))
			require.NoError(t, err)
			assert.JSONEq(t, `{}`, string(credentials), "cancelling login must store no credentials")
		})
	}
}

func TestRootFullscreenWithoutModelOrCredentialAsksWhichProvider(t *testing.T) {
	pigruntimetest.ClearEnv(t)
	e := newTestEnv(t)
	t.Setenv("TERM", "xterm-256color")
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	master, slave := openTestTerminal(t)
	asked, drained := make(chan struct{}), make(chan struct{})
	var result terminalDrainResult
	go func() {
		var output strings.Builder
		buf := make([]byte, 4096)
		sawAsk := false
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			if !sawAsk && strings.Contains(ansi.Strip(output.String()), "Log in to a provider:") {
				sawAsk = true
				close(asked)
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
		// Close the slave first: a blocked master read can hang on Darwin.
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
	case <-asked:
	case <-drained:
		t.Fatalf("terminal drain ended before the provider list: %v; output: %q", result.err, result.text)
	case err := <-done:
		t.Fatalf("fullscreen exited before the provider list: %v; stderr: %s", err, stderr.String())
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for the provider list")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for fullscreen exit")
	}
	assert.NoFileExists(t, filepath.Join(e.kopiHome, "config.json"))
}

func TestRootFullscreenDifferentProviderSmallModelWithoutCredentialFailsAtStart(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("MISTRAL_API_KEY", "")
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	configFile := filepath.Join(e.kopiHome, "config.json")
	e.writeFile(t, configFile, fmt.Sprintf(`{"smallModel":%q}`, builtinModel(t, "mistral")))
	master, slave := openTestTerminal(t)
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})
	root := newRoot()
	root.SetIn(slave)
	root.SetOut(slave)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetArgs([]string{"--model", builtinModel(t, "openai-codex")})

	err := root.ExecuteContext(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"smallModel" in `+configFile)
	assert.Contains(t, err.Error(), `no credential for provider "mistral"`)
	assert.Contains(t, err.Error(), `run "kopi login mistral"`)
	assert.NotContains(t, err.Error(), "failed to open session")
}

// startFullscreenWithConfig runs the fullscreen interface on a terminal with
// config as config.json and returns the error of start.
func startFullscreenWithConfig(t *testing.T, e testEnv, config string) error {
	t.Helper()
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	e.writeFile(t, filepath.Join(e.kopiHome, "config.json"), config)
	master, slave := openTestTerminal(t)
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})
	root := newRoot()
	root.SetIn(slave)
	root.SetOut(slave)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetArgs([]string{"--model", builtinModel(t, "openai-codex")})
	return root.ExecuteContext(t.Context())
}

func TestRootFullscreenUnknownWorkflowsModelFailsAtStart(t *testing.T) {
	e := newTestEnv(t)

	err := startFullscreenWithConfig(t, e, `{"workflows":{"model":"nosuch/model-1"}}`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"workflows.model" in `+filepath.Join(e.kopiHome, "config.json"))
	assert.Contains(t, err.Error(), `unknown model "nosuch/model-1": declare the model in `+filepath.Join(e.agentDir, "models.json"))
}

func TestRootFullscreenDifferentProviderWorkflowsModelWithoutCredentialFailsAtStart(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("MISTRAL_API_KEY", "")

	err := startFullscreenWithConfig(t, e, fmt.Sprintf(`{"workflows":{"model":%q}}`, builtinModel(t, "mistral")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"workflows.model" in `+filepath.Join(e.kopiHome, "config.json"))
	assert.Contains(t, err.Error(), `no credential for provider "mistral"`)
	assert.Contains(t, err.Error(), `run "kopi login mistral"`)
	assert.NotContains(t, err.Error(), "failed to open session")
}

func TestRootFullscreenTrustPromptStartsTheProjectServers(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("TERM", "xterm-256color")
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := newMockProvider(t, func(w http.ResponseWriter) { streamText(w, "trusted-response") })
	e.writeMockModels(t, server.URL)
	e.writeServers(t, filepath.Join(e.workspace, ".mcp.json"), map[string]any{"fake": map[string]any{
		"command": os.Args[0],
		"args":    []string{"-test.run=^TestMCPStdioHelper$"},
		"env":     map[string]string{stdioHelperEnv: "serve"},
	}})
	master, slave := openTestTerminal(t)
	asked, ready, answered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	drained := make(chan struct{})
	var result terminalDrainResult
	go func() {
		var output strings.Builder
		buf := make([]byte, 4096)
		var sawAsk, sawReady, sawAnswer bool
		askedAt := 0
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			plain := ansi.Strip(output.String())
			if !sawAsk {
				if i := strings.Index(plain, "Do you trust this folder?"); i >= 0 {
					sawAsk, askedAt = true, i
					close(asked)
				}
			}
			// The main interface's status line names the model; the trust
			// prompt, whose paths may contain "kopi", never does.
			if sawAsk && !sawReady && strings.Contains(plain[askedAt:], "mock-1") {
				sawReady = true
				close(ready)
			}
			if !sawAnswer && strings.Contains(plain, "trusted-response") {
				sawAnswer = true
				close(answered)
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
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up fullscreen terminal drain")
			return
		}
		_ = master.Close()
	})
	waitFor := func(name string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-drained:
			t.Fatalf("terminal drain ended before %s: %v; output: %q", name, result.err, result.text)
		case err := <-done:
			t.Fatalf("fullscreen exited before %s: %v; stderr: %s", name, err, stderr.String())
		case <-time.After(waitLimit):
			t.Fatalf("timed out waiting for %s", name)
		}
	}
	waitFor("the trust prompt", asked)
	_, err := io.WriteString(master, "1") // Yes, trust this folder
	require.NoError(t, err)
	waitFor("the main interface", ready)
	assert.Equal(t, map[string]string{canonical(t, e.workspace): "trusted"}, trustFolders(t, filepath.Join(e.kopiHome, "trust.json")))

	_, err = io.WriteString(master, "what can you use?\r")
	require.NoError(t, err)
	req := server.next(t)
	var tools []string
	for _, tl := range req.body["tools"].([]any) {
		tools = append(tools, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	assert.Contains(t, tools, "ToolSearch")
	system := req.body["messages"].([]any)[0].(map[string]any)
	assert.Contains(t, system["content"], "mcp__fake__search")
	waitFor("the rendered response", answered)
	_, err = master.Write([]byte{4}) // ctrl-d quits with an empty composer.
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for fullscreen exit")
	}
}
