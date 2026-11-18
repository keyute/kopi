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

func TestSearchSkipsFIFOWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool func(*Workspace) Tool
		args string
	}{
		{"Glob", func(ws *Workspace) Tool { return NewGlob(ws, nil) }, `{"pattern":"*"}`},
		{"Grep", func(ws *Workspace) Tool { return NewGrep(ws, nil) }, `{"pattern":"needle"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pipe := filepath.Join(dir, "pipe")
			require.NoError(t, syscall.Mkfifo(pipe, 0o600))
			// Held open with a match buffered, so a tool that opens and reads the
			// FIFO, blocking or not, reports it or hangs past go test's -timeout.
			f, err := os.OpenFile(pipe, os.O_RDWR, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			_, err = f.WriteString("needle\n")
			require.NoError(t, err)
			writeFile(t, filepath.Join(dir, "a.txt"), "needle\n")
			ws := newWorkspace(t, dir, nil)

			out, err := tc.tool(ws).Run(t.Context(), []byte(tc.args), allowTarget)
			require.NoError(t, err)
			assert.Contains(t, out.Text, "Found 1 file\na.txt")
			assert.NotContains(t, out.Text, "pipe")
		})
	}
}
