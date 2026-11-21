package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
)

const (
	titleSystem = "Write a title of at most six words for a coding session that begins with the user's message. Reply with the title only."
	// titlePromptBytes and titleBytes bound what is sent for the title and what is kept of it.
	titlePromptBytes = 2000
	titleBytes       = 60
)

// nameSessionLocked names the main session from the first human prompt in ps,
// the prompts about to reach its model, unless Title is off, ps holds none or
// the title was already decided on for this session. It must be called with
// queue held. The request never delays the turn.
func (c *Controller) nameSessionLocked(ps []pigruntime.Prompt) {
	if !c.cfg.Title {
		return
	}
	i := slices.IndexFunc(ps, func(p pigruntime.Prompt) bool { return !p.FromAgent && strings.TrimSpace(p.Text) != "" })
	if i < 0 {
		return
	}
	prompt := ps[i].Text
	c.mu.Lock()
	sess, bound := c.main.sess, c.main.bound
	if c.titled || sess == nil {
		c.mu.Unlock()
		return
	}
	c.titled = true
	c.mu.Unlock()
	c.wg.Go(func() { c.nameSession(sess, bound, prompt) })
}

// nameSession stores and announces the title of sess unless it is no
// longer the main session when the model answers. bound belongs
// to sess's relay epoch.
func (c *Controller) nameSession(sess *pigruntime.Session, bound func(event.Event), prompt string) {
	title, err := c.title(c.titleCtx, sess, prompt)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("failed to title session", "err", err)
		}
		return
	}
	c.titleCommit.Lock()
	defer c.titleCommit.Unlock()
	c.mu.Lock()
	current := c.main.sess == sess
	c.mu.Unlock()
	if !current {
		return
	}
	if err := sess.SetName(title); err != nil {
		slog.Warn("failed to title session", "err", err)
		return
	}
	bound(event.SessionNamed{Name: title})
}

// title asks the small model for a title for a session that begins with prompt.
func (c *Controller) title(ctx context.Context, sess *pigruntime.Session, prompt string) (string, error) {
	reply, cost, err := c.rt.Answer(ctx, c.SmallModel(), pigruntime.LowestEffort, titleSystem, truncate(prompt, titlePromptBytes))
	c.charge(sess, cost)
	if err != nil {
		return "", fmt.Errorf("failed to ask for a title: %w", err)
	}
	title := cleanTitle(reply)
	if title == "" {
		return "", fmt.Errorf("failed to ask for a title: %w", pigruntime.ErrNoAnswer)
	}
	return title, nil
}

// cleanTitle reduces a reply to one bounded line without quotes or control
// characters; "" when nothing is left.
func cleanTitle(reply string) string {
	line := ""
	for l := range strings.Lines(reply) {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(strings.Trim(line, "\"'`“”‘’"))
	return strings.TrimSpace(truncate(line, titleBytes))
}
