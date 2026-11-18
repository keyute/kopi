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
			info, err := tasks.Stop(args.TaskID)
			if err != nil {
				return Result{}, err
			}
			if info.Status == TaskStopped {
				return Result{Text: fmt.Sprintf("Task %s stopped.", info.ID)}, nil
			}
			return Result{Text: fmt.Sprintf("Task %s already %s.", info.ID, info.StatusText())}, nil
		},
		Display: Display{
			Title:   taskTitle("TaskStop"),
			Summary: taskSummary,
			Action:  Action{Verb: "Stopped", One: "task", Many: "tasks"},
		},
	}
}
