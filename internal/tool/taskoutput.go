package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

type taskIDArgs struct {
	TaskID string `json:"task_id"`
}

// NewTaskOutput returns the tool that reports a background task's status and
// output.
func NewTaskOutput(tasks *Tasks) Tool {
	return Tool{
		Name:        "TaskOutput",
		Label:       "TaskOutput",
		Description: "Returns at once with the status and output so far of a background task, a command or a subagent; long output keeps its start and end.",
		Parameters:  taskIDParameters(),
		Capability:  policy.Read,
		Run: func(_ context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeTaskIDArgs(raw)
			if err != nil {
				return Result{}, err
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			info, err := tasks.Output(args.TaskID)
			if err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("Task %s %s.%s\n%s\n%s", info.ID, info.StatusText(), namedTaskResult, taskName(info), info.Output)}, nil
		},
		Display: Display{
			Title:   taskTitle("TaskOutput"),
			Done:    taskTitle("Checked task"),
			Summary: taskSummary,
			Body: func(output string) string {
				_, rest, _ := strings.Cut(output, "\n")
				if _, _, ok := readTaskName(output); ok {
					_, rest, _ = strings.Cut(rest, "\n")
				}
				return rest
			},
			Action: Action{Verb: "Checked", One: "task", Many: "tasks"},
			Outcome: func(_ json.RawMessage, output string) (string, Action) {
				return taskOutcome("Checked on", output)
			},
		},
	}
}

func taskIDParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task id, such as t1.",
			},
		},
		"required":             []string{"task_id"},
		"additionalProperties": false,
	}
}

func decodeTaskIDArgs(raw json.RawMessage) (taskIDArgs, error) {
	args, err := decodeArgs[taskIDArgs](raw)
	if err != nil {
		return args, err
	}
	if strings.TrimSpace(args.TaskID) == "" {
		return args, fmt.Errorf("%w: task_id is required", ErrInvalidArgs)
	}
	return args, nil
}

// taskTitle runs before the arguments are validated, so it decodes leniently.
func taskTitle(label string) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args taskIDArgs
		_ = json.Unmarshal(raw, &args)
		if args.TaskID == "" {
			return label
		}
		return label + " " + args.TaskID
	}
}

// taskSummary relies on both tools' first line reading "Task <id> <status>.",
// optionally followed by the named-result discriminator.
func taskSummary(output string) string {
	first, _, _ := strings.Cut(output, "\n")
	parts := strings.SplitN(strings.TrimSuffix(first, namedTaskResult), " ", 3)
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSuffix(parts[2], ".")
}

// namedTaskResult distinguishes metadata from legacy output beginning with a
// name-like line. Unmarked results must keep that line as output.
const namedTaskResult = " [named task]"

// Kinds of the name line a task result carries after its first line.
const (
	nameAgent    = "Agent"
	nameCommand  = "Command"
	nameWorkflow = "Workflow"
)

// taskName is the line that names a task by what it is: "Agent: explore › look",
// "Command: make test" or "Workflow: audit", each on one line. Only a command is
// cut like a Bash title; a description is kept whole, as its launch row shows it.
func taskName(info TaskInfo) string {
	if info.Agent == "" {
		return nameCommand + ": " + titleLine(info.Command)
	}
	description, _, _ := strings.Cut(strings.TrimRight(info.Command, "\n"), "\n")
	if info.Agent == WorkflowAgent {
		return nameWorkflow + ": " + description
	}
	return nameAgent + ": " + info.Agent + " › " + description
}

// readTaskName returns the kind and name of the name line on output's second
// line; ok is false for a result without one.
func readTaskName(output string) (kind, name string, ok bool) {
	first, rest, _ := strings.Cut(output, "\n")
	if !strings.HasSuffix(first, namedTaskResult) {
		return "", "", false
	}
	line, _, _ := strings.Cut(rest, "\n")
	for _, k := range []string{nameAgent, nameCommand, nameWorkflow} {
		if name, ok := strings.CutPrefix(line, k+": "); ok {
			return k, name, true
		}
	}
	return "", "", false
}

// taskSubject names the task in output for a title, and gives its noun:
// "explore › look" and "agent", "shell make test" and "shell".
func taskSubject(output string) (subject, noun string, ok bool) {
	kind, name, ok := readTaskName(output)
	switch {
	case !ok:
		return "", "", false
	case kind == nameAgent:
		return name, "agent", true
	case kind == nameCommand:
		return "shell " + name, "shell", true
	}
	return "workflow › " + name, "workflow", true
}

// taskOutcome is the done title and action of a call that names the task in
// output as "<verb> <task>": "Checked on explore › look". Without a name line
// it is zero, so the row keeps its static title and action.
func taskOutcome(verb, output string) (string, Action) {
	subject, noun, ok := taskSubject(output)
	if !ok {
		return "", Action{}
	}
	return verb + " " + subject, Action{Verb: verb, One: noun, Many: noun + "s"}
}
