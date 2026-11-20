package app

import (
	"context"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/tool"
)

// chargeSide snapshots the root session and returns a func that adds a cost to
// the root's side spend only if that session is still the root, so a request
// that outlives a session switch charges nothing to the new one.
func (c *Controller) chargeSide() func(cost float64) {
	c.mu.Lock()
	root := c.main.sess
	c.mu.Unlock()
	return func(cost float64) { c.charge(root, cost) }
}

func (c *Controller) charge(root *pigruntime.Session, cost float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.main.sess == root {
		c.spent += cost
	}
}

// SideAnswer has model reply once to prompt under system, outside any session,
// and charges its cost to the session total.
func (c *Controller) SideAnswer(ctx context.Context, model string, effort pigruntime.Effort, system, prompt string) (string, error) {
	charge := c.chargeSide()
	text, cost, err := c.rt.Answer(ctx, model, effort, system, prompt)
	charge(cost)
	return text, err
}

// sideSearch has model search the web for query under system, outside any
// session, and charges its cost to the session total.
func (c *Controller) sideSearch(ctx context.Context, model, system, query string) (tool.SearchResult, error) {
	charge := c.chargeSide()
	result, cost, err := c.rt.Search(ctx, model, system, query)
	charge(cost)
	return result, err
}
