package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/tool"
)

// spentCost is a request that cost dollars, to charge by hand.
func spentCost(dollars float64) pigruntime.Usage {
	return pigruntime.Usage{Models: []pigruntime.ModelUsage{{Model: "p/m", Cost: dollars}}}
}

func TestSideAnswerChargesTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	for i := range 2 {
		text, err := h.c.SideAnswer(t.Context(), priced, pigruntime.DefaultEffort, "system", "prompt")

		require.NoError(t, err)
		assert.Equal(t, "priced answer", text)
		assert.Equal(t, float64(i+1)*pigruntimetest.PricedModelCost, h.c.Status().TotalCost)
	}
}

func TestSideChargeAfterASessionSwitchAddsNothing(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	h := newHarness(t, sb, Config{Model: sb.PricedModel(t)})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	charge := h.c.chargeSide()

	require.NoError(t, h.c.NewSession(t.Context()))
	charge(spentCost(5))

	assert.Zero(t, h.c.Status().TotalCost)
}

func TestSideAnswerOutlivingASessionSwitchChargesNothing(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	priced := sb.HeldPricedModel(t, reached, release)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	done := make(chan error, 1)
	go func() {
		_, err := h.c.SideAnswer(t.Context(), priced, pigruntime.DefaultEffort, "system", "prompt")
		done <- err
	}()
	wait(t, reached)

	require.NoError(t, h.c.NewSession(t.Context()))
	close(release)

	require.NoError(t, <-done)
	assert.Zero(t, h.c.Status().TotalCost)
}

func TestNewSessionZeroesTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	_, err := h.c.SideAnswer(t.Context(), priced, pigruntime.DefaultEffort, "system", "prompt")
	require.NoError(t, err)
	require.Positive(t, h.c.Status().TotalCost)

	require.NoError(t, h.c.NewSession(t.Context()))

	assert.Zero(t, h.c.Status().TotalCost)
}

func TestUsageRowsSumToTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.turn(t, "price me")
	_, err := h.c.SideAnswer(t.Context(), priced, pigruntime.DefaultEffort, "system", "prompt")
	require.NoError(t, err)

	got := h.c.Usage()

	assert.Equal(t, h.c.Status().TotalCost, got.TotalCost)
	assert.Equal(t, []ModelUsage{{Model: priced, Tokens: 2_000_000, Cost: 2 * pigruntimetest.PricedModelCost}}, got.Models, "the turn's and the side request's rows merge")
	assert.Equal(t, Tokens{Input: 2_000_000}, got.Tokens)
	assert.Equal(t, h.c.Status().SessionID, got.SessionID)
	assert.Equal(t, priced, got.Model)
	assert.Equal(t, 2, got.Messages)
	assert.Zero(t, got.ToolCalls)
	assert.Positive(t, got.ContextWindow)
	assert.Equal(t, h.c.main.sess.Cache(), got.Cache)
}

func TestUsageCountsARunningSubagent(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	reached, release := make(chan struct{}), make(chan struct{}, 1)
	def := agentDef("explorer")
	def.Model = sb.HeldPricedModel(t, reached, release)
	a := newAgentHarness(t, sb, Config{}, def)
	id := a.launch(t, "explorer", "find it", "go")
	wait(t, reached)
	release <- struct{}{}
	a.untilStatus(t, id, tool.TaskCompleted)
	_, err := a.c.message(t.Context(), id, "again")
	require.NoError(t, err)
	wait(t, reached)

	got := a.c.Usage()

	assert.Equal(t, []ModelUsage{{Model: def.Model, Tokens: 1_000_000, Cost: pigruntimetest.PricedModelCost}}, got.Models, "the second request is still held")
	assert.Equal(t, a.c.Status().TotalCost, got.TotalCost)
	close(release)
}

func TestUsageSortsRowsByCostThenModel(t *testing.T) {
	h, _ := openHarness(t, Config{})
	root := h.c.main.sess
	h.c.charge(root, pigruntime.Usage{Models: []pigruntime.ModelUsage{
		{Model: "p/b", Cost: 1}, {Model: "p/c", Cost: 3}, {Model: "p/a", Cost: 1}, {Model: "", Cost: 2},
	}})

	got := h.c.Usage()

	var order []string
	for _, row := range got.Models {
		order = append(order, row.Model)
	}
	assert.Equal(t, []string{"p/c", "", "p/a", "p/b"}, order)
	assert.Equal(t, 7.0, got.TotalCost)
}

func TestNewSessionEmptiesTheUsage(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	_, err := h.c.SideAnswer(t.Context(), priced, pigruntime.DefaultEffort, "system", "prompt")
	require.NoError(t, err)
	require.NotEmpty(t, h.c.Usage().Models)

	require.NoError(t, h.c.NewSession(t.Context()))

	got := h.c.Usage()
	assert.Empty(t, got.Models)
	assert.Zero(t, got.TotalCost)
}
