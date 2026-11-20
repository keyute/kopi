package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

// loginRun is the one running login. Controller.mu guards prompt.
type loginRun struct {
	provider string
	cancel   context.CancelFunc
	// done is closed once the login has ended and nothing of it can reach the
	// runtime or the relay any more.
	done   chan struct{}
	prompt *pendingLogin
}

// pendingLogin is the question waiting for AnswerLogin. The login asks one
// question at a time.
type pendingLogin struct {
	token uint64
	// answer has room for the one answer, so AnswerLogin never blocks.
	answer chan string
}

// beginLogin registers a login of provider and returns its context, which
// ends when the login is cancelled. It refuses while a turn runs or a subagent
// or workflow is running, a switch is under way or another login runs.
func (c *Controller) beginLogin(ctx context.Context, provider string) (*loginRun, context.Context, error) {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return nil, nil, ErrNoSession
	case c.login != nil:
		return nil, nil, ErrLoggingIn
	case c.switching != nil:
		return nil, nil, ErrBusy
	case c.main.work != nil || c.live > 0 || len(c.workflowRuns) > 0:
		return nil, nil, fmt.Errorf("failed to log in to %q: %w", provider, ErrWorkStillRunning)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &loginRun{provider: provider, cancel: cancel, done: make(chan struct{})}
	c.login = run
	return run, ctx, nil
}

// runLogin signs in to provider through the runtime and returns its notice. It
// runs only for the registered login, which ends when ctx does.
func (c *Controller) runLogin(ctx context.Context, provider string) (string, error) {
	c.relay.emit(event.Activity{Text: fmt.Sprintf("Logging in to %s…", provider)})
	defer c.relay.emit(event.Activity{})
	return c.rt.Login(ctx, provider, pigruntime.LoginUI{
		Ask: func(ctx context.Context, p pigruntime.LoginPrompt) (string, error) {
			return c.askLogin(ctx, provider, p)
		},
		Show: func(text string) { c.relay.emit(event.Notice{Text: text}) },
	})
}

