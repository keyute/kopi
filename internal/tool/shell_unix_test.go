//go:build unix

package tool

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	childprocess "github.com/keyute/kopi/internal/process"
	"github.com/keyute/kopi/internal/sandbox"
)

// waitLimit only guards against a hung test; no test relies on it elapsing.
const waitLimit = 30 * time.Second

// holdFIFO returns a command whose background grandchild holds the write end
// of a FIFO, and channels closed once that grandchild has opened it and once
// every writer has gone, which proves the grandchild died.
func holdFIFO(t *testing.T, dir string) (command string, started, dead <-chan struct{}) {
	t.Helper()
	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	startedCh, deadCh := make(chan struct{}), make(chan struct{})
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			t.Errorf("failed to open fifo: %v", err)
			return
		}
		defer f.Close()
		close(startedCh)
		_, _ = io.Copy(io.Discard, f)
		close(deadCh)
	}()
	return "sleep 600 >'" + fifo + "' & wait", startedCh, deadCh
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestShellTimeoutStopsCommand(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = sh.Run(t.Context(), "sleep 600", time.Millisecond)
	}()

	waitFor(t, done, "the command to stop")
	require.ErrorIs(t, err, ErrTimedOut)
}

func lifecycleShell(t *testing.T, dir string, sandboxed bool) *Shell {
	t.Helper()
	var sb *sandbox.Sandbox
	if sandboxed {
		sbs, err := sandbox.New(t.Context(), sandbox.Policy{Write: []string{dir}})
		if errors.Is(err, sandbox.ErrUnavailable) {
			t.Skip(err)
		}
		require.NoError(t, err)
		sb = sbs[0]
	}
	return NewShell(dir, sb)
}

func TestShellCancelKillsProcessGroup(t *testing.T) {
	for _, name := range []string{"unsandboxed", "sandboxed"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sh := lifecycleShell(t, dir, name == "sandboxed")
			command, started, dead := holdFIFO(t, dir)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				<-started
				cancel()
			}()

			_, err := sh.Run(ctx, command, testTimeout)

			require.ErrorIs(t, err, context.Canceled)
			waitFor(t, dead, "the grandchild to die")
		})
	}
}

func mkfifo(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	return path
}

// orphan returns a command whose shell starts a background child that
// inherits its output, then exits. exited delivers the shell's PID once the
// shell has exited, and dead closes once the child has died. The child is
// killed at cleanup if it is still alive.
func orphan(t *testing.T, dir string) (command string, exited <-chan int, dead <-chan struct{}) {
	t.Helper()
	deadFIFO, exitedFIFO := mkfifo(t, dir, "dead"), mkfifo(t, dir, "exited")
	exitedCh, deadCh := make(chan int, 1), make(chan struct{})
	childPID := make(chan int, 1)
	go func() {
		f, err := os.Open(deadFIFO)
		if err != nil {
			t.Errorf("failed to open fifo: %v", err)
			return
		}
		defer f.Close()
		_, _ = io.Copy(io.Discard, f)
		close(deadCh)
	}()
	// O_RDWR never blocks on a FIFO, and holding a write end keeps the
	// shell's line buffered after it closes fd 4.
	exitedFile, err := os.OpenFile(exitedFIFO, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exitedFile.Close() })
	go func() {
		line, _ := bufio.NewReader(exitedFile).ReadString('\n')
		var shell, child int
		if fields := strings.Fields(line); len(fields) == 2 {
			shell, _ = strconv.Atoi(fields[0])
			child, _ = strconv.Atoi(fields[1])
		}
		if child > 0 {
			childPID <- child
		}
		exitedCh <- shell
	}()
	t.Cleanup(func() {
		select {
		case <-deadCh:
			return
		default:
		}
		select {
		case n := <-childPID:
			// Not dead means the child still holds the FIFO, so n is still it.
			_ = syscall.Kill(n, syscall.SIGKILL)
		default:
		}
	})
	// The shell's last act is writing its PID and the child's to fd 4.
	command = "exec 3>'" + deadFIFO + "'; sleep 600 & exec 3>&- 4>'" + exitedFIFO + "'; echo \"$$ $!\" >&4"
	return command, exitedCh, deadCh
}

