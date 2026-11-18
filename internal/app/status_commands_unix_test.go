//go:build unix

package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestShellRunnerCleansUpDescendants(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "shell exits"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "ready")
			require.NoError(t, unix.Mkfifo(fifo, 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), waitLimit)
			defer cancel()
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
			}
			command := `sh -c 'exec 3>ready; printf r >&3; exec sleep 60' & `
			if mode == "shell exits" {
				command += "printf done"
			} else {
				command += "wait; printf done"
			}
			done := make(chan runResult, 1)
			go func() { out, err := (shellRunner{}).run(ctx, dir, command); done <- runResult{out, err} }()
			ready := make(chan error, 1)
			closed := make(chan error, 1)
			go func() {
				r, err := os.Open(fifo)
				if err != nil {
					ready <- err
					return
				}
				defer r.Close()
				var b [1]byte
				_, err = io.ReadFull(r, b[:])
				ready <- err
				_, err = r.Read(b[:])
				closed <- err
			}()
			select {
			case err := <-ready:
				require.NoError(t, err)
			case <-time.After(waitLimit):
				t.Fatal("child did not become ready")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case result := <-done:
				switch mode {
				case "cancel":
					assert.ErrorIs(t, result.err, context.Canceled)
				case "deadline":
					assert.ErrorIs(t, result.err, context.DeadlineExceeded)
				case "shell exits":
					assert.ErrorIs(t, result.err, exec.ErrWaitDelay)
				}
			case <-time.After(waitLimit):
				t.Fatal("status runner did not return")
			}
			// This independent pipe stays open for the child's lifetime, even if
			// the runner closes its stdout reader to bound draining.
			select {
			case err := <-closed:
				assert.ErrorIs(t, err, io.EOF)
			case <-time.After(waitLimit):
				t.Fatal("child survived runner completion")
			}
		})
	}
}

func TestShellRunnerNonzeroExit(t *testing.T) {
	_, err := (shellRunner{}).run(t.Context(), t.TempDir(), "printf stale; exit 7")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
}

func TestShellRunnerOutput(t *testing.T) {
	dir := t.TempDir()
	out, err := (shellRunner{}).run(t.Context(), dir, "printf '%s' \"$PWD\"; printf ignored >&2")
	require.NoError(t, err)
	assert.Equal(t, dir, out)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = (shellRunner{}).run(ctx, dir, "printf unexpected")
	assert.ErrorIs(t, err, context.Canceled)
}
