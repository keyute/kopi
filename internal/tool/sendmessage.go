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
			resumed, err := agents.Message(ctx, args.To, args.Message)
			if err != nil {
				return Result{}, err
			}
			if resumed {
				return Result{Text: fmt.Sprintf("Agent %s continued.", args.To)}, nil
			}
			return Result{Text: fmt.Sprintf("Message queued for %s.", args.To)}, nil
		},
		Display: Display{
			Title: sendMessageTitle("SendMessage"),
			Done:  sendMessageTitle("Messaged"),
			Body:  func(string) string { return "" },
			Summary: func(output string) string {
				if strings.HasSuffix(output, " continued.") {
					return "continued"
				}
				return "queued"
			},
			Action: Action{Verb: "Sent", One: "message", Many: "messages"},
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
