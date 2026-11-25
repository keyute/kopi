package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
)

// Agents starts and messages subagents.
type Agents interface {
	// Launch starts agent on prompt in the background and returns its task id.
	Launch(ctx context.Context, agent, description, prompt string) (id string, err error)
	// Message delivers message to the subagent task id; to is that task, zero when it cannot be read,
	// and outcome says what became of the message.
	Message(ctx context.Context, id, message string) (to TaskInfo, outcome MessageOutcome, err error)
}

// MessageOutcome is what became of a message to a subagent.
type MessageOutcome int

const (
	// MessageQueued: the subagent was running and reads it at its next step.
	MessageQueued MessageOutcome = iota
	// MessageResumed: the subagent had ended and runs again with it.
	MessageResumed
	// MessageEnded: the subagent ended and its end notice has not been read yet;
	// the message was not delivered.
	MessageEnded
)

type agentArgs struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

// NewAgent returns the tool that starts a subagent defined in defs.
func NewAgent(defs []subagent.Definition, agents Agents) Tool {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return Tool{
		Name:        "Agent",
		Label:       "Agent",
		Description: agentDescription(defs),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short label for the task, shown to the user.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task. The subagent sees nothing else of this conversation.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"enum":        names,
					"description": "Name of the subagent to run.",
				},
			},
			"required":             []string{"description", "prompt", "subagent_type"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[agentArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if strings.TrimSpace(args.Description) == "" {
				return Result{}, fmt.Errorf("%w: description is required", ErrInvalidArgs)
			}
			if strings.TrimSpace(args.Prompt) == "" {
				return Result{}, fmt.Errorf("%w: prompt is required", ErrInvalidArgs)
			}
			if !slices.Contains(names, args.SubagentType) {
				return Result{}, fmt.Errorf("%w: %s", ErrUnknownAgent, args.SubagentType)
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			id, err := agents.Launch(ctx, args.SubagentType, args.Description, args.Prompt)
			if err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Agent %s started.", id) + launchTail}, nil
		},
		Display: Display{
			// It runs before the arguments are validated, so it decodes leniently.
			Title: func(raw json.RawMessage) string {
				var args agentArgs
				_ = json.Unmarshal(raw, &args)
				if args.SubagentType == "" || args.Description == "" {
					return "Agent"
				}
				return args.SubagentType + " › " + args.Description
			},
			Summary: launchSummary,
			Change: func(raw json.RawMessage) (string, []PreviewLine) {
				var args agentArgs
				_ = json.Unmarshal(raw, &args)
				return "", previewLines(Context, args.Prompt)
			},
			Action: Action{Verb: "Launched", One: "agent", Many: "agents"},
		},
	}
}

// launchTail ends the result of a launch with how the task's end arrives and
// that the model does not redo the task meanwhile.
const launchTail = " You are told when it ends, in a new turn if yours has ended: do not do its task yourself meanwhile."

// launchResult matches a launch's result with or without launchTail, which a
// session saved before it existed lacks.
var launchResult = regexp.MustCompile(`^(?:Agent|Workflow) t\d+ started\.`)

func launchSummary(output string) string {
	if launchResult.MatchString(output) {
		return "launched"
	}
	return ""
}

// agentDescription lists the subagents in the order given.
func agentDescription(defs []subagent.Definition) string {
	desc := "A subagent is another agent with its own context, instructions and tools; it sees only the prompt it is given. " +
		"A call returns at once with a task id while the subagent runs in the background; its end is reported to you with its final message. " +
		"TaskStop stops it, and SendMessage sends it a further message."
	if len(defs) == 0 {
		return desc
	}
	var b strings.Builder
	b.WriteString(desc + " Subagents:")
	writeDefinitions(&b, defs)
	return b.String()
}

// writeDefinitions writes one "name: description" line per definition, in the order given.
func writeDefinitions(b *strings.Builder, defs []subagent.Definition) {
	for _, d := range defs {
		b.WriteString("\n" + d.Name + ": " + d.Description)
	}
}
