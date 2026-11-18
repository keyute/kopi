package pigruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

// Answer has model ("provider/id") reply once to prompt under system,
// outside any session.
func (r *Runtime) Answer(ctx context.Context, model, system, prompt string) (string, error) {
	m, err := r.usableModel(ctx, model)
	if err != nil {
		return "", fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	text, err := r.complete(ctx, m, system, prompt, "\n")
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	return text, nil
}

// complete asks m once and returns its text blocks joined with sep. Without
// text it fails with ErrNoAnswer.
func (r *Runtime) complete(ctx context.Context, m *ai.Model, system, prompt, sep string, opts ...ai.StreamOptions) (string, error) {
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
	text := strings.TrimSpace(ai.ContentText(msg.Content, sep))
	if text == "" {
		return "", ErrNoAnswer
	}
	return text, nil
}
