// Package line is a plain-text, line-oriented frontend: it passes input lines
// to the Controller and prints the events the Controller emits.
package line

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

const (
	// maxLineBytes is a 1 MiB input line plus its "\r\n".
	maxLineBytes = 1<<20 + 2
	maxArgsWidth = 120
)

type Frontend struct {
	c           *app.Controller
	in          io.Reader
	unsubscribe func()

	mu  sync.Mutex
	out io.Writer
	// atLineStart is whether the last write ended a line, so a status line
	// never continues streamed text.
	atLineStart bool
	// lastError is the last Error event of the current turn; a Failed outcome
	// carrying the same text is not printed twice.
	lastError string
}

// New subscribes to c at once, so events from an Open that follows (the
// session line) are printed. Run unsubscribes when it returns.
func New(c *app.Controller, in io.Reader, out io.Writer) *Frontend {
	f := &Frontend{c: c, in: in, out: out, atLineStart: true}
	f.unsubscribe = c.Subscribe(f.render)
	return f
}

// Run reads lines until EOF, /quit, ctx cancellation, or an interrupt at idle.
// interrupts delivers SIGINT. Input is strictly sequential: the next line is
// read only once the turn a prompt started has ended, and at EOF Run waits for
// the running turn first.
func (f *Frontend) Run(ctx context.Context, interrupts <-chan os.Signal) error {
	defer f.unsubscribe()

	lines := make(chan string)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	// The reader may stay blocked in Read after Run returns; nothing can
	// unblock a read on a terminal.
	go func() {
		sc := bufio.NewScanner(f.in)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-stop:
				return
			}
		}
		readErr <- sc.Err()
		close(lines)
	}()

	var turnDone <-chan struct{}
	for {
		input := lines
		if turnDone != nil {
			input = nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-interrupts:
			if !f.c.Interrupt() {
				return nil
			}
		case <-turnDone:
			turnDone = nil
		case text, ok := <-input:
			if !ok {
				if err := <-readErr; err != nil {
					return fmt.Errorf("failed to read input: %w", err)
				}
				return nil
			}
			w, err := f.c.Submit(ctx, text)
			switch {
			case errors.Is(err, app.ErrQuit):
				return nil
			case err != nil:
				f.println("[error] " + err.Error())
			case w != nil:
				turnDone = w.Done()
			}
		}
	}
}

func (f *Frontend) println(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.line(s)
}

func (f *Frontend) render(ev event.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch e := ev.(type) {
	case event.TextDelta:
		f.write(e.Text)
	case event.ToolStart:
		f.line("[tool] " + e.Name + " " + truncate(e.Args, maxArgsWidth))
	case event.ToolEnd:
		if e.IsError {
			first, _, _ := strings.Cut(strings.TrimSpace(e.Output), "\n")
			f.line("[tool] " + e.Name + " error: " + first)
		} else {
			f.line("[tool] " + e.Name + " ok")
		}
	case event.Notice:
		f.line(e.Text)
	case event.ModeChanged:
		f.line("[mode] " + e.Mode.String())
	case event.SessionChanged:
		s := "[session] " + e.SessionID
		if e.Resumed {
			s += fmt.Sprintf(" (resumed, %d messages)", e.MessageCount)
		}
		f.line(s)
	case event.Error:
		f.lastError = e.Err.Error()
		f.line("[error] " + f.lastError)
	case event.TurnEnd:
		switch e.Outcome.Status {
		case event.Completed:
			f.ensureLineStart()
		case event.Interrupted:
			f.line("[interrupted]")
		case event.Failed:
			if e.Outcome.Message != f.lastError {
				f.line("[error] " + e.Outcome.Message)
			}
		}
		f.lastError = ""
	}
}

// line, ensureLineStart and write must be called with mu held. Write errors
// are ignored: there is nowhere else to report them.
func (f *Frontend) line(s string) {
	f.ensureLineStart()
	f.write(s + "\n")
}

func (f *Frontend) ensureLineStart() {
	if !f.atLineStart {
		f.write("\n")
	}
}

func (f *Frontend) write(s string) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(f.out, s)
	f.atLineStart = strings.HasSuffix(s, "\n")
}

func truncate(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-3]) + "..."
}
