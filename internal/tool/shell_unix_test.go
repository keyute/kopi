//go:build unix

package tool

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitLimit only guards against a hung test; no test relies on it elapsing.
const waitLimit = 30 * time.Second

// holdFIFO returns a command whose background grandchild holds the write end
// of a FIFO, and channels closed once that grandchild has opened it and once
// every writer has gone, which proves the grandchild died.
func holdFIFO(t *testing.T) (command string, started, dead <-chan struct{}) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "fifo")
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

func TestShellCancelKillsProcessGroup(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	command, started, dead := holdFIFO(t)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()

	_, err := sh.Run(ctx, command, testTimeout)

	require.ErrorIs(t, err, context.Canceled)
	waitFor(t, dead, "the grandchild to die")
}
