package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
)

const (
	// WorkflowAgent is the task registry's agent name for a workflow run.
	WorkflowAgent = "workflow"
	// WorkflowCalls is how many agent() calls one run may make.
	WorkflowCalls = 1000
)

// Workflows starts workflow scripts.
type Workflows interface {
	// Start runs script in the background and returns its task id.
	Start(ctx context.Context, description, script string) (id string, err error)
}

type workflowArgs struct {
	Description string `json:"description"`
	Script      string `json:"script"`
}

// NewWorkflow returns the tool that starts a script whose agents may use the types in defs;
// maxWorkflows and maxAgents are the limits the description states.
func NewWorkflow(defs []subagent.Definition, w Workflows, maxWorkflows, maxAgents int) Tool {
	return Tool{
		Name:        "Workflow",
		Label:       "Workflow",
		Description: workflowDescription(defs, maxWorkflows, maxAgents),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short label for the workflow, shown to the user.",
				},
				"script": map[string]any{
					"type":        "string",
					"description": "The script: the body of an async function.",
				},
			},
			"required":             []string{"description", "script"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Deferred:   true,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[workflowArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if strings.TrimSpace(args.Description) == "" {
				return Result{}, fmt.Errorf("%w: description is required", ErrInvalidArgs)
			}
			if strings.TrimSpace(args.Script) == "" {
				return Result{}, fmt.Errorf("%w: script is required", ErrInvalidArgs)
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			id, err := w.Start(ctx, args.Description, args.Script)
			if err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Workflow %s started.", id)}, nil
		},
		Display: Display{
			// It runs before the arguments are validated, so it decodes leniently.
			Title: func(raw json.RawMessage) string {
				var args workflowArgs
				_ = json.Unmarshal(raw, &args)
				if args.Description == "" {
					return "Workflow"
				}
				return "Workflow › " + args.Description
			},
			Summary: func(output string) string {
				if strings.HasSuffix(output, " started.") {
					return "launched"
				}
				return ""
			},
			Body:   func(string) string { return "" },
			Action: Action{Verb: "Launched", One: "workflow", Many: "workflows"},
		},
	}
}

// NewStructuredOutput returns the tool whose arguments, matching schema, are a workflow agent's result;
// record receives the raw arguments of each call.
func NewStructuredOutput(schema map[string]any, record func(json.RawMessage)) Tool {
	return Tool{
		Name:        "StructuredOutput",
		Label:       "StructuredOutput",
		Description: "Returns your result: the arguments of your last call are returned as the result of your task, in place of your final message.",
		Parameters:  schema,
		Capability:  policy.Read,
		Run: func(_ context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			record(raw)
			return Result{Text: "Recorded."}, nil
		},
		Display: Display{
			Body:   func(string) string { return "" },
			Action: Action{Verb: "Recorded", One: "result", Many: "results"},
		},
	}
}

func workflowDescription(defs []subagent.Definition, maxWorkflows, maxAgents int) string {
	agentCall := "agent(prompt, {agentType, schema, label}): starts an agent with its own context, which sees only prompt, and returns a promise of its final message. " +
		"agentType names one of the agent types below; without it the agent has the tools a subagent can have."
	if len(defs) == 0 {
		agentCall = "agent(prompt, {schema, label}): starts an agent with its own context, which sees only prompt, and returns a promise of its final message. " +
			"The agent has the tools a subagent can have."
	}
	var b strings.Builder
	b.WriteString("Runs a JavaScript script that starts agents and combines their results. It is available because the user typed ultracode.\n" +
		"The script is the body of an async function. The call returns at once with a task id while the script runs in the background. " +
		"When it ends you are told, with the value it returned or the error it threw: within your turn, or in a new turn that its end starts if yours has ended. " +
		"TaskOutput returns a line for each agent's start and end so far, and TaskStop stops the script and its agents.\n" +
		"Agents share the working directory and the session's mode with you and with each other; nothing isolates those that write at the same time.\n" +
		"The script can call:\n" +
		"- " + agentCall + " With schema, a JSON Schema of type object, the promise is of an object matching it, and rejects if the agent gives none. label names the agent to the user.\n" +
		"- parallel(thunks): calls the functions together and returns a promise of their results in order, null for one that threw.\n" +
		"- pipeline(items, ...stages): passes every item through the stages in order, each called as stage(previous, item, index), the items together; returns a promise of the last stage's results in order, null for an item whose stage threw.\n" +
		fmt.Sprintf("At most %d of a script's agents run at once and the rest wait; an agent() call beyond the first %d fails the script. At most %d scripts run at once. A script has no files, network, timers or imports.", maxAgents, WorkflowCalls, maxWorkflows))
	if len(defs) > 0 {
		b.WriteString("\nAgent types:")
		writeDefinitions(&b, defs)
	}
	b.WriteString(workflowExamples)
	return b.String()
}

const workflowExamples = "\nExample, find then check:\n" +
	"const CLAIMS = {type: \"object\", properties: {claims: {type: \"array\", items: {type: \"string\"}}}, required: [\"claims\"]}\n" +
	"const checked = await pipeline([\"internal/policy\", \"internal/tool\"],\n" +
	"  area => agent(`List defects in ${area}, each with file:line.`, {schema: CLAIMS, label: area}),\n" +
	"  (found, area) => agent(`Check each claim against the source in ${area}. Return those that hold.\\n${JSON.stringify(found.claims)}`, {schema: CLAIMS, label: area + \" check\"}))\n" +
	"return checked.filter(Boolean).flatMap(c => c.claims)\n" +
	"Example, implement one slice at a time, then review it:\n" +
	"const REVIEW = {type: \"object\", properties: {approved: {type: \"boolean\"}, problems: {type: \"array\", items: {type: \"string\"}}}, required: [\"approved\", \"problems\"]}\n" +
	"const results = []\n" +
	"for (const slice of [\"the parser\", \"the printer\"]) {\n" +
	"  const report = await agent(`Implement ${slice} as PLAN.md describes and report what you changed.`, {label: slice})\n" +
	"  const review = await agent(`Review this change against PLAN.md.\\nReport: ${report}`, {schema: REVIEW, label: slice + \" review\"})\n" +
	"  results.push({slice, report, review})\n" +
	"}\n" +
	"return results"
