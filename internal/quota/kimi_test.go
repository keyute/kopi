package quota

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimi(t *testing.T) {
	client, f := fakeClient(http.StatusOK, `{"usages":{"limit_5h":{"used_ratio":0.12,"reset_time":"2026-11-21T14:30:00Z"},"limit_7d":{"used_ratio":0.4,"reset_time":"2026-11-24T09:00:00Z"},"limit_month_total":{"used_ratio":0.41,"reset_time":"2026-12-01T00:00:00Z"}}}`)

	got, err := Kimi(t.Context(), client, "tok")

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "5h", got[0].Label)
	assert.InDelta(t, 12, got[0].UsedPercent, 1e-9)
	assert.Equal(t, time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC), got[0].ResetsAt)
	assert.Equal(t, "wk", got[1].Label)
	assert.InDelta(t, 40, got[1].UsedPercent, 1e-9)
	assert.Equal(t, "mo", got[2].Label)
	assert.InDelta(t, 41, got[2].UsedPercent, 1e-9)
	assert.Equal(t, "https://api.kimi.com/coding/v1/usages", f.reqs[0].URL.String())
	assert.Equal(t, "Bearer tok", f.reqs[0].Header.Get("Authorization"))
}

func TestKimiNullRatioAndZero(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"usages":{"limit_5h":{"used_ratio":null},"limit_7d":{"used_ratio":0},"limit_month_total":{"used_ratio":"0"}}}`)

	got, err := Kimi(t.Context(), client, "tok")

	require.NoError(t, err)
	require.Len(t, got, 2, "null ratios are dropped but zero ratios are retained")
	assert.Equal(t, "wk", got[0].Label)
	assert.Zero(t, got[0].UsedPercent)
	assert.Equal(t, "mo", got[1].Label)
	assert.Zero(t, got[1].UsedPercent)
}

func TestKimiRatioAsStringAndInvalid(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"usages":{"limit_5h":{"used_ratio":"0.25"},"limit_7d":{"used_ratio":"lots"},"limit_month_total":{"reset_time":"2026-12-01T00:00:00Z"},"limit_month_code":{"used_ratio":0.5,"reset_time":"soon"}}}`)

	got, err := Kimi(t.Context(), client, "tok")

	require.NoError(t, err)
	require.Len(t, got, 2, "entries without a valid ratio are dropped")
	assert.Equal(t, "5h", got[0].Label)
	assert.InDelta(t, 25, got[0].UsedPercent, 1e-9)
	assert.True(t, got[0].ResetsAt.IsZero())
	assert.Equal(t, "mo code", got[1].Label)
	assert.True(t, got[1].ResetsAt.IsZero(), "an unparsable reset is unknown")
}
