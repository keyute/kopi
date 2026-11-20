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
	// titlePromptRunes and titleRunes bound what is sent for the title and what is kept of it.
	titlePromptRunes = 2000
	titleRunes       = 60
)

// nameSessionLocked names the main session from the first human prompt in ps,
// the prompts about to reach its model, unless Title is off, ps holds none or
// the title was already decided on for this session. It must be called with
// queue held. The request never delays the turn.
func (c *Controller) nameSessionLocked(ps []pigruntime.Prompt) {
	i := slices.IndexFunc(ps, func(p pigruntime.Prompt) bool { return !p.FromAgent && strings.TrimSpace(p.Text) != "" })
	if i < 0 {
		return
	}
	prompt := ps[i].Text
	c.mu.Lock()
	sess, bound := c.main.sess, c.main.bound
	if !c.cfg.Title || c.titled || sess == nil {
		c.mu.Unlock()
		return
	}
	c.titled = true
	c.mu.Unlock()
	c.wg.Go(func() { c.nameSession(sess, bound, prompt) })
}

// nameSession stores and announces the title of sess unless it already has a
// name or a session switch replaced it while the model was answering. bound
// belongs to sess's relay epoch.
func (c *Controller) nameSession(sess *pigruntime.Session, bound func(event.Event), prompt string) {
	if sess.Name() != "" {
		return
	}
	title, err := c.title(c.titleCtx, prompt)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("failed to title session", "err", err)
		}
		return
	}
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
func (c *Controller) title(ctx context.Context, prompt string) (string, error) {
	if r := []rune(prompt); len(r) > titlePromptRunes {
		prompt = string(r[:titlePromptRunes])
	}
	reply, err := c.rt.Answer(ctx, c.SmallModel(), pigruntime.LowestEffort, titleSystem, prompt)
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
	if r := []rune(line); len(r) > titleRunes {
		line = strings.TrimSpace(string(r[:titleRunes]))
	}
	return line
}
