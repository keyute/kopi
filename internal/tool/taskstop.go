package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/keyute/kopi/internal/policy"
)

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
