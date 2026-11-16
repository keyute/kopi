//go:build unix

package tool

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSkipsFIFOGitignoreWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, ".gitignore"), 0o600))
	writeFile(t, filepath.Join(dir, "a.txt"), "")
	ws := newWorkspace(t, dir, nil)

	// A blocking open of a writer-less FIFO never returns, so a regression
	// fails here through go test's -timeout.
	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Equal(t, []string{".gitignore", "a.txt"}, got)
}

func TestListKeepsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "a")
	require.NoError(t, os.Mkdir(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })
	writeFile(t, filepath.Join(dir, "b.txt"), "")
	ws := newWorkspace(t, dir, nil)

	got, err := ws.List(100)
	require.NoError(t, err)
	assert.Contains(t, got, "a/")
	assert.Contains(t, got, "b.txt")
}
