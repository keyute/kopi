package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
)

func TestSideAnswerChargesTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	for i, call := range []SideCall{ClassifierCall, FetchCall, SearchCall} {
		text, err := h.c.SideAnswer(t.Context(), call, priced, pigruntime.DefaultEffort, "system", "prompt")

		require.NoError(t, err)
		assert.Equal(t, "priced answer", text)
		assert.Equal(t, float64(i+1)*pigruntimetest.PricedModelCost, h.c.Status().TotalCost)
	}
}

func TestSideChargeAfterASessionSwitchAddsNothing(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	h := newHarness(t, sb, Config{Model: sb.PricedModel(t)})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	charge := h.c.chargeSide(FetchCall)

	require.NoError(t, h.c.NewSession(t.Context()))
	charge(5)

	assert.Zero(t, h.c.Status().TotalCost)
}

func TestNewSessionZeroesTheTotal(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	priced := sb.PricedModel(t)
	h := newHarness(t, sb, Config{Model: priced})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	_, err := h.c.SideAnswer(t.Context(), ClassifierCall, priced, pigruntime.DefaultEffort, "system", "prompt")
	require.NoError(t, err)
	require.Positive(t, h.c.Status().TotalCost)

	require.NoError(t, h.c.NewSession(t.Context()))

	assert.Zero(t, h.c.Status().TotalCost)
}
