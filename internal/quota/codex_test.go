package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	codexFiveHour = `{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":7980,"reset_at":1785269351}`
	codexWeek     = `{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":556112,"reset_at":1785825463}`
)

func TestCodex(t *testing.T) {
	want := []Window{
		{Label: "5h", UsedPercent: 12, ResetsAt: time.Unix(1785269351, 0)},
		{Label: "wk", UsedPercent: 40, ResetsAt: time.Unix(1785825463, 0)},
	}
	for name, body := range map[string]string{
		"primary is the short one": `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":` + codexFiveHour + `,"secondary_window":` + codexWeek + `}}`,
		"slots swapped":            `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":` + codexWeek + `,"secondary_window":` + codexFiveHour + `}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, f := fakeClient(http.StatusOK, body)

			got, err := Codex(t.Context(), client, "tok", "acct")

			require.NoError(t, err)
			assert.Equal(t, want, got)
			require.Len(t, f.reqs, 1)
			assert.Equal(t, "https://chatgpt.com/backend-api/wham/usage", f.reqs[0].URL.String())
			assert.Equal(t, "Bearer tok", f.reqs[0].Header.Get("Authorization"))
			assert.Equal(t, "acct", f.reqs[0].Header.Get("ChatGPT-Account-Id"))
		})
	}
}

func TestCodexWeeklyOnly(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"rate_limit":{"primary_window":`+codexWeek+`}}`)

	got, err := Codex(t.Context(), client, "tok", "acct")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "wk", got[0].Label)
}

func TestCodexNullRateLimit(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"plan_type":"business","rate_limit":null}`)

	got, err := Codex(t.Context(), client, "tok", "acct")

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCodexErrorHasNoToken(t *testing.T) {
	client, _ := fakeClient(http.StatusInternalServerError, ``)

	_, err := Codex(t.Context(), client, "secret-token", "acct")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-token")
}

func TestCodexSkipsAWindowWithoutALength(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":0,"reset_at":1785269351},"secondary_window":`+codexWeek+`}}`)

	got, err := Codex(t.Context(), client, "tok", "acct")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "wk", got[0].Label)
}

func TestCodexWithoutAnAccountSendsNoAccountHeader(t *testing.T) {
	client, f := fakeClient(http.StatusOK, `{"rate_limit":{"primary_window":`+codexWeek+`}}`)

	_, err := Codex(t.Context(), client, "tok", "")

	require.NoError(t, err)
	assert.NotContains(t, f.reqs[0].Header, "Chatgpt-Account-Id")
}
