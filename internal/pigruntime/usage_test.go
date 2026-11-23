package pigruntime

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

// assistantRecord is an assistant message of provider/model, optionally
// answered by responseModel, with the usage fields given.
func assistantRecord(provider, model, responseModel string, ts int64, usage string) string {
	if responseModel != "" {
		responseModel = fmt.Sprintf(`,"responseModel":%q`, responseModel)
	}
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop","provider":%q,"model":%q%s,"timestamp":%d,"usage":%s}`, provider, model, responseModel, ts, usage)
}

func usageJSON(input, output, cacheRead, cacheWrite int, cost float64, extra string) string {
	return fmt.Sprintf(`{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d%s,"totalTokens":%d,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":%v}}`,
		input, output, cacheRead, cacheWrite, extra, input+output+cacheRead+cacheWrite, cost)
}

func TestUsageByModelEqualsTheSessionStats(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	appendHistoryMessage(t, manager, `{"role":"user","content":"hi","timestamp":1}`)
	kept := appendHistoryMessage(t, manager, assistantRecord("p1", "m1", "", 2, usageJSON(100, 10, 0, 0, 0.5, "")))
	appendHistoryMessage(t, manager, assistantRecord("p1", "m1", "m1-2025", 3, usageJSON(200, 20, 30, 40, 0.25, "")))
	appendHistoryMessage(t, manager, assistantRecord("p2", "m2", "", 4, usageJSON(1, 2, 3, 4, 0.125, "")))
	_, err := manager.AppendCompaction("summary", kept, 1000, nil, false, &ai.Usage{Input: 7, Output: 1, Cost: ai.UsageCost{Total: 0.0625}})
	require.NoError(t, err)

	check := func(t *testing.T, sess *Session) {
		t.Helper()
		got := sess.UsageByModel()
		assert.ElementsMatch(t, []ModelUsage{
			{Model: "p1/m1", Tokens: 110, Cost: 0.5},
			{Model: "p1/m1-2025", Tokens: 290, Cost: 0.25},
			{Model: "p2/m2", Tokens: 10, Cost: 0.125},
			{Model: "", Tokens: 8, Cost: 0.0625},
		}, got.Models)
		assert.Equal(t, Tokens{Input: 308, Output: 33, CacheRead: 33, CacheWrite: 44}, got.Tokens)
		_, cost := sess.Usage()
		assert.InDelta(t, cost, got.Cost(), 1e-12)
		assert.Equal(t, 4, sess.LoggedMessages())
	}
	check(t, sess)

	require.NoError(t, sess.Close(t.Context()))
	check(t, resume(t, r, sess.ID()))
}

func TestUsageAddMergesRowsAndSumsTokens(t *testing.T) {
	a := Usage{Tokens: Tokens{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}, Models: []ModelUsage{{Model: "p/a", Tokens: 10, Cost: 1}, {Model: "", Tokens: 1, Cost: 0.5}}}
	b := Usage{Tokens: Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}, Models: []ModelUsage{{Model: "p/a", Tokens: 5, Cost: 2}, {Model: "p/b", Tokens: 7, Cost: 4}}}

	sum := a.Add(b)

	assert.Equal(t, Tokens{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44}, sum.Tokens)
	assert.ElementsMatch(t, []ModelUsage{{Model: "p/a", Tokens: 15, Cost: 3}, {Model: "", Tokens: 1, Cost: 0.5}, {Model: "p/b", Tokens: 7, Cost: 4}}, sum.Models)
	assert.Equal(t, 7.5, sum.Cost())
	assert.Equal(t, 1.5, a.Cost(), "the receiver keeps its rows")
	assert.Equal(t, Usage{}, Usage{}.Add(Usage{}))
	assert.Empty(t, Usage{}.Add(Usage{Models: []ModelUsage{{Model: "p/failed"}}}).Models, "a failed request adds no row")
}

func TestCacheEqualsTheSessionAccounting(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	manager := sess.inner.SessionManager()
	assert.Equal(t, Cache{}, sess.Cache(), "nothing is reported before a request")

	appendHistoryMessage(t, manager, `{"role":"user","content":"hi","timestamp":1}`)
	appendHistoryMessage(t, manager, assistantRecord("p1", "m1", "", 2, usageJSON(100, 10, 0, 50000, 0.5, "")))
	appendHistoryMessage(t, manager, assistantRecord("p1", "m1", "", 3, usageJSON(50000, 10, 100, 0, 0.5, "")))

	accounting := manager.Accounting()
	require.NotNil(t, accounting.LatestCacheHitRate)
	require.Equal(t, 1, accounting.CacheWaste.MissCount, "the second request paid again for what the first made cacheable")
	got := sess.Cache()
	assert.True(t, got.Reported)
	assert.Equal(t, *accounting.LatestCacheHitRate, got.Hit)
	assert.Equal(t, accounting.CacheWaste.MissCount, got.Misses)
	assert.Equal(t, accounting.CacheWaste.MissedCost, got.MissedCost)
	assert.True(t, got.Expires.IsZero(), "the faux model declares no cache lifetime")
}

func TestCacheIsNotReportedWithoutCacheTokens(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	sess := openTestSession(t, r)
	appendHistoryMessage(t, sess.inner.SessionManager(), assistantRecord("p1", "m1", "", 2, usageJSON(100, 10, 0, 0, 0.5, "")))

	assert.False(t, sess.Cache().Reported)
}

