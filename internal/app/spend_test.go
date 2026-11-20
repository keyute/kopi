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
	charge(5)

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