func (c *Controller) askLogin(ctx context.Context, provider string, p pigruntime.LoginPrompt) (string, error) {
	pl := &pendingLogin{answer: make(chan string, 1)}
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return "", err
	}
	token, end := c.openRequestLocked(func(token uint64) { c.relay.emit(event.LoginEnded{Token: token}) })
	pl.token = token
	c.login.prompt = pl
	c.mu.Unlock()

	req := event.LoginRequested{Token: pl.token, Provider: provider, Message: p.Message, Placeholder: p.Placeholder, Secret: p.Secret}
	for _, o := range p.Options {
		req.Options = append(req.Options, event.LoginOption{ID: o.ID, Label: o.Label, Description: o.Description})
	}
	c.relay.emit(req)
	defer end()
	select {
	case a := <-pl.answer:
		return a, nil
	case <-ctx.Done():
		c.mu.Lock()
		if c.login.prompt == pl {
			c.login.prompt = nil
		}
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

// AnswerLogin answers the login's pending question for token. It returns false
// when no question for token is pending.
func (c *Controller) AnswerLogin(token uint64, answer string) bool {
	c.mu.Lock()
	if c.login == nil || c.login.prompt == nil || c.login.prompt.token != token {
		c.mu.Unlock()
		return false
	}
	pl := c.login.prompt
	c.login.prompt = nil
	c.mu.Unlock()
	pl.answer <- answer
	return true
}

func (c *Controller) loginCommand(ctx context.Context, provider string) error {
	if provider == "" {
		return fmt.Errorf("%w: /login needs a provider", ErrMissingArgument)
	}
	run, ctx, err := c.beginLogin(ctx, provider)
	if err != nil {
		return err
	}
	go c.loginTask(run, ctx, nil)
	return nil
}

func (c *Controller) modelProvider() string {
	provider, _, _ := strings.Cut(c.Model(), "/")
	return provider
}

// withLoginHint is err, a missing credential of the session model's provider
// or, with no model yet, of every provider, pointing at /login.
func (c *Controller) withLoginHint(err error) error {
	provider := cmp.Or(c.modelProvider(), "<provider>")
	return fmt.Errorf("%w: run /login %s", err, provider)
}

// canLoginFirst reports whether err, the check's error, is only that the
// session model's provider has no credential, that provider offers a login and
// a human can run it. With no model yet, any provider offering a login will do.
func (c *Controller) canLoginFirst(ctx context.Context, err error) bool {
	if c.cfg.Unattended || !errors.Is(err, pigruntime.ErrNoCredential) {
		return false
	}
	providers, err := c.rt.Providers(ctx)
	if err != nil {
		return false
	}
	provider := c.modelProvider()
	return slices.ContainsFunc(providers, func(p pigruntime.Provider) bool {
		return (provider == "" || p.ID == provider) && len(LoginMethods(p)) > 0
	})
}

// loginFirst starts the login of the session model's provider, or of one the
// human picks when there is no model yet, for the first open req, which Start
// put off. missing is the Error reported when the login
// does not end in a session.
func (c *Controller) loginFirst(ctx context.Context, req OpenRequest, missing error) error {
	run, ctx, err := c.beginLogin(ctx, c.modelProvider())
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.deferred = &req
	c.mu.Unlock()
	go c.loginTask(run, ctx, missing)
	return nil
}

// loginTask runs the registered login and reports its end. Not in wg: Close
// waits on done instead, since it needs no session. A successful login with a
// deferred first open starts that open while still registered, so prompts
// never see a moment with neither and no other login can start in between. A
// cancelled or failed login of the first open also reports missing.
func (c *Controller) loginTask(run *loginRun, ctx context.Context, missing error) {
	defer close(run.done)
	defer run.cancel()
	defer c.endLogin()
	provider := run.provider
	var notice string
	var err error
	if provider == "" {
		provider, err = c.chooseProvider(ctx)
	}
	if err == nil {
		notice, err = c.runLogin(ctx, provider)
	}
	if err != nil {
		if ctx.Err() != nil {
			c.relay.emit(event.Notice{Text: "Login cancelled."})
		} else {
			c.relay.emit(event.Error{Err: err})
		}
		if missing != nil && !c.isClosed() {
			c.relay.emit(event.Error{Err: missing})
		}
		return
	}
	notices := []event.Event{event.Notice{Text: fmt.Sprintf("Logged in to %s.", provider)}}
	if notice != "" {
		notices = append(notices, event.Notice{Text: notice})
	}
	c.mu.Lock()
	req := c.deferred
	c.mu.Unlock()
	if req != nil {
		// The open reports the notices itself: its SessionChanged wipes the transcript.
		var chosen []event.Event
		chosen, err = c.check(context.WithoutCancel(ctx), *req)
		notices = append(notices, chosen...)
		if err == nil {
			err = c.startFirst(ctx, *req, notices)
			if err == nil {
				// The open has started; a failed one is retried with /new or /resume.
				c.mu.Lock()
				c.deferred = nil
				c.mu.Unlock()
			} else if !errors.Is(err, ErrNoSession) {
				c.relay.emit(event.Error{Err: err})
			}
			return
		}
	}
	for _, ev := range notices {
		c.relay.emit(ev)
	}
	if err != nil {
		if errors.Is(err, pigruntime.ErrNoCredential) {
			err = c.withLoginHint(err)
		}
		c.relay.emit(event.Error{Err: err})
	}
}

// chooseProvider asks which provider to log in to, among those that offer a
// login.
func (c *Controller) chooseProvider(ctx context.Context) (string, error) {
	candidates, err := c.loginCandidates()
	if err != nil {
		return "", err
	}
	prompt := pigruntime.LoginPrompt{Message: "Log in to a provider:"}
	for _, cand := range candidates {
		prompt.Options = append(prompt.Options, pigruntime.LoginOption{ID: cand.Value, Label: cand.Value, Description: cand.Description})
	}
	return c.askLogin(ctx, "", prompt)
}

// endLogin deregisters the login and wakes the model for the notices that
// queued meanwhile, which queueNotice left alone. It takes queue before mu, as
// queueNotice does, so no notice falls between the two.
func (c *Controller) endLogin() {
	c.queue.Lock()
	defer c.queue.Unlock()
	c.mu.Lock()
	c.login = nil
	c.main.wakeLocked()
}

func (c *Controller) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Controller) logoutCommand(ctx context.Context, provider string) error {
	if provider == "" {
		return fmt.Errorf("%w: /logout needs a provider", ErrMissingArgument)
	}
	// Checked before life, which a switch holds while it joins old work: the TUI
	// submits on its event loop.
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return ErrNoSession
	case c.login != nil:
		c.mu.Unlock()
		return ErrLoggingIn
	case c.switching != nil:
		c.mu.Unlock()
		return ErrBusy
	}
	c.mu.Unlock()
	// Held so that it cannot touch the runtime after Close.
	c.life.Lock()
	defer c.life.Unlock()
	if c.isClosed() {
		return ErrNoSession
	}
	if err := c.rt.Logout(ctx, provider); err != nil {
		return err
	}
	c.relay.emit(event.Notice{Text: fmt.Sprintf("Logged out of %s.", provider)})
	return nil
}

// LoginMethods names the logins p offers: its account sign-in, then "API key".
func LoginMethods(p pigruntime.Provider) []string {
	var methods []string
	if p.SignIn != "" {
		methods = append(methods, p.SignIn)
	}
	if p.APIKey {
		methods = append(methods, "API key")
	}
	return methods
}

func (c *Controller) loginCandidates() ([]Candidate, error) {
	providers, err := c.rt.Providers(context.Background())
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, p := range providers {
		methods := LoginMethods(p)
		if len(methods) == 0 {
			continue
		}
		desc := strings.Join(methods, ", ")
		if p.LoggedIn {
			desc += " (logged in)"
		}
		out = append(out, Candidate{Value: p.ID, Description: desc})
	}
	return out, nil
}

func (c *Controller) logoutCandidates() ([]Candidate, error) {
	providers, err := c.rt.Providers(context.Background())
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, p := range providers {
		if p.LoggedIn {
			out = append(out, Candidate{Value: p.ID})
		}
	}
	return out, nil
}
