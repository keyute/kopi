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
	OffEffort // no reasoning
	LowEffort
	HighEffort
	XHighEffort
	MaxEffort
)

var effortNames = map[string]Effort{
	"off": OffEffort, "minimal": LowestEffort, "low": LowEffort, "medium": MediumEffort,
	"high": HighEffort, "xhigh": XHighEffort, "max": MaxEffort,
}

// ParseEffort returns the Effort named off, minimal, low, medium, high, xhigh or max.
func ParseEffort(name string) (Effort, error) {
	e, ok := effortNames[name]
	if !ok {
		return DefaultEffort, fmt.Errorf("effort %q: %w", name, ErrUnknownEffort)
	}
	return e, nil
}

// thinkingLevel is PiG's level for e, "" for DefaultEffort. PiG clamps a level
// the model does not support to the nearest one it does.
func (e Effort) thinkingLevel() ai.ModelThinkingLevel {
	switch e {
	case OffEffort:
		return ai.ThinkingOff
	case LowestEffort:
		return ai.ThinkingMinimal
	case LowEffort:
		return ai.ThinkingLow
	case MediumEffort:
		return ai.ThinkingMedium
	case HighEffort:
		return ai.ThinkingHigh
	case XHighEffort:
		return ai.ThinkingXHigh
	case MaxEffort:
		return ai.ThinkingMax
	}
	return ""
}

// Answer has model ("provider/id") reply once to prompt under system,
// outside any session. It also returns the request's cost in dollars,
// estimated from the model's price as Session.Usage estimates it.
func (r *Runtime) Answer(ctx context.Context, model string, effort Effort, system, prompt string) (string, float64, error) {
	m, err := r.usableModel(ctx, model)
	if err != nil {
		return "", 0, fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	opts := ai.StreamOptions{Thinking: effort.thinkingLevel().ReasoningOption()}
	text, cost, err := r.complete(ctx, m, system, prompt, func(content []ai.AssistantContentBlock) string {
		return ai.ContentText(content, "\n")
	}, opts)
	if err != nil {
		if ctx.Err() != nil {
			return "", cost, err
		}
		return "", cost, fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	return text, cost, nil
}

// complete asks m once and returns the text join makes of its content and the
// request's cost in dollars, also when it fails. Without text it fails with
// ErrNoAnswer.
func (r *Runtime) complete(ctx context.Context, m *ai.Model, system, prompt string, join func([]ai.AssistantContentBlock) string, opts ...ai.StreamOptions) (string, float64, error) {
	msg := r.svcs.ModelRuntime().CompleteSimple(ctx, m, ai.Context{
		SystemPrompt: system,
		Messages:     []ai.Message{ai.UserMessage{Content: ai.UserText(prompt), Timestamp: time.Now().UnixMilli()}},
	}, opts...)
	var cost float64
	if msg != nil {
		cost = msg.Usage.Cost.Total
	}
	// PiG reports a failed request only on the message, never as a Go error.
	if ctx.Err() != nil {
		return "", cost, ctx.Err()
	}
	if msg == nil {
		return "", cost, ErrNoAnswer
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		return "", cost, errors.New(pigTexts.Replace(msg.ErrorMessage))
	}
	text := strings.TrimSpace(join(msg.Content))
	if text == "" {
		return "", cost, ErrNoAnswer
	}
	return text, cost, nil
}
