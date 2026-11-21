package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const copilotPaid = `{"copilot_plan":"individual","quota_reset_date":"2026-12-01","quota_reset_date_utc":"2026-12-01T00:00:00.000Z","quota_snapshots":{"premium_interactions":{"entitlement":1500,"remaining":990,"percent_remaining":66.0,"unlimited":false,"overage_permitted":true,"overage_count":0},"chat":{"entitlement":0,"remaining":0,"percent_remaining":100.0,"unlimited":true},"completions":{"entitlement":0,"remaining":0,"percent_remaining":100.0,"unlimited":true}}}`

func TestCopilotPaid(t *testing.T) {
	client, f := fakeClient(http.StatusOK, copilotPaid)

	got, err := Copilot(t.Context(), client, "gh-token", "")

	require.NoError(t, err)
	assert.Equal(t, []Window{{Label: "premium", UsedPercent: 34, ResetsAt: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}}, got)
	require.Len(t, f.reqs, 1)
	assert.Equal(t, "https://api.github.com/copilot_internal/user", f.reqs[0].URL.String())
	assert.Equal(t, "Bearer gh-token", f.reqs[0].Header.Get("Authorization"))
}

func TestCopilotFree(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"copilot_plan":"free","quota_reset_date":"2026-12-01","quota_snapshots":{"premium_interactions":{"percent_remaining":110,"unlimited":false},"chat":{"entitlement":50,"remaining":20,"percent_remaining":40.0,"unlimited":false}}}`)

	got, err := Copilot(t.Context(), client, "gh-token", "")

	require.NoError(t, err)
	reset := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, []Window{
		{Label: "premium", UsedPercent: 0, ResetsAt: reset},
		{Label: "chat", UsedPercent: 60, ResetsAt: reset},
	}, got, "a date-only reset is read, and used is floored at zero")
}

func TestCopilotEnterpriseHost(t *testing.T) {
	client, f := fakeClient(http.StatusOK, copilotPaid)

	_, err := Copilot(t.Context(), client, "gh-token", "octo.ghe.com")

	require.NoError(t, err)
	assert.Equal(t, "https://api.octo.ghe.com/copilot_internal/user", f.reqs[0].URL.String())
}

func TestCopilotRefusesNonHostname(t *testing.T) {
	for _, domain := range []string{"evil.test/x", "evil.test@host", "a b", "host:80", "-x.test", "x.test?"} {
		client, f := fakeClient(http.StatusOK, copilotPaid)

		_, err := Copilot(t.Context(), client, "gh-token", domain)

		assert.Error(t, err, domain)
		assert.Empty(t, f.reqs, domain)
	}
}
