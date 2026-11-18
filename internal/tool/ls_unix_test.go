//go:build unix

package tool

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLSListsFIFOWithoutOpening(t *testing.T) {
	tool, dir := newLSTool(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
	writeFile(t, filepath.Join(dir, "a.txt"), "")

	// A blocking open of a writer-less FIFO never returns, so a regression
	// fails here through go test's -timeout.
	out, err := runLS(t, tool, `{}`)
	require.NoError(t, err)
	assert.Equal(t, "a.txt\npipe", out)
}
