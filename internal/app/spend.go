package app

import (
	"context"
	"maps"

	"github.com/keyute/kopi/internal/pigruntime"
)

// SideCall names a request Kopi makes outside any session, for the ledger.
type SideCall int

const (
	ClassifierCall SideCall = iota
	FetchCall
	SearchCall
	sideCalls
)

// spend is what the root session has spent beyond what its open sessions
// report: side requests and workflow agents' sessions, which close when the
// agent ends. Costs are estimated dollars. Controller.mu guards it.
type spend struct {
	side      [sideCalls]float64
	workflows map[string]float64 // by workflow task id
}

func (s *spend) sum() float64 {
	var total float64
	for _, v := range s.side {
		total += v
	}
	for v := range maps.Values(s.workflows) {
		total += v
	}
	return total
}

func (s *spend) addWorkflow(id string, cost float64) {
	if s.workflows == nil {
		s.workflows = map[string]float64{}
	}
	s.workflows[id] += cost
}

// chargeSide snapshots the root session and returns a func that adds a cost to
// call's bucket only if that session is still the root, so a request that
// outlives a session switch charges nothing to the new one.
func (c *Controller) chargeSide(call SideCall) func(cost float64) {
	c.mu.Lock()
	root := c.main.sess
	c.mu.Unlock()
	return func(cost float64) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.main.sess == root {
			c.spend.side[call] += cost
		}
	}
}

// SideAnswer has model reply once to prompt under system, outside any session,
// and charges its cost to call in the session total.
func (c *Controller) SideAnswer(ctx context.Context, call SideCall, model string, effort pigruntime.Effort, system, prompt string) (string, error) {
	charge := c.chargeSide(call)
	text, cost, err := c.rt.Answer(ctx, model, effort, system, prompt)
	charge(cost)
	return text, err
}
