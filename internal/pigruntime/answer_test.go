package pigruntime

import (
	"context"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnswerSendsSystemAndPromptAndReturnsTrimmedText(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "  the answer\n")})

	got, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "answer from the page", "what is it?")
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

func TestAnswerReportsProviderError(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}),
	})

	_, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "system", "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid request: bad thing")
}

func TestAnswerEmptyReplyFails(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	faux.SetResponses([]ai.FauxResponseStep{reply(" \n")})

	_, err := r.Answer(t.Context(), fauxModel, DefaultEffort, "system", "prompt")
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

	_, err := r.Answer(ctx, fauxModel, DefaultEffort, "system", "prompt")
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
			_, err := r.Answer(t.Context(), tc.model, DefaultEffort, "system", "prompt")
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

			_, err := r.Answer(t.Context(), "faux-thinker/thinker", tc.effort, "system", "prompt")
			require.NoError(t, err)
			assert.Equal(t, tc.want, <-levels)
		})
	}
}
