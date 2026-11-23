package app

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// chargeSide snapshots the root session and returns a func that adds a request's
// usage to the root's side spend only if that session is still the root, so a
// request that outlives a session switch charges nothing to the new one.
func (c *Controller) chargeSide() func(used pigruntime.Usage) {
	c.mu.Lock()
	root := c.main.sess
	c.mu.Unlock()
	return func(used pigruntime.Usage) { c.charge(root, used) }
}

func (c *Controller) charge(root *pigruntime.Session, used pigruntime.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.main.sess == root {
		c.spent = c.spent.Add(used)
	}
}

// SideAnswer has model reply once to prompt under system, outside any session,
// and charges its usage to the session total.
func (c *Controller) SideAnswer(ctx context.Context, model string, effort pigruntime.Effort, system, prompt string) (string, error) {
	charge := c.chargeSide()
	text, used, err := c.rt.Answer(ctx, model, effort, system, prompt)
	charge(used)
	return text, err
}

// sideSearch has model search the web for query under system, outside any
// session, and charges its usage to the session total.
func (c *Controller) sideSearch(ctx context.Context, model, system, query string) (tool.SearchResult, error) {
	charge := c.chargeSide()
	result, used, err := c.rt.Search(ctx, model, system, query)
	charge(used)
	return result, err
}

// Usage returns what the current session has used, summed over the sources
// Status.TotalCost sums. It also asks for a throttled read of the plan limits,
// which Limits shows once LimitsChanged arrives.
func (c *Controller) Usage() Usage {
	c.limits.refresh()
	c.queue.Lock()
	children := maps.Clone(c.children)
	c.mu.Lock()
	sess, sessID := c.main.sess, c.sessID
	used := c.spent
	c.mu.Unlock()
	c.queue.Unlock()

	u := Usage{SessionID: sessID, Model: c.Model(), Limits: c.limits.get()}
	if sess != nil {
		u.ToolCalls, _ = sess.Usage()
		u.Messages = sess.LoggedMessages()
		if tokens, window, ok := sess.ContextUsage(); ok {
			u.ContextTokens, u.ContextWindow = tokens, window
			parts, tools := sess.ContextSizes()
			u.Context = contextOf(tokens, parts, tools)
		}
		u.CompactAt, _ = sess.CompactAt()
		rec := sess.Record()
		u.Cache, u.Compactions, u.ModelTime, u.ToolTime = rec.Cache, rec.Compactions, rec.ModelTime, rec.ToolTime
	}
	used = totalUsage(used, sess, children)
	u.Tokens, u.Models = used.Tokens, slices.Clone(used.Models)
	slices.SortFunc(u.Models, func(a, b ModelUsage) int {
		return cmp.Or(cmp.Compare(b.Cost, a.Cost), cmp.Compare(a.Model, b.Model))
	})
	return u
}

// totalUsage is side with what the root session sess, if any, and the open
// children have used: the sum Status.TotalCost and Usage both report.
func totalUsage(side pigruntime.Usage, sess *pigruntime.Session, children map[string]*runner) pigruntime.Usage {
	if sess != nil {
		side = side.Add(sess.UsageByModel())
	}
	for _, r := range children {
		side = side.Add(r.sess.UsageByModel())
	}
	return side
}
