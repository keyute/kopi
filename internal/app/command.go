package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

type Command struct {
	Name, Usage, Summary string
}

type command struct {
	Command
	run  func(c *Controller, ctx context.Context, args string) error
	args func(c *Controller) ([]Candidate, error)
}

func commandTable() []command {
	return []command{
		{Command: Command{"plan", "/plan", "switch to plan mode (read-only tools)"}, run: func(c *Controller, _ context.Context, _ string) error {
			c.EnterPlan()
			return nil
		}},
		{Command: Command{"execute", "/execute", "switch to execute mode"}, run: func(c *Controller, _ context.Context, _ string) error {
			c.EnterExecute()
			return nil
		}},
		{Command: Command{"new", "/new", "start a new session"}, run: func(c *Controller, ctx context.Context, _ string) error {
			return c.switchTo(ctx, OpenRequest{})
		}},
		{Command: Command{"resume", "/resume [id]", "resume a session, or list sessions without an id"}, run: (*Controller).resumeCommand, args: (*Controller).sessionCandidates},
		{Command: Command{"quit", "/quit", "exit kopi"}, run: func(*Controller, context.Context, string) error {
			return ErrQuit
		}},
	}
}

// parse reports whether line is a control command. A "//" prefix is the
// escape for a prompt that starts with "/", so it is not a command.
func parse(line string) (name, args string, isCommand bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") || strings.HasPrefix(line, "//") {
		return "", "", false
	}
	name, args, _ = strings.Cut(line[1:], " ")
	return name, strings.TrimSpace(args), true
}

func (c *Controller) resumeCommand(ctx context.Context, id string) error {
	if id != "" {
		return c.switchTo(ctx, OpenRequest{ResumeID: id})
	}
	// The listing reads every session file, so it runs in the background. It
	// holds life so that it cannot touch the runtime after Close.
	go func() {
		c.life.Lock()
		defer c.life.Unlock()
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		if err := c.listSessions(); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
	return nil
}

func (c *Controller) listSessions() error {
	sessions, err := c.Sessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		c.notice("no saved sessions for this workspace")
		return nil
	}
	var b strings.Builder
	b.WriteString("sessions (newest first):")
	for _, s := range sessions {
		fmt.Fprintf(&b, "\n  %s  %s", s.ID, describeSession(s))
	}
	c.notice(b.String())
	return nil
}

// Arguments returns the candidates for command name's argument; nil when it takes
// none. It may read every session file, so frontends call it off their event loop.
func (c *Controller) Arguments(name string) ([]Candidate, error) {
	cmds := commandTable()
	i := slices.IndexFunc(cmds, func(cmd command) bool { return cmd.Name == name })
	if i < 0 || cmds[i].args == nil {
		return nil, nil
	}
	// Held so that it cannot touch the runtime after Close.
	c.life.Lock()
	defer c.life.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, nil
	}
	return cmds[i].args(c)
}

func (c *Controller) sessionCandidates() ([]Candidate, error) {
	sessions, err := c.Sessions()
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, Candidate{Value: s.ID, Description: describeSession(s)})
	}
	return out, nil
}

func describeSession(s pigruntime.SessionInfo) string {
	first, _, _ := strings.Cut(s.FirstMessage, "\n")
	return s.Modified.Local().Format(time.DateTime) + "  " + first
}
