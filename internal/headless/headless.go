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
}

// New returns a frontend for prompt, which must not be blank.
func New(c *app.Controller, prompt string, out io.Writer) *Frontend {
	return &Frontend{c: c, prompt: prompt, out: out}
}

// Run sends the prompt and waits for its turn to end. interrupts delivers
// SIGINT, which interrupts the turn.
func (f *Frontend) Run(ctx context.Context, interrupts <-chan os.Signal) error {
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
			return f.finish(w.Outcome(), w.Reply())
		}
	}
}

func (f *Frontend) finish(outcome event.Outcome, text string) error {
	switch outcome.Status {
	case event.Failed:
		return fmt.Errorf("%w: %s", ErrFailed, outcome.Message)
	case event.Interrupted:
		return ErrInterrupted
	}
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
