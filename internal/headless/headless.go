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

// Run sends the prompt and waits for its turn to end, then for the running
// subagents and the turns their ends wake, and finishes with the last turn's
// outcome; a turn that does not complete finishes the run at once. interrupts
// delivers SIGINT, which interrupts the running turn or, with only subagents
// left, ends the run.
func (f *Frontend) Run(ctx context.Context, interrupts <-chan os.Signal) error {
	turns := f.c.FollowWork()
	defer turns.Close()
	if _, err := f.c.Prompt(ctx, f.prompt); err != nil {
		return err
	}
	var last *app.Work
	for {
		w, live, changed := turns.Next()
		switch {
		case w != nil:
			if err := f.await(ctx, interrupts, w); err != nil {
				return err
			}
			if w.Outcome().Status != event.Completed {
				return f.finish(w.Outcome(), w.Reply())
			}
			last = w
		case !live:
			return f.finish(last.Outcome(), last.Reply())
		default:
			select {
			case <-ctx.Done():
				return fmt.Errorf("failed to finish the turn: %w", ctx.Err())
			case <-interrupts:
				return ErrInterrupted
			case <-changed:
			}
		}
	}
}

// await waits for w, interrupting it on SIGINT.
func (f *Frontend) await(ctx context.Context, interrupts <-chan os.Signal, w *app.Work) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to finish the turn: %w", ctx.Err())
		case <-interrupts:
			f.c.Interrupt(app.Main)
		case <-w.Done():
			return nil
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
