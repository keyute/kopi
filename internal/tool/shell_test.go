package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run returns the command's combined output. The error of a command that
// failed or timed out carries the output, then the reason on a final line.
func (s *Shell) Run(ctx context.Context, command string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if runCtx.Err() != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s\n%w after %v", noOutputNotice, ErrTimedOut, timeout)
	}
	p, err := s.start(ctx, command)
	if err != nil {
		return "", err
	}
	select {
	case <-p.done:
	case <-runCtx.Done():
		reason := ctx.Err()
		if reason == nil {
			reason = fmt.Errorf("%w after %v", ErrTimedOut, timeout)
		}
		p.stop(reason)
		<-p.done
	}
	return p.result()
}

const testTimeout = time.Minute

func runShell(t *testing.T, sh *Shell, command string) (string, error) {
	t.Helper()
	return sh.Run(t.Context(), command, testTimeout)
}

func TestShellDoesNotStartCanceledRun(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			sh := NewShell(t.TempDir(), nil)
			// A Start attempt must fail differently from cancellation.
			sh.shell = filepath.Join(sh.dir, "missing-shell")
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			cancel()

			out, err := sh.Run(ctx, "echo unexpected", testTimeout)

			require.ErrorIs(t, err, ctx.Err())
			assert.Equal(t, ctx.Err().Error(), err.Error())
			assert.Empty(t, out)
		})
	}
}

func TestShellDoesNotStartExpiredRun(t *testing.T) {
	sh := NewShell(t.TempDir(), nil)
	sh.shell = filepath.Join(sh.dir, "missing-shell")

	out, err := sh.Run(t.Context(), "echo unexpected", -time.Second)

	require.ErrorIs(t, err, ErrTimedOut)
	assert.Equal(t, fmt.Sprintf("%s\n%s after -1s", noOutputNotice, ErrTimedOut), err.Error())
	assert.Empty(t, out)
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

func TestOutputBufferReadsWhileWritten(t *testing.T) {
	var b outputBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _ = b.Write([]byte("x"))
		}
	}()
	for {
		select {
		case <-done:
			assert.Equal(t, strings.Repeat("x", 1000), b.String())
			return
		default:
			_ = b.String()
		}
	}
}
