// Package headless is the one-shot frontend: it sends one prompt to the
// Controller and prints the final message of the reply.
package headless

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

type Frontend struct {
	c      *app.Controller
	prompt string
	out    io.Writer
	// text is the last stretch of text in the reply; over is whether a
	// notice has followed it, so that later text replaces it. Both are
	// written only while the turn runs and read only once it is done.
	text strings.Builder
	over bool
}

// New returns a frontend for prompt, which must not be blank.
func New(c *app.Controller, prompt string, out io.Writer) *Frontend {
	return &Frontend{c: c, prompt: prompt, out: out}
}

// Run sends the prompt and waits for its turn to end. interrupts delivers
// SIGINT, which interrupts the turn.
func (f *Frontend) Run(ctx context.Context, interrupts <-chan os.Signal) error {
	defer f.c.Subscribe(f.render)()
	w, err := f.c.Prompt(ctx, f.prompt)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to finish the turn: %w", ctx.Err())
		case <-interrupts:
			f.c.Interrupt()
		case <-w.Done():
			return f.finish(w.Outcome())
		}
	}
}

// render keeps only the final message: text before a tool call is
// intermediate, and a notice (such as a retry) ends a failed attempt's text.
// A notice after the final text, such as compaction, leaves it in place.
func (f *Frontend) render(ev event.Event) {
	switch e := ev.(type) {
	case event.TextDelta:
		if f.over {
			f.text.Reset()
			f.over = false
		}
		f.text.WriteString(e.Text)
	case event.ToolStart:
		f.text.Reset()
		f.over = false
	case event.Notice:
		f.over = true
	}
}

func (f *Frontend) finish(outcome event.Outcome) error {
	switch outcome.Status {
	case event.Failed:
		return fmt.Errorf("%w: %s", ErrFailed, outcome.Message)
	case event.Interrupted:
		return ErrInterrupted
	}
	text := f.text.String()
	if text == "" {
		return nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := io.WriteString(f.out, text); err != nil {
		return fmt.Errorf("failed to print the reply: %w", err)
	}
	return nil
}
