//go:build unix

package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRefusesFIFOWithoutBlocking(t *testing.T) {
	home, kopiHome := syntheticHome(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(home, "pipe"), 0o600))
	require.NoError(t, syscall.Mkfifo(filepath.Join(home, ".ssh", "pipe"), 0o600))
	ws := newWorkspace(t, home, DefaultProtected(home, kopiHome))

	for _, tc := range []struct {
		path string
		want error
	}{
		{"pipe", ErrNotRegular},
		{".ssh/pipe", ErrProtected},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// A blocking open of a writer-less FIFO never returns, so a
			// regression fails here through go test's -timeout.
			f, err := ws.Open(tc.path)
			require.ErrorIs(t, err, tc.want)
			require.Nil(t, f)
		})
	}
}

func TestOpenFailsClosedWhenProtectedPathCannotBeChecked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	locked := filepath.Join(t.TempDir(), "locked")
	writeFile(t, filepath.Join(locked, ".ssh", "id_x"), "private key")
	ws := newWorkspace(t, dir, []string{filepath.Join(locked, ".ssh")})
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })

	for _, path := range []string{"a.txt", "missing.txt"} {
		f, err := ws.Open(path)
		require.ErrorIs(t, err, os.ErrPermission, path)
		require.Nil(t, f)
	}
}

func TestWriteFileRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
	ws := newWorkspace(t, dir, nil)

	err := ws.WriteFile("pipe", []byte("x"))
	require.ErrorIs(t, err, ErrNotRegular)
	assert.Equal(t, []string{"pipe"}, listDir(t, dir))
}

func TestWriteFileLeavesNothingWhenCreateFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ro", "a.txt"), "old")
	require.NoError(t, os.Chmod(filepath.Join(dir, "ro"), 0o555))
	t.Cleanup(func() { require.NoError(t, os.Chmod(filepath.Join(dir, "ro"), 0o755)) })
	ws := newWorkspace(t, dir, nil)

	err := ws.WriteFile("ro/a.txt", []byte("new"))
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Equal(t, []string{"a.txt"}, listDir(t, filepath.Join(dir, "ro")))
	got, err := os.ReadFile(filepath.Join(dir, "ro", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestWriteFileRemovesTempFileWhenRenameFails(t *testing.T) {
	// An immutable file can be renamed over by no one, so the temp file is
	// created and filled and only the final rename fails. Only the BSDs let an
	// unprivileged user set the flag.
	if _, err := exec.LookPath("chflags"); err != nil {
		t.Skip("chflags is not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "old")
	require.NoError(t, exec.Command("chflags", "uchg", path).Run())
	t.Cleanup(func() { require.NoError(t, exec.Command("chflags", "nouchg", path).Run()) })
	ws := newWorkspace(t, dir, nil)

	err := ws.WriteFile("a.txt", []byte("new"))
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Equal(t, []string{"a.txt"}, listDir(t, dir))
	assert.Equal(t, "old", readContent(t, path))
}
