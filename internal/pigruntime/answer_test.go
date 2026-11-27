package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func TestAnswerSendsSystemAndPromptAndReturnsTrimmedText(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "  the answer\n")})

	got, _, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "answer from the page", "what is it?")
	require.NoError(t, err)
	assert.Equal(t, "the answer", got)
	req := <-requests
	assert.Equal(t, "answer from the page", ai.GetCurrentSystemPrompt(req))
	var prompts []ai.UserContent
	for _, m := range req {
		if u, ok := m.(ai.UserMessage); ok {
			prompts = append(prompts, u.Content)
		}
	}
	assert.Equal(t, []ai.UserContent{ai.UserText("what is it?")}, prompts)
}

func TestAnswerReturnsTheRequestsCost(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, "An answer.", nil)
	})

	_, used, err := r.Answer(t.Context(), searchModel, DefaultEffort, "system", "prompt")

	require.NoError(t, err)
	assert.Equal(t, 4.0, used.Cost())
	assert.Equal(t, Usage{Tokens: Tokens{Input: 1, Output: 1}, Models: []ModelUsage{{Model: searchModel, Tokens: 2, Cost: 4}}}, used)
}

func TestAnswerNamesTheModelTheProviderRepliedAs(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-completions", "synthetic-key", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"model":"search-1-2026-11-01","choices":[{"delta":{"content":"An answer."},"finish_reason":null}]}

data: {"model":"search-1-2026-11-01","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}

data: [DONE]

`)
	})

	_, used, err := r.Answer(t.Context(), searchModel, DefaultEffort, "system", "prompt")

	require.NoError(t, err)
	require.Len(t, used.Models, 1)
	assert.Equal(t, "kopi-search/search-1-2026-11-01", used.Models[0].Model)
}

func TestAnswerReturnsTheCostOfAnEmptyAnswer(t *testing.T) {
	r, _ := newSearchRuntime(t, "openai-responses", "synthetic-key", func(w http.ResponseWriter) {
		responsesStream(w, " ", nil)
	})

	_, used, err := r.Answer(t.Context(), searchModel, DefaultEffort, "system", "prompt")

	require.ErrorIs(t, err, ErrNoAnswer)
	assert.Equal(t, 4.0, used.Cost())
}

func TestAnswerReportsProviderError(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}),
	})

	_, _, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "system", "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request: bad thing")
}

func TestAnswerEmptyReplyFails(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{reply(" \n")})

	_, _, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "system", "prompt")
	assert.ErrorIs(t, err, ErrNoAnswer)
}

func TestAnswerCancelledReturnsContextError(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	ctx, cancel := context.WithCancel(t.Context())
	faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			cancel()
			return ai.FauxAssistantMessage(ai.FauxContentString("too late"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})

	_, _, err := r.Answer(ctx, fauxModel, DefaultEffort, "system", "prompt")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestAnswerUnusableModelFails(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	models := r.svcs.ModelRuntime().GetAllModels("openai-codex")
	require.NotEmpty(t, models)
	codex, ok := models[0].(*ai.Model)
	require.True(t, ok)

	for _, tc := range []struct {
		name, model string
		sentinel    error
	}{
		{name: "unknown model", model: fauxProvider + "/missing", sentinel: ErrUnknownModel},
		{name: "missing credential", model: "openai-codex/" + codex.ID, sentinel: ErrNoCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.Answer(t.Context(), tc.model, DefaultEffort, "system", "prompt")
			require.ErrorIs(t, err, tc.sentinel)
			assert.Contains(t, err.Error(), tc.model)
		})
	}
}

func TestAnswerEffortSetsThinkingLevel(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))

	for _, tc := range []struct {
		name   string
		effort Effort
		want   ai.ThinkingLevel
	}{
		{"default", DefaultEffort, ""},
		{"lowest", LowestEffort, ai.ThinkingLevelMinimal},
		{"medium", MediumEffort, ai.ThinkingLevelMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := make(chan ai.ThinkingLevel, 1)
			thinker.SetResponses([]ai.FauxResponseStep{
				ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
					levels <- o.Thinking
					return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
				}),
			})

			_, _, err := r.Answer(t.Context(), "faux-thinker/thinker", tc.effort, "system", "prompt")
			require.NoError(t, err)
			assert.Equal(t, tc.want, <-levels)
		})
	}
}

const sideQuestion = "what is it?"

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func TestAnswerFromSendsTheMainRequestPlusTheQuestion(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	side := make(chan []ai.Message, 1)
	main := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), captureRequest(side, "the answer"), captureRequest(main, "second")})
	sess := openTestSession(t, r, probeTool(okProbe))
	_, err := sess.Send(t.Context(), Prompt{Text: "one"}, nil, nil)
	require.NoError(t, err)

	got, _, err := r.AnswerFrom(t.Context(), sess, "what is it?")
	require.NoError(t, err)
	assert.Equal(t, "the answer", got)
	_, err = sess.Send(t.Context(), Prompt{Text: "two"}, nil, nil)
	require.NoError(t, err)

	sideReq, mainReq := <-side, <-main
	require.Len(t, sideReq, len(mainReq))
	last := len(sideReq) - 1
	assert.Equal(t, marshal(t, mainReq[:last]), marshal(t, sideReq[:last]))
	assert.Equal(t, []string{"one", sideQuestion}, userTexts(sideReq))
	assert.Equal(t, kopiText, ai.GetCurrentSystemPrompt(sideReq))
	assert.Equal(t, []string{"Probe"}, toolSchemaNames(ai.GetCurrentTools(sideReq)))
}

func TestAnswerFromUsesCurrentModelAndEffort(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	thinker, model := registerThinker(t, r)
	sess := openTestSession(t, r)
	requests := make(chan struct {
		model    string
		thinking ai.ThinkingLevel
	}, 1)
	thinker.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, m *ai.Model) (ai.AssistantMessage, error) {
			requests <- struct {
				model    string
				thinking ai.ThinkingLevel
			}{m.ProviderMeta.ProviderID + "/" + m.ID, o.Thinking}
			return ai.FauxAssistantMessage(ai.FauxContentString("the answer"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})

	require.NoError(t, r.SetModel(t.Context(), sess, model))
	_, err := sess.SetEffort(HighEffort)
	require.NoError(t, err)
	_, _, err = r.AnswerFrom(t.Context(), sess, sideQuestion)
	require.NoError(t, err)

	got := <-requests
	assert.Equal(t, model, got.model)
	assert.Equal(t, ai.ThinkingLevelHigh, got.thinking)
}

func TestAnswerFromCarriesTheSessionIDWithoutContinuation(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	opts := make(chan ai.StreamOptions, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		opts <- o
		return ai.FauxAssistantMessage(ai.FauxContentString("the answer"), ai.FauxAssistantMessageOptions{}), nil
	})})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: "one"}, nil, nil)
	require.NoError(t, err)

	_, used, err := r.AnswerFrom(t.Context(), sess, "what is it?")
	require.NoError(t, err)

	got := <-opts
	assert.Equal(t, sess.ID(), got.SessionID)
	assert.True(t, got.DisableContinuation)
	assert.Equal(t, ai.TransportSSE, got.Transport)
	assert.Positive(t, used.Tokens.CacheRead)
}

func TestAnswerFromLeavesTheSessionAlone(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), reply("the answer")})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: "one"}, nil, nil)
	require.NoError(t, err)
	files := sessionFiles(t, filepath.Join(sb.agentDir, "sessions"))
	require.Len(t, files, 1)
	before, err := os.ReadFile(files[0])
	require.NoError(t, err)
	count := sess.MessageCount()

	_, _, err = r.AnswerFrom(t.Context(), sess, "what is it?")
	require.NoError(t, err)

	assert.Equal(t, count, sess.MessageCount())
	after, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestAnswerFromAnswersWhileAToolCallIsInFlight(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "the answer"), reply("done")})
	reached, release := make(chan struct{}), make(chan struct{})
	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		close(reached)
		<-release
		return toolOutput, nil
	}))
	sent := make(chan error, 1)
	go func() {
		res, err := sess.Send(t.Context(), Prompt{Text: "probe"}, nil, nil)
		if err == nil && res.Outcome.Status != event.Completed {
			err = errors.New("turn did not complete")
		}
		sent <- err
	}()
	<-reached

	got, _, err := r.AnswerFrom(t.Context(), sess, "what is it?")
	close(release)

	require.NoError(t, err)
	assert.Equal(t, "the answer", got)
	assert.Equal(t, []string{"probe", sideQuestion}, userTexts(<-requests))
	require.NoError(t, <-sent)
	assert.Equal(t, 4, sess.MessageCount())
}

func TestAnswerFromReplyOfOnlyAToolCallFailsAndRunsNothing(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{callProbe()})
	ran := false
	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		ran = true
		return toolOutput, nil
	}))

	_, _, err := r.AnswerFrom(t.Context(), sess, "what is it?")

	require.ErrorIs(t, err, ErrNoAnswer)
	assert.ErrorContains(t, err, "failed to answer the side question")
	assert.False(t, ran)
}

// The faux provider estimates a request's tokens from its transcript, so the
// input of a side request is more than that of the question alone.
func TestAnswerFromReturnsTheRequestsUsage(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), reply("the answer")})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: strings.Repeat("a", 4000)}, nil, nil)
	require.NoError(t, err)

	_, used, err := r.AnswerFrom(t.Context(), sess, "what is it?")

	require.NoError(t, err)
	assert.Greater(t, used.Tokens.Input+used.Tokens.CacheRead, 1000)
	assert.Positive(t, used.Tokens.Output)
	assert.Equal(t, []ModelUsage{{Model: fauxModel, Tokens: used.Tokens.Input + used.Tokens.CacheRead + used.Tokens.CacheWrite + used.Tokens.Output}}, used.Models)
}

func TestAnswerFromAfterACompactionCarriesTheSummary(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), reply("second"), reply("the summary"), captureRequest(requests, "the answer")})
	sess := openTestSession(t, r)
	for _, prompt := range []string{oldPrompt, keptPrompt} {
		_, err := sess.Send(t.Context(), Prompt{Text: prompt}, nil, nil)
		require.NoError(t, err)
	}

	_, _, err := r.AnswerFrom(t.Context(), sess, "what is it?")

	require.NoError(t, err)
	texts := userTexts(<-requests)
	require.Len(t, texts, 3)
	assert.Contains(t, texts[0], "the summary")
	assert.Equal(t, []string{keptPrompt, sideQuestion}, texts[1:])
}

func TestParseEffort(t *testing.T) {
	for name, want := range map[string]Effort{
		"off": OffEffort, "minimal": LowestEffort, "low": LowEffort, "medium": MediumEffort,
		"high": HighEffort, "xhigh": XHighEffort, "max": MaxEffort,
	} {
		got, err := ParseEffort(name)
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
	for _, name := range []string{"", "High", "ultra"} {
		_, err := ParseEffort(name)
		assert.ErrorIs(t, err, ErrUnknownEffort, name)
	}
}
