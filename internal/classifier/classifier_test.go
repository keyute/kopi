package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type stageCall struct{ system, prompt string }

// fakeStage answers with reply and err, recording what it was asked.
type fakeStage struct {
	mu    sync.Mutex
	calls []stageCall
	reply string
	err   error
}

func (f *fakeStage) answer(_ context.Context, system, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stageCall{system, prompt})
	return f.reply, f.err
}

func reply(decision, reason string) string {
	b, _ := json.Marshal(map[string]string{"decision": decision, "reason": reason})
	return string(b)
}

var bashCall = policy.Request{CallID: "c1", Tool: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}

func TestParse(t *testing.T) {
	long := strings.Repeat("é", 300)
	tests := []struct {
		name  string
		reply string
		want  policy.Review
	}{
		{"allow", `{"decision":"allow","reason":"ok"}`, policy.Review{Verdict: policy.VerdictAllow, Reason: "ok"}},
		{"deny", `{"decision":"deny","reason":"bad"}`, policy.Review{Verdict: policy.VerdictDeny, Reason: "bad"}},
		{"ask", `{"decision":"ask","reason":"unsure"}`, policy.Review{Verdict: policy.VerdictAsk, Reason: "unsure"}},
		{"fenced", "```\n{\"decision\":\"allow\",\"reason\":\"ok\"}\n```", policy.Review{Verdict: policy.VerdictAllow, Reason: "ok"}},
		{"fenced with language", "  ```json\n{\"decision\":\"deny\",\"reason\":\"bad\"}\n```\n", policy.Review{Verdict: policy.VerdictDeny, Reason: "bad"}},
		{"unknown decision", `{"decision":"maybe","reason":"x"}`, policy.Review{}},
		{"text before", "Sure: " + `{"decision":"allow","reason":"ok"}`, policy.Review{}},
		{"text after", `{"decision":"allow","reason":"ok"}` + " done", policy.Review{}},
		{"fence without JSON", "```\nallow\n```", policy.Review{}},
		{"capitalised decision", `{"decision":"Allow","reason":"ok"}`, policy.Review{}},
		{"not JSON", "sure, go ahead", policy.Review{}},
		{"multi-line reason", "{\"decision\":\"deny\",\"reason\":\"a\\n  b\\t\\nc\"}", policy.Review{Verdict: policy.VerdictDeny, Reason: "a b c"}},
		{"long reason", reply("deny", long), policy.Review{Verdict: policy.VerdictDeny, Reason: strings.Repeat("é", 200)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parse(tt.reply))
		})
	}
}

func TestReviewFilterAllowSkipsJudge(t *testing.T) {
	filter, judge := &fakeStage{reply: reply("allow", "fine")}, &fakeStage{}
	got, err := New(filter.answer, judge.answer, nil).Review(t.Context(), Conversation{}, bashCall)
	require.NoError(t, err)
	assert.Equal(t, policy.Review{Verdict: policy.VerdictAllow, Reason: "fine"}, got)
	assert.Empty(t, judge.calls)
}

func TestReviewJudgeReplacesFilter(t *testing.T) {
	tests := []struct {
		name          string
		filter, judge string
		want          policy.Review
	}{
		{"judge allows", reply("deny", "A"), reply("allow", ""), policy.Review{Verdict: policy.VerdictAllow}},
		{"judge denies", reply("ask", "unsure"), reply("deny", "B"), policy.Review{Verdict: policy.VerdictDeny, Reason: "B"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, judge := &fakeStage{reply: tt.filter}, &fakeStage{reply: tt.judge}
			got, err := New(filter.answer, judge.answer, nil).Review(t.Context(), Conversation{}, bashCall)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReviewFilterErrorStillRunsJudge(t *testing.T) {
	filter, judge := &fakeStage{err: errors.New("down")}, &fakeStage{reply: reply("deny", "no")}
	got, err := New(filter.answer, judge.answer, nil).Review(t.Context(), Conversation{}, bashCall)
	require.NoError(t, err)
	assert.Equal(t, policy.Review{Verdict: policy.VerdictDeny, Reason: "no"}, got)
}

func TestReviewJudgeErrorIsReturned(t *testing.T) {
	boom := errors.New("down")
	filter, judge := &fakeStage{reply: reply("ask", "")}, &fakeStage{err: boom}
	_, err := New(filter.answer, judge.answer, nil).Review(t.Context(), Conversation{}, bashCall)
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "failed to review the call")
}

func TestReviewStagesShareInput(t *testing.T) {
	filter, judge := &fakeStage{reply: reply("ask", "")}, &fakeStage{reply: reply("ask", "")}
	_, err := New(filter.answer, judge.answer, []string{"no pushes"}).Review(t.Context(), Conversation{Task: "t"}, bashCall)
	require.NoError(t, err)
	require.Len(t, filter.calls, 1)
	require.Len(t, judge.calls, 1)
	assert.Equal(t, filter.calls[0], judge.calls[0])
}

func TestReviewStageTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := func(ctx context.Context, _, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		start := time.Now()
		_, err := New(block, block, nil).Review(t.Context(), Conversation{}, bashCall)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 2*stageTimeout, time.Since(start))
	})
}

func TestReviewCancelledDuringFilterSkipsJudge(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	filter := func(ctx context.Context, _, _ string) (string, error) {
		cancel()
		return "", ctx.Err()
	}
	judge := &fakeStage{}
	_, err := New(filter, judge.answer, nil).Review(ctx, Conversation{}, bashCall)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, judge.calls)
}

func TestNewSystemPrompt(t *testing.T) {
	assert.Equal(t, systemPrompt, New(nil, nil, nil).system)
	assert.Equal(t, systemPrompt+"\n\nRules from the user:\n- a\n- b", New(nil, nil, []string{"a", "b"}).system)
}

func TestReviewMessage(t *testing.T) {
	type msg struct {
		Task    string `json:"task"`
		History []struct {
			Tool   string `json:"tool"`
			Args   string `json:"args"`
			Failed bool   `json:"failed"`
		} `json:"history"`
		Action struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		} `json:"action"`
	}
	run := func(t *testing.T, conv Conversation, r policy.Request) (string, msg) {
		t.Helper()
		filter := &fakeStage{reply: reply("allow", "")}
		_, err := New(filter.answer, nil, nil).Review(t.Context(), conv, r)
		require.NoError(t, err)
		var m msg
		require.NoError(t, json.Unmarshal([]byte(filter.calls[0].prompt), &m))
		return filter.calls[0].prompt, m
	}

	t.Run("empty history is an array", func(t *testing.T) {
		raw, m := run(t, Conversation{Task: "do it"}, bashCall)
		assert.Contains(t, raw, `"history":[]`)
		assert.Equal(t, "do it", m.Task)
		assert.Equal(t, "Bash", m.Action.Tool)
		assert.JSONEq(t, `{"command":"ls"}`, string(m.Action.Args))
	})
	t.Run("history entries", func(t *testing.T) {
		_, m := run(t, Conversation{History: []Call{{Tool: "Read", Args: `{"a":1}`, Failed: true}}}, bashCall)
		require.Len(t, m.History, 1)
		assert.Equal(t, "Read", m.History[0].Tool)
		assert.Equal(t, `{"a":1}`, m.History[0].Args)
		assert.True(t, m.History[0].Failed)
	})
	t.Run("invalid args become a string", func(t *testing.T) {
		_, m := run(t, Conversation{}, policy.Request{Tool: "Bash", Args: json.RawMessage(`not json`)})
		assert.JSONEq(t, `"not json"`, string(m.Action.Args))
	})
}
