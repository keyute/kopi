//go:build unix

package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
)

// heldStatusCommands holds Close after app-owned execution has been joined.
type heldStatusCommands struct {
	*app.StatusCommands
	cleaned, release, returned chan struct{}
}

func (r *heldStatusCommands) Close() {
	r.StatusCommands.Close()
	close(r.cleaned)
	<-r.release
	close(r.returned)
}

func TestRunQuitJoinsStatusDescendantCleanup(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ready")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	ready, closed := make(chan error, 1), make(chan error, 1)
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

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &quitController{fakeController: fakeController{status: app.Status{Workspace: dir}}, ctx: make(chan context.Context, 1)}
	in, keys := io.Pipe()
	defer in.Close()
	defer keys.Close()
	opts := testOptions()
	opts.StatusLine.Left = []string{"cmd"}
	opts.StatusLine.Segments = map[string]config.StatusSegment{
		"cmd": {Command: `sh -c 'exec 3>ready; printf r >&3; exec sleep 60' & wait`},
	}
	f := New(c, in, &lockedBuffer{}, opts)
	runner := &heldStatusCommands{StatusCommands: app.NewStatusCommands(dir, opts.StatusLine), cleaned: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	f.commands = runner
	defer close(runner.release)
	done := make(chan error, 1)
	go func() { done <- f.Run(parent, nil) }()
	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("descendant did not become ready")
	}
	_, err := io.WriteString(keys, "/quit\r")
	require.NoError(t, err)
	select {
	case <-runner.cleaned:
	case <-time.After(waitLimit):
		t.Fatal("status runner was not cancelled")
	}
	select {
	case err := <-closed:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(waitLimit):
		t.Fatal("descendant survived cleanup")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before status execution completed: %v", err)
	default:
	}
	runner.release <- struct{}{}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("Run did not join status execution")
	}
	select {
	case <-runner.returned:
	default:
		t.Fatal("Run returned without joining status execution")
	}
	assert.NoError(t, parent.Err())
}
