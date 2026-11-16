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

func NewBash(sh *Shell) Tool {
	return Tool{
		Name:  "Bash",
		Label: "Bash",
		Description: "Runs a shell command (bash, else sh) in the workspace and returns its combined stdout and stderr; " +
			"long output keeps its start and end. Every call starts a fresh shell in the workspace with no input: " +
			"cd and variables do not carry over. A non-zero exit status is an error that reports the exit code. " +
			"timeout is in milliseconds (default 120000, at most 600000); a command that runs out of time is killed. " +
			"Commands may run in an OS sandbox that allows writing only in the workspace, temporary directories and paths the user allowed, " +
			"and may block reading some paths and the network; a blocked operation fails with \"Operation not permitted\".",
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
		Capability: policy.Exec,
		Sandboxed:  sh.Sandboxed(),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) error) (string, error) {
			args, err := decodeBashArgs(raw)
			if err != nil {
				return "", err
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := admit(policy.Target{}); err != nil {
				return "", err
			}
			timeout := defaultBashTimeoutMS
			if args.Timeout != nil {
				timeout = *args.Timeout
			}
			return sh.Run(ctx, args.Command, time.Duration(timeout)*time.Millisecond)
		},
		Display: Display{
			Title: bashTitle,
			Summary: func(output string) string {
				if output == noOutputNotice {
					return "no output"
				}
				return lineCount(countLines(output))
			},
			Preview: bashPreview(sh.Sandboxed()),
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
	line, whole := bashTitleLine(command)
	if !whole {
		line += " …"
	}
	return "Bash " + line
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

func bashPreview(sandboxed bool) func(json.RawMessage) []PreviewLine {
	return func(raw json.RawMessage) []PreviewLine {
		// The notice comes first: the approval UI cuts long previews from the end.
		var out []PreviewLine
		if !sandboxed {
			out = append(out, PreviewLine{Kind: Context, Text: "runs without the OS sandbox"})
		}
		command := bashCommand(raw)
		if _, whole := bashTitleLine(command); !whole {
			out = append(out, previewLines(Context, command)...)
		}
		return out
	}
}
