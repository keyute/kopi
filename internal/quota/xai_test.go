package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXAI(t *testing.T) {
	client, f := fakeClient(http.StatusOK, `{"config":{"creditUsagePercent":12.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-17T00:00:00Z","end":"2026-11-24T00:00:00Z"}}}`)

	got, err := XAI(t.Context(), client, "tok")

	require.NoError(t, err)
	assert.Equal(t, []Window{{Label: "wk", UsedPercent: 12.5, ResetsAt: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}}, got)
	assert.Equal(t, "https://cli-chat-proxy.grok.com/v1/billing?format=credits", f.reqs[0].URL.String())
	assert.Equal(t, "Bearer tok", f.reqs[0].Header.Get("Authorization"))
}

func TestXAIZeroUsageIsOmitted(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"config":{"currentPeriod":{"start":"2026-11-01T00:00:00Z","end":"2026-12-01T00:00:00Z"}}}`)

	got, err := XAI(t.Context(), client, "tok")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "mo", got[0].Label)
	assert.Zero(t, got[0].UsedPercent)
}

func TestXAINoPeriod(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"config":{}}`)

	got, err := XAI(t.Context(), client, "tok")

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestXAIUnparsablePeriod(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"config":{"currentPeriod":{"start":"x","end":"2026-11-24T00:00:00Z"}}}`)

	got, err := XAI(t.Context(), client, "tok")

	require.Error(t, err)
	assert.Nil(t, got)
}

func TestXAIEmptyPeriodHasNoWindow(t *testing.T) {
	for _, period := range []string{
		`"start":"2026-11-24T00:00:00Z","end":"2026-11-24T00:00:00Z"`,
		`"start":"2026-11-24T00:00:00Z","end":"2026-11-17T00:00:00Z"`,
	} {
		client, _ := fakeClient(http.StatusOK, `{"config":{"currentPeriod":{`+period+`}}}`)

		got, err := XAI(t.Context(), client, "tok")

		require.NoError(t, err)
		assert.Empty(t, got, period)
	}
}
