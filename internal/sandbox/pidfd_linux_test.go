package sandbox

import (
	"bufio"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestBwrapDoneWaitsForNamespace(t *testing.T) {
	dir := t.TempDir()
	sb := newBwrapIn(t, dir, Policy{})[0]
	// The child leaves the process group, so killing the group would not reach
	// it; it ends only when the namespace does. It holds w, and reports ready
	// on stdout once started.
	cmd, done, err := sb.Command([]string{"/bin/bash", "-c", `setsid sleep 600 </dev/null >/dev/null 2>&1 & echo ready; exec sleep 600`})
	require.NoError(t, err)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	require.NoError(t, w.Close())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)

	// Only the outer bwrap dies; the namespace's PID 1 goes with it, but not
	// necessarily before Wait returns.
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	done()

	fds := []unix.PollFd{{Fd: int32(r.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the namespace still holds the pipe after done")
	got, err := r.Read(make([]byte, 1))
	assert.Zero(t, got)
	assert.ErrorIs(t, err, io.EOF)
}
