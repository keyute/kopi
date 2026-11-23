package pigruntime

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
