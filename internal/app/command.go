package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Command struct {
	Name, Summary string
	// Argument reports that the command's argument is chosen from Arguments.
	Argument bool
}

type command struct {
	Command
	run  func(c *Controller, ctx context.Context, args string) error
	args func(c *Controller) ([]Candidate, error)
}

func commandTable() []command {
	return []command{
		{Command: Command{Name: "plan", Summary: "switch to plan mode (workspace read-only)"}, run: func(c *Controller, _ context.Context, _ string) error {
			c.EnterPlan()
			return nil
		}},
		{Command: Command{Name: "execute", Summary: "switch to execute mode"}, run: func(c *Controller, _ context.Context, _ string) error {
			c.EnterExecute()
			return nil
		}},
		{Command: Command{Name: "new", Summary: "start a new session"}, run: func(c *Controller, ctx context.Context, _ string) error {
			return c.switchTo(ctx, OpenRequest{})
		}},
		{Command: Command{Name: "resume", Summary: "resume a session"}, run: (*Controller).resumeCommand, args: (*Controller).sessionCandidates},
		{Command: Command{Name: "login", Summary: "log in to a model provider"}, run: (*Controller).loginCommand, args: (*Controller).loginCandidates},
		{Command: Command{Name: "logout", Summary: "remove a model provider's stored credential"}, run: (*Controller).logoutCommand, args: (*Controller).logoutCandidates},
		{Command: Command{Name: "tasks", Summary: "list and stop background tasks"}, run: func(*Controller, context.Context, string) error {
			return ErrShowTasks
		}},
		{Command: Command{Name: "quit", Summary: "exit kopi"}, run: func(*Controller, context.Context, string) error {
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
	if id == "" {
		return fmt.Errorf("%w: /resume needs a session id", ErrMissingArgument)
	}
	return c.switchTo(ctx, OpenRequest{ResumeID: id})
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
		first, _, _ := strings.Cut(s.FirstMessage, "\n")
		out = append(out, Candidate{Value: s.ID, Label: s.Modified.Local().Format(time.DateTime), Description: first})
	}
	return out, nil
}
