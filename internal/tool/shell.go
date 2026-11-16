package tool

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/sandbox"
)

const (
	// maxOutputPart is how many bytes of output are kept from each end.
	maxOutputPart = 15000
	// waitDelay bounds the wait for output after the shell exits, so a
	// process that inherited the pipe cannot hold the call open.
	waitDelay = time.Second

	noOutputNotice = "(no output)"
)

// Shell runs commands in a fresh shell each time, under the OS sandbox when
// it has one.
type Shell struct {
	dir, shell string
	sb         *sandbox.Sandbox
}

// NewShell runs commands from dir; a nil sb runs them unsandboxed.
func NewShell(dir string, sb *sandbox.Sandbox) *Shell {
	shell, err := exec.LookPath("bash")
	if err != nil {
		shell = "/bin/sh"
	}
	return &Shell{dir: dir, shell: shell, sb: sb}
}

func (s *Shell) Sandboxed() bool { return s.sb != nil }

// Run returns the command's combined output. The error of a command that
// failed or timed out carries the output, then the reason on a final line.
func (s *Shell) Run(ctx context.Context, command string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{s.shell, "-c", command}
	if s.sb != nil {
		argv = s.sb.Wrap(argv)
	}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = s.dir
	out := &outputBuffer{}
	// One comparable writer for both makes exec share a single pipe, so the
	// streams interleave in order.
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = waitDelay
	killGroup(cmd)
	err := cmd.Run()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text := out.String()
	if runCtx.Err() != nil {
		return "", fmt.Errorf("%s\n%w after %v", text, ErrTimedOut, timeout)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		return text, nil
	case errors.As(err, &exitErr):
		status := fmt.Sprintf("exit code %d", exitErr.ExitCode())
		if exitErr.ExitCode() == -1 {
			status = exitErr.String()
		}
		return "", fmt.Errorf("%s\n%w: %s", text, ErrExitStatus, status)
	default:
		return "", fmt.Errorf("failed to run command: %w", err)
	}
}

// outputBuffer keeps the first maxOutputPart bytes written and a sliding
// window of the last maxOutputPart, so memory stays bounded however much a
// command prints.
type outputBuffer struct {
	head, tail []byte
	total      int
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	n := min(len(p), maxOutputPart-len(b.head))
	b.head = append(b.head, p[:n]...)
	rest := p[n:]
	if len(rest) >= maxOutputPart {
		b.tail = append(b.tail[:0], rest[len(rest)-maxOutputPart:]...)
		return len(p), nil
	}
	if drop := len(b.tail) + len(rest) - maxOutputPart; drop > 0 {
		b.tail = append(b.tail[:0], b.tail[drop:]...)
	}
	b.tail = append(b.tail, rest...)
	return len(p), nil
}

func (b *outputBuffer) String() string {
	if b.total == 0 {
		return noOutputNotice
	}
	s := string(b.head)
	if cut := b.total - len(b.head) - len(b.tail); cut > 0 {
		s += fmt.Sprintf("\n[… %d bytes cut …]\n", cut)
	}
	return strings.ToValidUTF8(s+string(b.tail), "�")
}