func TestCacheExpiresAfterTheModelsLifetime(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "")
	sb := newSandbox(t)
	writeAgentFile(t, sb, "models.json", `{"providers":{"kopi-cache":{"baseUrl":"http://127.0.0.1:1","api":"openai-responses","apiKey":"synthetic-key","models":[
		{"id":"cached-1","promptCache":{"short":300,"long":3600}},
		{"id":"plain-1"}]}}}`)
	r, _ := newTestRuntime(t, sb)
	const started = int64(1_700_000_000_000)
	// open returns a session of model holding messages, resumed from its log.
	open := func(t *testing.T, model string, messages ...string) *Session {
		t.Helper()
		opts := OpenOptions{Model: model, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()}
		sess, err := r.Open(t.Context(), opts)
		require.NoError(t, err)
		if len(messages) == 0 {
			t.Cleanup(func() { assert.NoError(t, sess.Close(t.Context())) })
			return sess
		}
		for _, m := range messages {
			appendHistoryMessage(t, sess.inner.SessionManager(), m)
		}
		require.NoError(t, sess.Close(t.Context()))
		opts.ResumeID = sess.ID()
		resumed, err := r.Open(t.Context(), opts)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, resumed.Close(t.Context())) })
		return resumed
	}
	user := `{"role":"user","content":"hi","timestamp":1}`

	assert.True(t, open(t, "kopi-cache/cached-1").Cache().Expires.IsZero(), "no request yet")

	short := open(t, "kopi-cache/cached-1", user, assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 100, 0, 0, "")))
	assert.Equal(t, time.UnixMilli(started).Add(5*time.Minute), short.Cache().Expires)

	long := open(t, "kopi-cache/cached-1", user, assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 0, 100, 0, `,"cacheWrite1h":100`)))
	assert.Equal(t, time.UnixMilli(started).Add(time.Hour), long.Cache().Expires)

	last := open(t, "kopi-cache/cached-1", user,
		assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 0, 100, 0, `,"cacheWrite1h":100`)),
		assistantRecord("kopi-cache", "cached-1", "", started+60_000, usageJSON(10, 1, 100, 0, 0, "")))
	assert.Equal(t, time.UnixMilli(started+60_000).Add(time.Hour), last.Cache().Expires, "a read keeps the tier of the latest write")

	t.Run("compaction and resume", func(t *testing.T) {
		manager := last.inner.SessionManager()
		kept := appendHistoryMessage(t, manager, assistantRecord("kopi-cache", "cached-1", "", started+120_000, usageJSON(10, 1, 100, 0, 0, "")))
		_, err := manager.AppendCompaction("summary", kept, 1000, nil, false, nil)
		require.NoError(t, err)
		want := time.UnixMilli(started + 120_000).Add(time.Hour)
		assert.Equal(t, want, last.Cache().Expires)
		require.NoError(t, last.Close(t.Context()))
		resumed, err := r.Open(t.Context(), OpenOptions{Model: "kopi-cache/cached-1", ResumeID: last.ID(), SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, resumed.Close(t.Context())) })
		for _, msg := range resumed.inner.Messages() {
			if m := msg.Assistant; m != nil && m.Usage != nil {
				assert.Zero(t, m.Usage.CacheWrite, "the write is no longer in active context")
			}
		}
		assert.Equal(t, want, resumed.Cache().Expires, "full branch history keeps the hourly tier")
	})

	rewritten := open(t, "kopi-cache/cached-1", user,
		assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 0, 100, 0, `,"cacheWrite1h":100`)),
		assistantRecord("kopi-cache", "cached-1", "", started+60_000, usageJSON(10, 1, 0, 100, 0, "")))
	assert.Equal(t, time.UnixMilli(started+60_000).Add(5*time.Minute), rewritten.Cache().Expires, "the latest write decides the tier")

	switched := open(t, "kopi-cache/cached-1", user, assistantRecord("kopi-cache", "plain-1", "", started, usageJSON(10, 1, 100, 0, 0, "")))
	require.Equal(t, "cached-1", switched.inner.Model().ID)
	assert.True(t, switched.Cache().Expires.IsZero(), "another model made the last request")

	switchedAway := open(t, "kopi-cache/plain-1", user, assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 100, 0, 0, "")))
	assert.True(t, switchedAway.Cache().Expires.IsZero(), "the session's model is not the one whose cache was left")

	failed := open(t, "kopi-cache/cached-1", user,
		assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 100, 0, 0, "")),
		assistantRecord("kopi-cache", "cached-1", "", started+600_000, usageJSON(0, 0, 0, 0, 0, "")))
	assert.Equal(t, time.UnixMilli(started).Add(5*time.Minute), failed.Cache().Expires, "a later message that sent no prompt is skipped")

	plain := open(t, "kopi-cache/plain-1", user, assistantRecord("kopi-cache", "plain-1", "", started, usageJSON(10, 1, 100, 0, 0, "")))
	assert.True(t, plain.Cache().Expires.IsZero(), "the model declares no cache lifetime")

	for _, tc := range []struct {
		name, process, provider string
		lifetime                time.Duration
	}{
		{"process long", "long", "", time.Hour},
		{"process short", "short", "", 5 * time.Minute},
		{"provider long", "short", "long", time.Hour},
		{"provider short", "long", "short", 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PI_CACHE_RETENTION", tc.process)
			_, err := r.svcs.Credentials().Modify(t.Context(), "kopi-cache", func(*ai.Credential) (*ai.Credential, error) {
				return &ai.Credential{Type: ai.CredentialAPIKey, Key: "synthetic-key", Env: map[string]string{"PI_CACHE_RETENTION": tc.provider}}, nil
			})
			require.NoError(t, err)
			sess := open(t, "kopi-cache/cached-1", user, assistantRecord("kopi-cache", "cached-1", "", started, usageJSON(10, 1, 100, 0, 0, "")))
			assert.Equal(t, time.UnixMilli(started).Add(tc.lifetime), sess.Cache().Expires, "Responses usage has no hourly cache-write split")
		})
	}
}
