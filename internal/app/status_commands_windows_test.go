package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
)

type windowsHelperRunner struct {
	executable string
	env        []string
}

func (r windowsHelperRunner) run(ctx context.Context, dir, _ string) (string, error) {
	return runWindowsStatus(ctx, r.executable, []string{r.executable, "-test.run=^TestStatusWindowsHelper$"}, r.env, dir)
}

func TestWindowsStatusOutputArgumentsEnvironmentWorkspace(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	env := append(os.Environ(), "KOPI_STATUS_HELPER=output", "KOPI_STATUS_DIR="+dir, "KOPI_STATUS_VALUE=custom value")
	out, err := runWindowsStatus(t.Context(), executable, []string{executable, "-test.run=^TestStatusWindowsHelper$", "--", `quoted value \\"end`}, env, dir)
	require.NoError(t, err)
	assert.Equal(t, dir+"|custom value|"+`quoted value \\"end`, out)
}

func TestWindowsStatusNonzeroExit(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	env := append(os.Environ(), "KOPI_STATUS_HELPER=nonzero", "KOPI_STATUS_DIR="+dir)
	_, err = runWindowsStatus(t.Context(), executable, []string{executable, "-test.run=^TestStatusWindowsHelper$"}, env, dir)
	require.ErrorContains(t, err, "exit code 7")
}

func statusTestEvent(t *testing.T) (windows.Handle, string) {
	t.Helper()
	id, err := windows.GenerateGUID()
	require.NoError(t, err)
	name := "Local\\kopi-status-" + id.String()
	h, err := windows.CreateEvent(nil, 1, 0, windows.StringToUTF16Ptr(name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h, name
}

func waitStatusHandle(t *testing.T, h windows.Handle) {
	t.Helper()
	state, err := windows.WaitForSingleObject(h, uint32(waitLimit/time.Millisecond))
	require.NoError(t, err)
	require.EqualValues(t, windows.WAIT_OBJECT_0, state)
}

func TestStatusWindowsHelper(t *testing.T) {
	role := os.Getenv("KOPI_STATUS_HELPER")
	if role == "" {
		return
	}
	dir := os.Getenv("KOPI_STATUS_DIR")
	if err := os.WriteFile(filepath.Join(dir, role), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	if role == "nonzero" {
		fmt.Print("stale")
		os.Exit(7)
	}
	if role == "output" {
		cwd, err := os.Getwd()
		if err != nil {
			os.Exit(7)
		}
		fmt.Printf("%s|%s|%s", cwd, os.Getenv("KOPI_STATUS_VALUE"), os.Args[len(os.Args)-1])
		os.Exit(0)
	}
	open := func(key string) windows.Handle {
		h, err := windows.OpenEvent(windows.EVENT_ALL_ACCESS, false, windows.StringToUTF16Ptr(os.Getenv(key)))
		if err != nil {
			os.Exit(2)
		}
		return h
	}
	ready, gate, hold := open("KOPI_STATUS_READY"), open("KOPI_STATUS_GATE"), open("KOPI_STATUS_HOLD")
	if role == "grandchild" {
		if err := windows.SetEvent(ready); err != nil {
			os.Exit(4)
		}
		_, _ = windows.WaitForSingleObject(hold, windows.INFINITE)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestStatusWindowsHelper$")
	child.Env = append(os.Environ(), "KOPI_STATUS_HELPER=grandchild")
	if os.Getenv("KOPI_STATUS_STDOUT") == "retained" {
		child.Stdout = os.Stdout
	}
	if err := child.Start(); err != nil {
		os.Exit(5)
	}
	_, _ = windows.WaitForSingleObject(gate, windows.INFINITE)
	if _, err := fmt.Fprint(os.Stdout, "done"); err != nil {
		os.Exit(6)
	}
	os.Exit(0)
}

func TestWindowsStatusOwnsDescendants(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "shutdown", "shell exits", "shell exits retaining stdout"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ready, readyName := statusTestEvent(t)
			gate, gateName := statusTestEvent(t)
			_, holdName := statusTestEvent(t)
			env := append(os.Environ(), "KOPI_STATUS_HELPER=child", "KOPI_STATUS_READY="+readyName, "KOPI_STATUS_GATE="+gateName, "KOPI_STATUS_HOLD="+holdName, "KOPI_STATUS_DIR="+dir)
			if mode != "shell exits" {
				env = append(env, "KOPI_STATUS_STDOUT=retained")
			}
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 5*time.Second)
				defer stop()
			}
			done := make(chan runResult, 1)
			var commands *StatusCommands
			if mode == "shutdown" {
				commands = NewStatusCommands(dir, config.StatusLine{Left: []string{"helper"}, Segments: map[string]config.StatusSegment{"helper": {Command: "helper"}}})
				commands.runner = windowsHelperRunner{executable: executable, env: env}
				commands.Start(ctx, func(ev event.Event) {
					result := ev.(event.StatusCommandResult)
					done <- runResult{result.Output, result.Err}
				})
				defer commands.Close()
			} else {
				go func() {
					out, err := runWindowsStatus(ctx, executable, []string{executable, "-test.run=^TestStatusWindowsHelper$"}, env, dir)
					done <- runResult{out, err}
				}()
			}
			waitStatusHandle(t, ready)
			processes := make([]windows.Handle, 0, 2)
			for _, role := range []string{"child", "grandchild"} {
				pidText, err := os.ReadFile(filepath.Join(dir, role))
				require.NoError(t, err)
				pid, err := strconv.Atoi(string(pidText))
				require.NoError(t, err)
				process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, windows.CloseHandle(process)) })
				processes = append(processes, process)
			}
			switch mode {
			case "cancel":
				cancel()
			case "shutdown":
				commands.Close()
			case "shell exits", "shell exits retaining stdout":
				require.NoError(t, windows.SetEvent(gate))
			}
			select {
			case result := <-done:
				switch mode {
				case "cancel", "shutdown":
					assert.ErrorIs(t, result.err, context.Canceled)
				case "deadline":
					assert.ErrorIs(t, result.err, context.DeadlineExceeded)
				default:
					require.NoError(t, result.err)
					assert.Equal(t, "done", result.out)
				}
			case <-time.After(waitLimit):
				t.Fatal("status runner did not return")
			}
			for _, process := range processes {
				waitStatusHandle(t, process)
			}
		})
	}
}

func TestWindowsStatusFailedSetupDoesNotExecute(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	for _, bad := range []struct {
		name, executable, dir string
		env                   []string
	}{
		{"invalid arguments", executable, dir, []string{"INVALID=\x00"}},
		{"invalid workspace", executable, dir + "\x00", os.Environ()},
		{"creation fails", filepath.Join(dir, "missing.exe"), dir, os.Environ()},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := runWindowsStatus(t.Context(), bad.executable, []string{executable, "-test.run=^TestStatusWindowsHelper$"}, append(bad.env, "KOPI_STATUS_HELPER=child", "KOPI_STATUS_DIR="+dir), bad.dir)
			require.Error(t, err)
			_, err = os.Stat(filepath.Join(dir, "child"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runWindowsStatus(ctx, executable, []string{executable}, os.Environ(), dir)
	assert.ErrorIs(t, err, context.Canceled)
}
