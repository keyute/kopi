package app

import (
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
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

	assert.Equal(t, h.c.Status().TotalCost, pigruntime.Usage{Models: got.Models}.Cost())
	assert.Equal(t, []ModelUsage{{Model: priced, Tokens: 2_000_000, Cost: 2 * pigruntimetest.PricedModelCost}}, got.Models, "the turn's and the side request's rows merge")
	assert.Equal(t, Tokens{Input: 2_000_000}, got.Tokens)
	assert.Equal(t, h.c.Status().SessionID, got.SessionID)
	assert.Equal(t, priced, got.Model)
	assert.Equal(t, 2, got.Messages)
}

func TestUsageReadsTheMainSession(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{execTool(&ran)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Text("ok")})
	h.turn(t, "run it")

	got := h.c.Usage()

	assert.Equal(t, 1, got.ToolCalls)
	tokens, window, ok := h.c.main.sess.ContextUsage()
	require.True(t, ok)
	assert.Positive(t, tokens)
	assert.Equal(t, tokens, got.ContextTokens)
	assert.Equal(t, window, got.ContextWindow)
	assert.True(t, got.Cache.Reported, "the second request read what the first cached")
	rec := h.c.main.sess.Record()
	assert.Equal(t, rec.Cache, got.Cache)
	assert.Equal(t, rec.ModelTime, got.ModelTime)
	assert.Equal(t, rec.ToolTime, got.ToolTime)
	assert.Zero(t, got.Compactions)
	at, ok := h.c.main.sess.CompactAt()
	require.True(t, ok)
	assert.Equal(t, at, got.CompactAt)
	c := got.Context
	assert.Positive(t, c.SystemPrompt)
	assert.Positive(t, c.Tools)
	sum := c.SystemPrompt + c.Tools + c.Instructions + c.Skills + c.Agents + c.MCP + c.Messages
	assert.LessOrEqual(t, sum, tokens)
	assert.Less(t, tokens-sum, 6, "the categories are the whole context, each short by under a token where scaled")
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
	assert.Equal(t, a.c.Status().TotalCost, pigruntime.Usage{Models: got.Models}.Cost())
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
	assert.Equal(t, 7.0, pigruntime.Usage{Models: got.Models}.Cost())
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
}
