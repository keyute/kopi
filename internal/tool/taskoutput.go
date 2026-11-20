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
			return Result{Text: fmt.Sprintf("Task %s %s.\n%s", info.ID, info.StatusText(), info.Output)}, nil
		},
		Display: Display{
			Title:   taskTitle("TaskOutput"),
			Done:    taskTitle("Checked task"),
			Summary: taskSummary,
			Action:  Action{Verb: "Checked", One: "task", Many: "tasks"},
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

// taskSummary relies on both tools' first line reading "Task <id> <status>.".
func taskSummary(output string) string {
	first, _, _ := strings.Cut(output, "\n")
	parts := strings.SplitN(first, " ", 3)
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSuffix(parts[2], ".")
}
