package tool

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	outputs    *Outputs
}

// NewShell runs commands from dir; a nil sb runs them unsandboxed.
func NewShell(dir string, sb *sandbox.Sandbox) *Shell {
	shell, err := exec.LookPath("bash")
	if err != nil {
		shell = "/bin/sh"
	}
	return &Shell{dir: dir, shell: shell, sb: sb}
}

// WithOutputs returns a copy of s whose output too long to return whole is
// saved to outputs.
func (s *Shell) WithOutputs(o *Outputs) *Shell {
	c := *s
	c.outputs = o
	return &c
}

func (s *Shell) Sandboxed() bool { return s.sb != nil }

// FiltersNetwork reports whether the shell's commands reach the network only
// through a filtering proxy.
func (s *Shell) FiltersNetwork() bool { return s.sb != nil && s.sb.FiltersNetwork() }

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
	var cmd *exec.Cmd
	release := func() []string { return nil }
	if s.sb == nil {
		cmd = exec.Command(argv[0], argv[1:]...)
	} else {
		var err error
		cmd, release, err = s.sb.Command(argv)
		if err != nil {
			return nil, fmt.Errorf("failed to sandbox command: %w", err)
		}
	}
	cmd.Dir = s.dir
	r, w, err := os.Pipe()
	if err != nil {
		release()
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
		release()
		return nil, err
	}
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		release()
		return nil, fmt.Errorf("failed to run command: %w", err)
	}
	p := &process{out: s.outputs.buffer(), stopCh: make(chan error, 1), done: make(chan struct{})}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(p.out, r)
	}()
	exited, reap := watchExit(cmd)
	go p.supervise(cmd, r, drained, exited, reap, release)
	return p, nil
}

// stop kills the command's process group, recording reason as why it ended.
// It has no effect once the supervisor has finished, and reports whether this
// call was the first to ask.
func (p *process) stop(reason error) bool {
	first := false
	p.once.Do(func() {
		first = true
		p.stopCh <- reason
	})
	return first
}

// supervise waits for the command, kills it on stop, and reaps it.
//
// Every kill happens before the single reap: until then the shell's PID, which
// is also its process group ID, cannot be reused, so killing the group is safe.
func (p *process) supervise(cmd *exec.Cmd, r *os.File, drained <-chan struct{}, exited <-chan error, reap func() error, release func() []string) {
	defer close(p.done)
	// start returned while the command runs, so release must follow the reap.
	// It runs before p.done closes, and so before anything reads p.out.
	defer func() {
		_, _ = io.WriteString(p.out, refusedNote(release()))
		p.out.close()
	}()
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

// refusedNote is the line that ends a result naming the destinations the
// sandbox's proxy refused, or nothing when there are none.
func refusedNote(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	return "\n[sandbox refused network access to: " + strings.Join(hosts, ", ") + "]\n"
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
// command prints. With a dir, the output is also saved to a file from the first
// write that would cut it, or from keep. The zero value saves nothing.
type outputBuffer struct {
	mu         sync.Mutex
	head, tail []byte
	total      int

	// dir is where the file is created, "" once there is nothing left to try.
	dir string
	// closed means the command ended: later writes only reach memory, but keep
	// still saves what head and tail hold.
	closed bool
	// limit is the most bytes the file takes.
	limit   int
	f       *os.File
	path    string
	written int
	// partial means the file stopped short of the end of the output.
	partial bool
}

// Write always reports len(p) and no error: io.Copy stops draining on one, and
// the command would block on a full pipe.
func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dir != "" && !b.closed && b.total+len(p) > 2*maxOutputPart {
		b.start()
	}
	b.save(p)
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

// keep saves the output to the file from now on, however short it is: a
// background command's output is read from the file while the command runs.
// A command that already ended gets the file too.
func (b *outputBuffer) keep() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dir == "" {
		return
	}
	b.start()
	if b.closed && b.f != nil {
		_ = b.f.Close()
		b.f = nil
	}
}

// start creates the file with what is written so far. Head and tail still
// hold every byte written: nothing is cut before the file exists.
func (b *outputBuffer) start() {
	b.create()
	b.save(b.head)
	b.save(b.tail)
}

// create opens the file, once. A failure is logged and the buffer carries on
// without a file.
func (b *outputBuffer) create() {
	dir := b.dir
	b.dir = ""
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		slog.Warn("failed to create the output folder", "dir", dir, "err", err)
		return
	}
	root, err := pinSession(dir, dir, true)
	if err != nil {
		slog.Warn("failed to create the output folder", "dir", dir, "err", err)
		return
	}
	defer root.Close()
	name := rand.Text() + ".txt"
	path := filepath.Join(dir, name)
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Warn("failed to create an output file", "path", path, "err", err)
		return
	}
	b.f, b.path = f, path
}

// save appends data to the file, which it closes at the limit or on a write
// failure, leaving the bytes before that in it.
func (b *outputBuffer) save(data []byte) {
	if b.f == nil || len(data) == 0 {
		return
	}
	if room := b.limit - b.written; len(data) > room {
		data = data[:max(room, 0)]
		b.partial = true
	}
	n, err := b.f.Write(data)
	b.written += n
	if err != nil {
		slog.Warn("failed to write an output file", "path", b.path, "err", err)
		b.partial = true
	}
	if b.partial {
		_ = b.f.Close()
		b.f = nil
	}
}

// close ends the file; later writes only reach memory.
func (b *outputBuffer) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.f != nil {
		_ = b.f.Close()
		b.f = nil
	}
}

// file is the saved output's path, "" when none was created.
func (b *outputBuffer) file() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.saved() {
		return ""
	}
	return b.path
}

// saved reports whether the file holds more than the head, so that the notice
// can point into it.
func (b *outputBuffer) saved() bool {
	return b.path != "" && (!b.partial || b.written > len(b.head))
}

func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total == 0 {
		return noOutputNotice
	}
	s := string(b.head)
	if cut := b.total - len(b.head) - len(b.tail); cut > 0 {
		if !b.saved() {
			s += fmt.Sprintf("\n[… %d bytes cut …]\n", cut)
		} else {
			what := "full output"
			if b.partial {
				what = fmt.Sprintf("first %d bytes of output", b.written)
			}
			s += fmt.Sprintf("\n[… %d bytes cut; %s: %s; use Read with offset=%d to continue …]\n", cut, what, b.path, bytes.Count(b.head, []byte("\n"))+1)
		}
	}
	return strings.ToValidUTF8(s+string(b.tail), "�")
}
