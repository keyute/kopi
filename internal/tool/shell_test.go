package tool

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTimeout = time.Minute

func runShell(t *testing.T, sh *Shell, command string) (string, error) {
	t.Helper()
	return sh.Run(t.Context(), command, testTimeout)
}

func TestShellCombinesOutputInOrder(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	out, err := runShell(t, sh, "echo out; echo err >&2; echo out2")

	require.NoError(t, err)
	assert.Equal(t, "out\nerr\nout2\n", out)
}

func TestShellKeepsHeadAndTailOfLongOutput(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	// 1000 lines of 100 bytes.
	out, err := runShell(t, sh, `i=0; while [ $i -lt 1000 ]; do printf '%099d\n' $i; i=$((i+1)); done`)

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, fmt.Sprintf("%099d\n", 0)))
	assert.True(t, strings.HasSuffix(out, fmt.Sprintf("%099d\n", 999)))
	assert.Contains(t, out, "\n[… 70000 bytes cut …]\n")
	assert.Len(t, out, 2*maxOutputPart+len("\n[… 70000 bytes cut …]\n"))
}

func TestOutputBufferStaysBounded(t *testing.T) {
	var b outputBuffer
	chunk := []byte(strings.Repeat("x", 4099))
	for b.total < 10<<20 {
		_, err := b.Write(chunk)
		require.NoError(t, err)
		require.LessOrEqual(t, len(b.head), maxOutputPart)
		require.LessOrEqual(t, len(b.tail), maxOutputPart)
		require.LessOrEqual(t, cap(b.tail), 2*maxOutputPart)
	}
	_, err := b.Write([]byte(strings.Repeat("y", 64<<10)))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("y", maxOutputPart), string(b.tail))
	assert.LessOrEqual(t, cap(b.tail), 2*maxOutputPart)
}

func TestShellReportsExitStatus(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	_, err := runShell(t, sh, "echo oops; exit 3")

	require.ErrorIs(t, err, ErrExitStatus)
	assert.True(t, strings.HasPrefix(err.Error(), "oops\n"))
	assert.True(t, strings.HasSuffix(err.Error(), "exit code 3"))
}

func TestShellStartsFreshEachCall(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell(dir, nil)

	_, err := runShell(t, sh, "cd /; export KOPI_SHELL_TEST_X=1")
	require.NoError(t, err)
	out, err := runShell(t, sh, `pwd; echo "x=$KOPI_SHELL_TEST_X"`)

	require.NoError(t, err)
	assert.Equal(t, dir+"\nx=\n", out)
}

func TestShellHasNoInput(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	out, err := runShell(t, sh, "cat")

	require.NoError(t, err)
	assert.Equal(t, noOutputNotice, out)
}

func TestShellFallsBackToSh(t *testing.T) {
	t.Setenv("PATH", "")

	sh := NewShell(t.TempDir(), nil)

	assert.Equal(t, "/bin/sh", sh.shell)
	out, err := runShell(t, sh, "echo hi")
	require.NoError(t, err)
	assert.Equal(t, "hi\n", out)
}

func TestShellReportsNoOutput(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)

	out, err := runShell(t, sh, "true")

	require.NoError(t, err)
	assert.Equal(t, noOutputNotice, out)
}
