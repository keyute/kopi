package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// process is a started command. Its supervisor alone kills and reaps it; the
// fields after done are written by the supervisor and read only after done.
type process struct {
	out    *outputBuffer
	stopCh chan error
	once   sync.Once
	done   chan struct{}
	reason error
	err    error
	exit   error
	ended  time.Time
}

// start runs command without tying it to ctx, which is checked only before
// the command starts.
func (s *Shell) start(ctx context.Context, command string) (*process, error) {
	argv := []string{s.shell, "-c", command}
	if s.sb != nil {
		argv = s.sb.Wrap(argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.dir
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create output pipe: %w", err)
	}
	// An *os.File goes to the shell as is, so exec starts no copy goroutine
	// that Wait would drain, and one fd for both keeps the streams
	// interleaved in order.
	cmd.Stdout, cmd.Stderr = w, w
	newSession(cmd)
	if err := ctx.Err(); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, err
	}
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("failed to run command: %w", err)
	}
	p := &process{out: &outputBuffer{}, stopCh: make(chan error, 1), done: make(chan struct{})}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(p.out, r)
	}()
	exited, reap := watchExit(cmd)
	go p.supervise(cmd, r, drained, exited, reap)
	return p, nil
}

// stop kills the command's process group, recording reason as why it ended.
// It has no effect once the supervisor has finished.
func (p *process) stop(reason error) {
	p.once.Do(func() { p.stopCh <- reason })
}

// supervise waits for the command, kills it on stop, and reaps it.
//
// Every kill happens before the single reap: until then the shell's PID, which
// is also its process group ID, cannot be reused, so killing the group is safe.
func (p *process) supervise(cmd *exec.Cmd, r *os.File, drained <-chan struct{}, exited <-chan error, reap func() error) {
	defer close(p.done)
	defer func() { p.ended = time.Now() }()
	var exitErr error
	select {
	case exitErr = <-exited:
	case p.reason = <-p.stopCh:
		_ = kill(cmd)
		exitErr = <-exited
	}
	if exitErr != nil {
		_ = kill(cmd)
		_ = r.Close()
		<-drained
		_ = reap()
		p.err = fmt.Errorf("failed to wait for command: %w", exitErr)
		return
	}

	timer := time.NewTimer(waitDelay)
	defer timer.Stop()
	stopCh := p.stopCh
	if p.reason != nil {
		stopCh = nil
	}
	select {
	case <-drained:
	case <-timer.C:
	case p.reason = <-stopCh:
		_ = kill(cmd)
		select {
		case <-drained:
		case <-timer.C:
		}
	}
	_ = r.Close()
	<-drained
	p.exit = reap()
}

// result is the command's output, or the error carrying it, once done.
func (p *process) result() (string, error) {
	if p.err != nil {
		return "", p.err
	}
	text := p.out.String()
	switch {
	case errors.Is(p.reason, context.Canceled), errors.Is(p.reason, context.DeadlineExceeded):
		return "", p.reason
	case p.reason != nil:
		return "", fmt.Errorf("%s\n%w", text, p.reason)
	}
	var exitStatus *exec.ExitError
	switch {
	case p.exit == nil:
		return text, nil
	case errors.As(p.exit, &exitStatus):
		return "", fmt.Errorf("%s\n%w: %s", text, ErrExitStatus, exitReason(exitStatus))
	default:
		return "", fmt.Errorf("failed to run command: %w", p.exit)
	}
}

func exitReason(e *exec.ExitError) string {
	if e.ExitCode() == -1 {
		return e.String()
	}
	return fmt.Sprintf("exit code %d", e.ExitCode())
}

// outputBuffer keeps the first maxOutputPart bytes written and a sliding
// window of the last maxOutputPart, so memory stays bounded however much a
// command prints.
type outputBuffer struct {
	mu         sync.Mutex
	head, tail []byte
	total      int
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total == 0 {
		return noOutputNotice
	}
	s := string(b.head)
	if cut := b.total - len(b.head) - len(b.tail); cut > 0 {
		s += fmt.Sprintf("\n[… %d bytes cut …]\n", cut)
	}
	return strings.ToValidUTF8(s+string(b.tail), "�")
}
