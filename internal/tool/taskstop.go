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

// NewTaskStop returns the tool that stops a background task.
func NewTaskStop(tasks *Tasks) Tool {
	return Tool{
		Name:        "TaskStop",
		Label:       "TaskStop",
		Description: "Stops a running background task, a command or a subagent; a command is killed with the processes it started.",
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
			info, stopped, err := tasks.Stop(args.TaskID)
			if err != nil {
				return Result{}, err
			}
			status := "stopped"
			if !stopped {
				status = "already " + info.StatusText()
			}
			return Result{Text: fmt.Sprintf("Task %s %s.%s\n%s", info.ID, status, namedTaskResult, taskName(info))}, nil
		},
		Display: Display{
			Title:   taskTitle("TaskStop"),
			Done:    taskTitle("Stopped task"),
			Body:    func(string) string { return "" },
			Summary: taskStopSummary,
			Action:  Action{Verb: "Stopped", One: "task", Many: "tasks"},
			Outcome: func(raw json.RawMessage, output string) (string, Action) {
				if taskStopSummary(output) == "" {
					return taskOutcome("Stopped", output)
				}
				// Nothing was stopped, so the row keeps the call's title.
				subject, noun, ok := taskSubject(output)
				if !ok {
					return taskTitle("TaskStop")(raw), Action{Verb: "Checked", One: "task", Many: "tasks"}
				}
				return "TaskStop " + subject, Action{Verb: "Checked on", One: noun, Many: noun + "s"}
			},
			Subject: func(raw json.RawMessage, _ string) string {
				var args taskIDArgs
				_ = json.Unmarshal(raw, &args)
				return args.TaskID
			},
		},
	}
}

// taskStopSummary is "" for a task this call stopped, which the title says.
func taskStopSummary(output string) string {
	if summary := taskSummary(output); summary != "stopped" {
		return summary
	}
	return ""
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

// taskSummary relies on the result's first line reading "Task <id> <status>.",
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
// cut like a Bash title; a description keeps its first line.
func taskName(info TaskInfo) string {
	if info.Agent == "" {
		return nameCommand + ": " + titleLine(info.Command)
	}
	description, _, _ := strings.Cut(strings.TrimSpace(info.Command), "\n")
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
// output as "<verb> <task>": "Stopped explore › look". Without a name line
// it is zero, so the row keeps its static title and action.
func taskOutcome(verb, output string) (string, Action) {
	subject, noun, ok := taskSubject(output)
	if !ok {
		return "", Action{}
	}
	return verb + " " + subject, Action{Verb: verb, One: noun, Many: noun + "s"}
}