func TestShellCancelAfterShellExitKillsProcessGroup(t *testing.T) {
	for _, name := range []string{"unsandboxed", "sandboxed"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sh := lifecycleShell(t, dir, name == "sandboxed")
			command, exited, dead := orphan(t, dir)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				// EOF on fd 4 can come before the exit is observable, so wait for
				// it the way Run does, without reaping.
				if pid := <-exited; pid > 0 {
					if err := childprocess.AwaitExit(pid); err != nil {
						t.Errorf("failed to await shell exit: %v", err)
					}
				} else {
					t.Errorf("failed to read the shell's pid")
				}
				cancel()
			}()

			_, err := sh.Run(ctx, command, testTimeout)

			require.ErrorIs(t, err, context.Canceled)
			waitFor(t, dead, "the child to die")
		})
	}
}

func TestShellTimeoutAfterShellExitKillsProcessGroup(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	command, _, dead := orphan(t, t.TempDir())

	_, err := sh.Run(t.Context(), command, waitDelay/2)

	require.ErrorIs(t, err, ErrTimedOut)
	waitFor(t, dead, "the child to die")
}

func TestShellBoundedDrainLeavesChildRunning(t *testing.T) {
	dir := t.TempDir()
	in, ack := mkfifo(t, dir, "in"), mkfifo(t, dir, "ack")
	pidFile := filepath.Join(dir, "pid")
	answered := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-answered:
			return
		default:
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(n, syscall.SIGKILL)
			}
		}
		// Release a round trip blocked opening either FIFO.
		if f, err := os.OpenFile(in, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		if f, err := os.OpenFile(ack, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	sh := NewShell(t.TempDir(), nil)

	_, err := sh.Run(t.Context(), "{ read x <'"+in+"'; echo \"$x\" >'"+ack+"'; } & echo $! >'"+pidFile+"'", testTimeout)

	require.NoError(t, err)
	var got []byte
	var writeErr, readErr error
	roundTrip := make(chan struct{})
	go func() {
		defer close(roundTrip)
		if writeErr = os.WriteFile(in, []byte("ping\n"), 0o600); writeErr != nil {
			return
		}
		got, readErr = os.ReadFile(ack)
	}()
	waitFor(t, roundTrip, "the child to answer")
	close(answered)
	require.NoError(t, writeErr)
	require.NoError(t, readErr)
	assert.Equal(t, "ping\n", string(got))
}

func TestShellTellsExitCodeFromSignal(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	_, err := runShell(t, sh, "exit 137")
	require.ErrorIs(t, err, ErrExitStatus)
	assert.True(t, strings.HasSuffix(err.Error(), "exit code 137"))

	_, err = runShell(t, sh, "kill -KILL $$")
	require.ErrorIs(t, err, ErrExitStatus)
	assert.True(t, strings.HasSuffix(err.Error(), "signal: killed"))
}

func TestShellWaitsForExitAfterOutputCloses(t *testing.T) {
	gate := mkfifo(t, t.TempDir(), "gate")
	sh := NewShell(t.TempDir(), nil)
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = runShell(t, sh, "exec >&- 2>&-; read x <'"+gate+"'; exit 3")
	}()

	// Opening blocks until the shell, its output already closed, reads.
	f, openErr := os.OpenFile(gate, os.O_WRONLY, 0)
	require.NoError(t, openErr)
	require.NoError(t, f.Close())

	waitFor(t, done, "the command to finish")
	require.ErrorIs(t, err, ErrExitStatus)
	assert.True(t, strings.HasSuffix(err.Error(), "exit code 3"))
}
