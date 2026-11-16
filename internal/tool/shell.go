package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
//
// Every kill happens before the single reap: until then the shell's PID, which
// is also its process group ID, cannot be reused, so killing the group is safe.
func (s *Shell) Run(ctx context.Context, command string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{s.shell, "-c", command}
	if s.sb != nil {
		argv = s.sb.Wrap(argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.dir
	r, w, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("failed to create output pipe: %w", err)
	}
	// An *os.File goes to the shell as is, so exec starts no copy goroutine
	// that Wait would drain, and one fd for both keeps the streams
	// interleaved in order.
	cmd.Stdout, cmd.Stderr = w, w
	newSession(cmd)
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return "", fmt.Errorf("failed to run command: %w", err)
	}
	out := &outputBuffer{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(out, r)
	}()
	exited, reap := watchExit(cmd)

	var reason error
	stop := func() {
		reason = ErrTimedOut
		if err := ctx.Err(); err != nil {
			reason = err
		}
		_ = kill(cmd)
	}
	var exitErr error
	select {
	case exitErr = <-exited:
	case <-runCtx.Done():
		stop()
		exitErr = <-exited
	}
	if exitErr != nil {
		_ = kill(cmd)
		_ = r.Close()
		<-drained
		_ = reap()
		return "", fmt.Errorf("failed to wait for command: %w", exitErr)
	}

	timer := time.NewTimer(waitDelay)
	defer timer.Stop()
	done := runCtx.Done()
	if reason != nil {
		done = nil
	}
	select {
	case <-drained:
	case <-timer.C:
	case <-done:
		stop()
		select {
		case <-drained:
		case <-timer.C:
		}
	}
	_ = r.Close()
	<-drained
	err = reap()

	text := out.String()
	switch {
	case errors.Is(reason, ErrTimedOut):
		return "", fmt.Errorf("%s\n%w after %v", text, ErrTimedOut, timeout)
	case reason != nil:
		return "", reason
	}
	var exitStatus *exec.ExitError
	switch {
	case err == nil:
		return text, nil
	case errors.As(err, &exitStatus):
		status := fmt.Sprintf("exit code %d", exitStatus.ExitCode())
		if exitStatus.ExitCode() == -1 {
			status = exitStatus.String()
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
