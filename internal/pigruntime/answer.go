package pigruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

// Effort is how much the model reasons before it answers.
type Effort int

const (
	DefaultEffort Effort = iota // the model's own default
	LowestEffort                // the lowest level the model supports
	MediumEffort
)

// Answer has model ("provider/id") reply once to prompt under system,
// outside any session.
func (r *Runtime) Answer(ctx context.Context, model string, effort Effort, system, prompt string) (string, error) {
	m, err := r.usableModel(ctx, model)
	if err != nil {
		return "", fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	var opts ai.StreamOptions
	switch effort {
	case LowestEffort:
		// PiG raises this to the nearest level the model supports.
		opts.Thinking = ai.ThinkingLevelMinimal
	case MediumEffort:
		opts.Thinking = ai.ThinkingLevelMedium
	}
	text, err := r.complete(ctx, m, system, prompt, func(content []ai.AssistantContentBlock) string {
		return ai.ContentText(content, "\n")
	}, opts)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	return text, nil
}

// complete asks m once and returns the text join makes of its content. Without
// text it fails with ErrNoAnswer.
func (r *Runtime) complete(ctx context.Context, m *ai.Model, system, prompt string, join func([]ai.AssistantContentBlock) string, opts ...ai.StreamOptions) (string, error) {
	msg := r.svcs.ModelRuntime().CompleteSimple(ctx, m, ai.Context{
		SystemPrompt: system,
		Messages:     []ai.Message{ai.UserMessage{Content: ai.UserText(prompt), Timestamp: time.Now().UnixMilli()}},
	}, opts...)
	// PiG reports a failed request only on the message, never as a Go error.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if msg == nil {
		return "", ErrNoAnswer
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		return "", errors.New(pigTexts.Replace(msg.ErrorMessage))
	}
	text := strings.TrimSpace(join(msg.Content))
	if text == "" {
		return "", ErrNoAnswer
	}
	return text, nil
}
