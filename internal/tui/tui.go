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
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// Controller is the part of app.Controller the frontend uses.
type Controller interface {
	Subscribe(fn func(event.Event)) func()
	Submit(ctx context.Context, session, text string, images ...string) (*app.Work, error)
	SendNow(ctx context.Context, session, text string, images ...string) error
	TakeQueued(session string) []event.Input
	Interrupt(session string) bool
	// Answer answers the pending approval for token; false when it is stale.
	Answer(token uint64, d policy.Decision) bool
	// AnswerLogin answers the pending login question for token; false when it
	// is stale.
	AnswerLogin(token uint64, answer string) bool
	// AnswerQuestion answers the pending questions for token, one answer per
	// question; nil dismisses them. False when it is stale.
	AnswerQuestion(token uint64, answers []tool.Answer) bool
	// AnswerPlan answers the pending plan for token; false when it is stale.
	AnswerPlan(token uint64, a tool.PlanAnswer) bool
	// RefreshPlan reads the plan file again, for after the human edited it,
	// and repeats the plan's request; false when it is stale.
	RefreshPlan(token uint64) bool
	// PlanFile is the session's plan file, "" when it has none.
	PlanFile() string
	ToggleMode()
	History() []string
	Status() app.Status
	Agents() []app.AgentStatus
	Commands() []app.Command
	Spans(session, text string) []app.Span
	// Arguments and Files may read many files; call them off the event loop.
	Arguments(name string) ([]app.Candidate, error)
	Files() ([]app.Candidate, error)
	// Background moves the live foreground command to the background; false
	// when there was none.
	Background() bool
	Tasks() []tool.TaskInfo
	TaskOutput(id string) (tool.TaskInfo, error)
	StopTask(id string) error
}

// Options are the frontend's settings. Keys maps action names to key
// strings; an action without keys is disabled. StatusLine is the config's
// status line, AgentLine its agent rows'. ImageDir holds saved clipboard images. Themes
// come from LoadThemes.
type Options struct {
	Version    string
	Keys       map[string][]string
	StatusLine config.StatusLine
	AgentLine  config.AgentLine
	ImageDir   string
	Themes     Themes
}

type statusCommands interface {
	Start(context.Context, func(event.Event))
	Observe(event.Event)
	Close()
}

type Frontend struct {
	c           Controller
	in          io.Reader
	out         io.Writer
	opts        Options
	bridge      *bridge
	commands    statusCommands
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if f.commands == nil {
		f.commands = app.NewStatusCommands(f.c.Status().Workspace, f.opts.StatusLine)
	}
	f.commands.Start(ctx, f.bridge.push)
	defer f.commands.Close()
	defer f.unsubscribe()
	defer f.bridge.close()
	pruneImages(f.opts.ImageDir, time.Now())

	m := newModel(ctx, f.c, f.bridge, f.opts)
	m.commands = f.commands
	p := tea.NewProgram(m,
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
				if !m.editing.Load() {
					p.Send(interruptMsg{})
				}
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
