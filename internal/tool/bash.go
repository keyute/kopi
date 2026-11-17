package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/keyute/kopi/internal/policy"
)

const (
	defaultBashTimeoutMS = 120000
	maxBashTimeoutMS     = 600000
	maxBashTitleRunes    = 80
)

type bashArgs struct {
	Command string `json:"command"`
	// Timeout is a pointer so a missing timeout is told apart from 0.
	Timeout *int `json:"timeout"`
}

// NewBash runs execute-mode calls in sh and plan-mode calls in plan, which
// must keep commands from writing the workspace; with plan unsandboxed, plan
// mode cannot run commands.
func NewBash(sh, plan *Shell) Tool {
	description := "Runs a shell command (bash, else sh) in the workspace and returns its combined stdout and stderr; " +
		"long output keeps its start and end. Every call starts a fresh shell in the workspace with no input: " +
		"cd and variables do not carry over. A non-zero exit status is an error that reports the exit code. " +
		"timeout is in milliseconds (default 120000, at most 600000); a command that runs out of time is killed."
	if sh.Sandboxed() {
		description += " Commands run in an OS sandbox that allows writing only in temporary directories, paths the user allowed " +
			"and, in execute mode, the workspace; it may block reading some paths and the network. " +
			"A blocked operation fails with \"Operation not permitted\"."
	}
	planSandboxed := plan.Sandboxed()
	return Tool{
		Name:        "Bash",
		Label:       "Bash",
		Description: description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command line to run.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     maxBashTimeoutMS,
					"description": "Milliseconds before the command is killed; default 120000, at most 600000.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
		Capability:    policy.Exec,
		Sandboxed:     sh.Sandboxed(),
		PlanSandboxed: planSandboxed,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (string, error) {
			args, err := decodeBashArgs(raw)
			if err != nil {
				return "", err
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			mode, err := admit(policy.Target{})
			if err != nil {
				return "", err
			}
			// The shell is chosen from the admitted mode only: a later read of
			// the mode could run a plan-admitted call with a writable workspace.
			run := sh
			if mode != policy.Execute {
				if !planSandboxed {
					return "", fmt.Errorf("failed to run command: plan mode runs commands only in the OS sandbox, which is off or unavailable: %w", policy.ErrDenied)
				}
				run = plan
			}
			timeout := defaultBashTimeoutMS
			if args.Timeout != nil {
				timeout = *args.Timeout
			}
			return run.Run(ctx, args.Command, time.Duration(timeout)*time.Millisecond)
		},
		Display: Display{
			Title: bashTitle,
			Summary: func(output string) string {
				if output == noOutputNotice {
					return "no output"
				}
				return lineCount(countLines(output))
			},
			Preview: bashPreview,
			Action:  Action{Verb: "Ran", One: "command", Many: "commands"},
		},
	}
}

func decodeBashArgs(raw json.RawMessage) (bashArgs, error) {
	args, err := decodeArgs[bashArgs](raw)
	if err != nil {
		return args, err
	}
	if args.Command == "" {
		return args, fmt.Errorf("%w: command is required", ErrInvalidArgs)
	}
	if args.Timeout != nil && (*args.Timeout < 1 || *args.Timeout > maxBashTimeoutMS) {
		return args, fmt.Errorf("%w: timeout must be between 1 and %d", ErrInvalidArgs, maxBashTimeoutMS)
	}
	return args, nil
}

// bashCommand runs before the arguments are validated, so it decodes leniently.
func bashCommand(raw json.RawMessage) string {
	var args struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &args)
	return args.Command
}

func bashTitle(raw json.RawMessage) string {
	command := bashCommand(raw)
	if command == "" {
		return "Bash"
	}
	return "Bash " + titleLine(command)
}

// titleLine is bashTitleLine's line, marked with " …" when cut.
func titleLine(s string) string {
	line, whole := bashTitleLine(s)
	if !whole {
		line += " …"
	}
	return line
}

// bashTitleLine returns the command's first line cut to maxBashTitleRunes
// runes, and whether that shows the whole command.
func bashTitleLine(command string) (line string, whole bool) {
	first, rest, _ := strings.Cut(command, "\n")
	n := 0
	for i := range first {
		if n == maxBashTitleRunes {
			return first[:i], false
		}
		n++
	}
	return first, rest == ""
}

func bashPreview(raw json.RawMessage) []PreviewLine {
	command := bashCommand(raw)
	if _, whole := bashTitleLine(command); whole {
		return nil
	}
	return previewLines(Context, command)
}
