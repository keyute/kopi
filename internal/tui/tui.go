// Package tui is the fullscreen terminal frontend: it passes composer input
// to the Controller and draws the transcript its events reduce to.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// Controller is the part of app.Controller the frontend uses.
type Controller interface {
	Subscribe(fn func(event.Event)) func()
	Submit(ctx context.Context, text string, images ...string) (*app.Work, error)
	SendNow(ctx context.Context, text string, images ...string) error
	TakeQueued() []event.Input
	Interrupt() bool
	ToggleMode()
	History() []string
	Status() app.Status
}

// Options are the frontend's settings. Keys maps action names to key
// strings; an action without keys is disabled. StatusLeft and StatusRight
// name status line segments. ImageDir holds saved clipboard images.
type Options struct {
	Version                 string
	Keys                    map[string][]string
	StatusLeft, StatusRight []string
	ImageDir                string
}

type Frontend struct {
	c           Controller
	in          io.Reader
	out         io.Writer
	opts        Options
	bridge      *bridge
	unsubscribe func()
}

// New subscribes to c at once, so events from an Open that follows are
// drawn. Run unsubscribes when it returns.
func New(c Controller, in io.Reader, out io.Writer, opts Options) *Frontend {
	b := newBridge()
	return &Frontend{c: c, in: in, out: out, opts: opts, bridge: b, unsubscribe: c.Subscribe(b.push)}
}

// Run shows the UI until /quit, ctx cancellation, the quit key on an empty
// draft, or two cancels (the cancel key or SIGINT) in a row at idle with an
// empty draft. interrupts delivers SIGINT.
func (f *Frontend) Run(ctx context.Context, interrupts <-chan os.Signal) error {
	defer f.unsubscribe()
	defer f.bridge.close()
	pruneImages(f.opts.ImageDir, time.Now())

	p := tea.NewProgram(newModel(ctx, f.c, f.bridge, f.opts),
		tea.WithInput(f.in), tea.WithOutput(f.out), tea.WithoutSignalHandler())
	stopped := make(chan struct{})
	defer close(stopped)
	// Send and Quit return once the program has stopped.
	go func() {
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				p.Quit()
				return
			case <-interrupts:
				p.Send(interruptMsg{})
			}
		}
	}()
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run the terminal UI: %w", err)
	}
	if hint := exitHint(f.c.Status()); hint != "" {
		_, _ = fmt.Fprintln(f.out, hint)
	}
	return nil
}

// exitHint names how to resume a session that has any messages.
func exitHint(s app.Status) string {
	if s.MessageCount == 0 {
		return ""
	}
	return "Resume with: kopi -r " + s.SessionID + " --model '" + strings.ReplaceAll(s.Model, "'", "'\"'\"'") + "'"
}
