package pigruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
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
// outside any session. It also returns what the request used, with its cost
// estimated from the model's price as Session.Usage estimates it.
func (r *Runtime) Answer(ctx context.Context, model string, effort Effort, system, prompt string) (string, Usage, error) {
	m, err := r.usableModel(ctx, model)
	if err != nil {
		return "", Usage{}, fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	opts := ai.StreamOptions{Thinking: effort.thinkingLevel().ReasoningOption()}
	text, used, err := r.complete(ctx, m, plainContext(system, prompt), func(content []ai.AssistantContentBlock) string {
		return ai.ContentText(content, "\n")
	}, opts)
	if err != nil {
		if ctx.Err() != nil {
			return "", used, err
		}
		return "", used, fmt.Errorf("failed to answer with model %q: %w", model, err)
	}
	return text, used, nil
}

// AnswerFrom has sess's own model, at its current effort, reply once to prompt
// after the session so far, and returns the reply's text and what the request
// used. The prompt and the reply are not added to the session or its file, a
// tool call in the reply is ignored and never runs, and the session's turn, if
// one is running, is not disturbed. A reply of no text fails with ErrNoAnswer.
func (r *Runtime) AnswerFrom(ctx context.Context, sess *Session, prompt string) (string, Usage, error) {
	// PiG writes the system message, which carries the prompt sections and the
	// tool declarations, into the session's messages before each of its loop's
	// requests, so they go out as of the last one: none before the first prompt,
	// and a change since is not in them yet. PiG's loop also runs per-request
	// steps it keeps unexported (re-projecting the session, hiding the
	// declarations a loadout hook hides, which Kopi installs none of, and the
	// per-request image limit); this request skips them.
	c := ai.Context{Messages: append(agent.ConvertToLLM(sess.inner.Messages()),
		ai.UserMessage{Content: ai.UserText(prompt), Timestamp: time.Now().UnixMilli()})}
	// The session's id routes the provider's prompt cache as the loop's own
	// requests do. DisableContinuation keeps this request from using or
	// replacing the session's server-side continuation state (Anthropic message
	// threads), as PiG's cache warming sets it for a replay. The request goes
	// over SSE because PiG's Codex websocket transport keeps per-session
	// connection and fallback state under that id, which the loop owns.
	// RepairToolFlow repairs tool results as the loop's requests do.
	opts := ai.StreamOptions{
		Thinking:            sess.inner.ThinkingLevel().ReasoningOption(),
		RepairToolFlow:      true,
		SessionID:           sess.inner.ID(),
		DisableContinuation: true,
		Transport:           ai.TransportSSE,
	}
	text, used, err := r.complete(ctx, sess.inner.Model(), c, func(content []ai.AssistantContentBlock) string {
		return ai.ContentText(content, "\n")
	}, opts)
	if err != nil {
		if ctx.Err() != nil {
			return "", used, err
		}
		return "", used, fmt.Errorf("failed to answer the side question: %w", err)
	}
	return text, used, nil
}

func plainContext(system, prompt string) ai.Context {
	return ai.Context{
		SystemPrompt: system,
		Messages:     []ai.Message{ai.UserMessage{Content: ai.UserText(prompt), Timestamp: time.Now().UnixMilli()}},
	}
}

// complete asks m once and returns the text join makes of its content and what
// the request used, also when it fails. Without text it fails with
// ErrNoAnswer.
func (r *Runtime) complete(ctx context.Context, m *ai.Model, c ai.Context, join func([]ai.AssistantContentBlock) string, opts ...ai.StreamOptions) (string, Usage, error) {
	msg := r.svcs.ModelRuntime().CompleteSimple(ctx, m, c, opts...)
	var used Usage
	if msg != nil {
		used = usageOf(msg, m)
	}
	// PiG reports a failed request only on the message, never as a Go error.
	if ctx.Err() != nil {
		return "", used, ctx.Err()
	}
	if msg == nil {
		return "", used, ErrNoAnswer
	}
	if msg.StopReason == ai.StopReasonError || msg.StopReason == ai.StopReasonAborted {
		return "", used, errors.New(pigTexts.Replace(msg.ErrorMessage))
	}
	text := strings.TrimSpace(join(msg.Content))
	if text == "" {
		return "", used, ErrNoAnswer
	}
	return text, used, nil
}
