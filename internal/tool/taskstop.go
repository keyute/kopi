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
		Description: "Stops a running background task, killing its command and the processes it started.",
		Parameters:  taskIDParameters(),
		Capability:  policy.Read,
		Run: func(_ context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (string, error) {
			args, err := decodeTaskIDArgs(raw)
			if err != nil {
				return "", err
			}
			if _, err := admit(policy.Target{}); err != nil {
				return "", err
			}
			info, err := tasks.Stop(args.TaskID)
			if err != nil {
				return "", err
			}
			if info.Status == TaskStopped {
				return fmt.Sprintf("Task %s stopped.", info.ID), nil
			}
			return fmt.Sprintf("Task %s already %s.", info.ID, info.StatusText()), nil
		},
		Display: Display{
			Title:   taskTitle("TaskStop"),
			Summary: taskSummary,
			Action:  Action{Verb: "Stopped", One: "task", Many: "tasks"},
		},
	}
}
