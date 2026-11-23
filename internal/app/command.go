package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

type Command struct {
	Name, Summary string
	// Argument reports that the command's argument is chosen from Arguments.
	Argument bool
	// Optional reports that the command also runs without that argument.
	Optional bool
	// Skill reports that the command is a skill, which loads with the prompt it
	// is typed in.
	Skill bool
}

type command struct {
	Command
	run  func(c *Controller, ctx context.Context, args string) error
	args func(c *Controller) ([]Candidate, error)
}

func commandTable() []command {
	return []command{
		{Command: Command{Name: "plan", Summary: "switch to plan mode (workspace read-only); /plan open edits the plan file", Optional: true}, run: (*Controller).planCommand, args: (*Controller).planCandidates},
		{Command: Command{Name: "execute", Summary: "switch to execute mode"}, run: func(c *Controller, _ context.Context, _ string) error {
			c.EnterExecute()
			return nil
		}},
		{Command: Command{Name: "effort", Summary: "set the reasoning effort for this session"}, run: (*Controller).effortCommand, args: (*Controller).effortCandidates},
		{Command: Command{Name: "model", Summary: "switch the model until kopi exits"}, run: (*Controller).modelCommand, args: (*Controller).modelCandidates},
		{Command: Command{Name: "new", Summary: "start a new session"}, run: func(c *Controller, ctx context.Context, _ string) error {
			return c.switchTo(ctx, OpenRequest{})
		}},
		{Command: Command{Name: "resume", Summary: "resume a session"}, run: (*Controller).resumeCommand, args: (*Controller).sessionCandidates},
		{Command: Command{Name: "login", Summary: "log in to a model provider"}, run: (*Controller).loginCommand, args: (*Controller).loginCandidates},
		{Command: Command{Name: "logout", Summary: "remove a model provider's stored credential"}, run: (*Controller).logoutCommand, args: (*Controller).logoutCandidates},
		{Command: Command{Name: "tasks", Summary: "list and stop background tasks"}, run: func(*Controller, context.Context, string) error {
			return ErrShowTasks
		}},
		{Command: Command{Name: "usage", Summary: "show context, plan limits and cost"}, run: func(*Controller, context.Context, string) error {
			return ErrShowUsage
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

// leadingWord is the input's first whitespace-delimited word without its "/".
func leadingWord(text string) string {
	word, _, _ := strings.Cut(strings.Join(strings.Fields(text), " "), " ")
	return strings.TrimPrefix(word, "/")
}

func isControl(name string) bool {
	return slices.ContainsFunc(commandTable(), func(c command) bool { return c.Name == name })
}

// planCommand enters plan mode; "open" asks the frontend to open the plan file.
func (c *Controller) planCommand(_ context.Context, arg string) error {
	switch arg {
	case "":
		c.EnterPlan()
		return nil
	case "open":
		_, err := os.Stat(c.cfg.Plans.Path())
		if err == nil {
			return ErrOpenPlan
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		c.relay.emit(event.Notice{Text: "This session has no plan file yet."})
		return nil
	}
	return fmt.Errorf("%w: /plan takes no argument or open, got %q", ErrUnknownArgument, arg)
}

func (c *Controller) planCandidates() ([]Candidate, error) {
	return []Candidate{{Value: "open", Description: "edit the plan file"}}, nil
}

func (c *Controller) resumeCommand(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: /resume needs a session id", ErrMissingArgument)
	}
	return c.switchTo(ctx, OpenRequest{ResumeID: id})
}

func (c *Controller) effortCommand(_ context.Context, arg string) error {
	if arg == "" {
		return fmt.Errorf("%w: /effort needs a level", ErrMissingArgument)
	}
	e, err := pigruntime.ParseEffort(arg)
	if err != nil {
		return fmt.Errorf("%w: /effort: %w", ErrUnknownArgument, err)
	}
	// Checked before life, which a switch holds while it joins old work: the TUI
	// submits on its event loop.
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return ErrNoSession
	case c.busyLocked():
		c.mu.Unlock()
		return ErrBusy
	}
	c.mu.Unlock()
	// Held so that it cannot touch the runtime after Close.
	c.life.Lock()
	defer c.life.Unlock()
	c.mu.Lock()
	closed, sess := c.closed, c.main.sess
	c.mu.Unlock()
	if closed || sess == nil {
		return ErrNoSession
	}
	if sess.Effort() == "" {
		c.relay.emit(event.Notice{Text: "This model does not reason."})
		return nil
	}
	if err := sess.SetEffort(e); err != nil {
		return err
	}
	c.relay.emit(event.EffortChanged{})
	c.relay.emit(event.Notice{Text: fmt.Sprintf("Effort set to %s for this session.", sess.Effort())})
	return nil
}

func (c *Controller) effortCandidates() ([]Candidate, error) {
	c.mu.Lock()
	sess := c.main.sess
	c.mu.Unlock()
	if sess == nil {
		return nil, nil
	}
	current := sess.Effort()
	if current == "" {
		return nil, nil
	}
	var out []Candidate
	for _, name := range sess.Efforts() {
		cand := Candidate{Value: name}
		if name == current {
			cand.Description = "current"
		}
		out = append(out, cand)
	}
	return out, nil
}

// modelCommand switches the main session to ref in the background; a failure
// after the checks arrives as an Error event. Later sessions and work that
// names no model follow it, among them a running workflow's later agent() calls
// and the WebFetch, WebSearch and classifier calls of running agents; running
// subagents keep their model. It marks the change before returning, so no turn
// starts, and no switch or login begins, until the model has changed. The
// goroutine is not in wg, for the reason switchTo's is not.
func (c *Controller) modelCommand(ctx context.Context, ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: /model needs a provider/id", ErrMissingArgument)
	}
	provider, _, err := config.ParseModelRef(ref)
	if err != nil {
		return fmt.Errorf("%w: /model: %w", ErrUnknownArgument, err)
	}
	sess, err := c.beginModelChange()
	if err != nil {
		return err
	}
	go func() {
		// Held so that it cannot touch the runtime after Close.
		c.life.Lock()
		defer c.life.Unlock()
		defer c.endModelChange()
		if c.isClosed() {
			return
		}
		if err := c.changeModel(ctx, sess, provider, ref); err != nil {
			c.relay.emit(event.Error{Err: err})
		}
	}()
	return nil
}

func (c *Controller) changeModel(ctx context.Context, sess *pigruntime.Session, provider, ref string) error {
	saveErr := c.rt.SetModel(ctx, sess, ref)
	if saveErr != nil && !errors.Is(saveErr, pigruntime.ErrModelNotSaved) {
		if errors.Is(saveErr, pigruntime.ErrNoCredential) {
			return loginHint(saveErr, provider)
		}
		return saveErr
	}
	c.mu.Lock()
	c.model = ref
	c.mu.Unlock()
	c.offerSearch(sess)
	c.limits.force()
	c.relay.emit(event.ModelChanged{Model: ref})
	if saveErr != nil {
		return saveErr
	}
	c.relay.emit(event.Notice{Text: fmt.Sprintf("Using %s until kopi exits. Set \"model\" in %s to keep it.", ref, c.cfg.ConfigFile)})
	return nil
}

// beginModelChange returns the main session once nothing prevents changing its
// model, and marks the change in the same section that checked for work, so a
// wake cannot start a turn before endModelChange.
func (c *Controller) beginModelChange() (*pigruntime.Session, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return nil, ErrNoSession
	case c.login != nil:
		return nil, ErrLoggingIn
	case c.busyLocked():
		return nil, ErrBusy
	case c.main.work != nil:
		return nil, fmt.Errorf("failed to change model: %w", ErrWorkStillRunning)
	case c.main.sess == nil:
		return nil, ErrNoSession
	}
	c.changingModel = true
	return c.main.sess, nil
}

// endModelChange ends the mark and starts the turn a notice that arrived
// meanwhile asks for.
func (c *Controller) endModelChange() {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	c.changingModel = false
	c.main.wakeLocked()
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

func (c *Controller) modelCandidates() ([]Candidate, error) {
	models, err := c.rt.Models(context.Background())
	if err != nil {
		return nil, err
	}
	current := c.Model()
	out := make([]Candidate, 0, len(models))
	for _, m := range models {
		desc := m.Name
		if m.Ref == current {
			desc += " (current)"
		}
		out = append(out, Candidate{Value: m.Ref, Description: desc})
	}
	return out, nil
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
