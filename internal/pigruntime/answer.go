package pigruntime

import (
	"context"
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
	msg := r.svcs.ModelRuntime().CompleteSimple(ctx, m, ai.Context{
		SystemPrompt: system,
		Messages:     []ai.Message{ai.UserMessage{Content: ai.UserText(prompt), Timestamp: time.Now().UnixMilli()}},
	})
	// PiG reports a failed request only on the message, never as a Go error.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if msg == nil {
		return "", fmt.Errorf("failed to answer with model %q: %w", model, ErrNoAnswer)
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		return "", fmt.Errorf("failed to answer with model %q: %s", model, pigTexts.Replace(msg.ErrorMessage))
	}
	text := strings.TrimSpace(ai.ContentText(msg.Content))
	if text == "" {
		return "", fmt.Errorf("failed to answer with model %q: %w", model, ErrNoAnswer)
	}
	return text, nil
}
