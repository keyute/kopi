package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

type sendMessageArgs struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

// NewSendMessage returns the tool that sends a message to a subagent.
func NewSendMessage(agents Agents) Tool {
	return Tool{
		Name:        "SendMessage",
		Label:       "SendMessage",
		Description: "Sends a message to a subagent by its task id. A running subagent reads it when its current step ends; one that has ended starts again with its earlier context and reports again when it ends.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to": map[string]any{
					"type":        "string",
					"description": "The subagent's task id, such as t3.",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "The message to send.",
				},
			},
			"required":             []string{"to", "message"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[sendMessageArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if strings.TrimSpace(args.To) == "" {
				return Result{}, fmt.Errorf("%w: to is required", ErrInvalidArgs)
			}
			if strings.TrimSpace(args.Message) == "" {
				return Result{}, fmt.Errorf("%w: message is required", ErrInvalidArgs)
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			to, resumed, err := agents.Message(ctx, args.To, args.Message)
			if err != nil {
				return Result{}, err
			}
			text := fmt.Sprintf("Message queued for %s.", args.To)
			if resumed {
				text = fmt.Sprintf("Agent %s continued.", args.To)
			}
			if to.ID != "" {
				text += "\n" + taskName(to)
			}
			return Result{Text: text}, nil
		},
		Display: Display{
			Title: sendMessageTitle("SendMessage"),
			Done:  sendMessageTitle("Messaged"),
			Summary: func(output string) string {
				first, _, _ := strings.Cut(output, "\n")
				if strings.HasSuffix(first, " continued.") {
					return "continued"
				}
				return "queued"
			},
			Change: func(raw json.RawMessage) (string, []PreviewLine) {
				var args sendMessageArgs
				_ = json.Unmarshal(raw, &args)
				return "", previewLines(Context, args.Message)
			},
			Action: Action{Verb: "Messaged", One: "agent", Many: "agents"},
			Outcome: func(_ json.RawMessage, output string) (string, Action) {
				return taskOutcome("Messaged", output)
			},
		},
	}
}

// sendMessageTitle runs before the arguments are validated, so it decodes leniently.
func sendMessageTitle(verb string) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args sendMessageArgs
		_ = json.Unmarshal(raw, &args)
		if args.To == "" {
			return verb
		}
		return verb + " " + args.To
	}
}
