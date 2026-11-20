package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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
	Timeout         *int `json:"timeout"`
	RunInBackground bool `json:"run_in_background"`
	Unsandboxed     bool `json:"unsandboxed"`
}

const (
	startedFormat = "Background task %s started."
	movedFormat   = "Background task %s: the user moved this command to the background, where it keeps running."
	timeoutFormat = "Background task %s: this command did not finish within its %v timeout and keeps running in the background."
)

var (
	startedResult = resultPattern(startedFormat)
	movedResult   = resultPattern(movedFormat)
	timeoutResult = resultPattern(timeoutFormat, `(?:\d+ms|(?:\d+m)?\d+(?:\.\d+)?s)`)
)

// resultPattern matches the text format produces, with the task id as the
// first group and any further verbs matching args.
func resultPattern(format string, args ...string) *regexp.Regexp {
	verbs := append([]any{`(t\d+)`}, make([]any, len(args))...)
	for i, a := range args {
		verbs[i+1] = a
	}
	return regexp.MustCompile("^" + fmt.Sprintf(regexp.QuoteMeta(format), verbs...) + "$")
}

// backgroundTask returns the task id output names when it is one of the
// background results, and whether the task was started rather than moved.
func backgroundTask(output string) (id string, started, ok bool) {
	if m := startedResult.FindStringSubmatch(output); m != nil {
		return m[1], true, true
	}
	for _, re := range []*regexp.Regexp{movedResult, timeoutResult} {
		if m := re.FindStringSubmatch(output); m != nil {
			return m[1], false, true
		}
	}
	return "", false, false
}

// NewBash runs execute-mode calls in sh and plan-mode calls in plan, which
// must keep commands from writing the workspace; with plan unsandboxed, plan
// mode cannot run commands. Commands run through tasks, which keeps those
// moved to the background; with nil tasks they run in the foreground only and
// one still running at its timeout is stopped. The unsandboxed parameter is
// offered only when sh is sandboxed and allowUnsandboxed is true.
func NewBash(sh, plan *Shell, tasks *Tasks, allowUnsandboxed bool) Tool {
	description := "Runs a shell command (bash, else sh) in the workspace and returns its combined stdout and stderr; " +
		"long output keeps its start and end. Every call starts a fresh shell in the workspace with no input: " +
		"cd and variables do not carry over. A non-zero exit status is an error that reports the exit code. "
	timeoutText := "Milliseconds before the command is stopped; default 120000, at most 600000."
	properties := map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "The command line to run.",
		},
	}
	if tasks == nil {
		description += "A command still running at its timeout is stopped; the error then holds the output so far."
	} else {
		description += "A command still running at its timeout, or one the user moves to the background, keeps running as a background task; " +
			"run_in_background starts it as one at once. The result then gives the task id: read its output with TaskOutput " +
			"and stop it with TaskStop; you are told when it ends."
		timeoutText = "Milliseconds before the command moves to the background; default 120000, at most 600000."
		properties["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Start the command as a background task and return its id at once; timeout does not apply.",
		}
	}
	properties["timeout"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     maxBashTimeoutMS,
		"description": timeoutText,
	}
	var open *Shell
	var unsandboxedHook func(json.RawMessage) bool
	if sh.Sandboxed() {
		description += " Commands run in an OS sandbox that allows writing only in temporary directories, paths the user allowed " +
			"and, in execute mode, the workspace; it may block reading some paths and the network. " +
			"A blocked operation fails, typically with \"Operation not permitted\" or \"Read-only file system\". " +
			"A directory it blocks reading may instead appear empty."
		if allowUnsandboxed {
			description += " unsandboxed runs one command outside it; that call asks for approval unless a rule allows it."
			properties["unsandboxed"] = map[string]any{
				"type":        "boolean",
				"description": "Run this command outside the OS sandbox; plan mode refuses such a call.",
			}
			open = NewShell(sh.dir, nil)
			unsandboxedHook = bashUnsandboxed
		}
	}
	planSandboxed := plan.Sandboxed()
	return Tool{
		Name:        "Bash",
		Label:       "Bash",
		Description: description,
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             []string{"command"},
			"additionalProperties": false,
		},
		Capability:    policy.Exec,
		Sandboxed:     sh.Sandboxed(),
		PlanSandboxed: planSandboxed,
		Unsandboxed:   unsandboxedHook,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeBashArgs(raw)
			if err != nil {
				return Result{}, err
			}
			if args.RunInBackground && tasks == nil {
				return Result{}, fmt.Errorf("%w: run_in_background is not available", ErrInvalidArgs)
			}
			if args.Unsandboxed && unsandboxedHook == nil {
				return Result{}, fmt.Errorf("%w: unsandboxed is not available", ErrInvalidArgs)
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			mode, err := admit(policy.Target{})
			if err != nil {
				return Result{}, err
			}
			// The shell is chosen from the admitted mode only: a later read of
			// the mode could run a plan-admitted call with a writable workspace.
			run := sh
			if args.Unsandboxed {
				if mode != policy.Execute {
					return Result{}, fmt.Errorf("failed to run command: an unsandboxed command runs only in execute mode: %w", policy.ErrDenied)
				}
				run = open
			} else if mode != policy.Execute {
				if !planSandboxed {
					return Result{}, fmt.Errorf("failed to run command: plan mode runs commands only in the OS sandbox, which is off or unavailable: %w", policy.ErrDenied)
				}
				run = plan
			}
			if args.RunInBackground {
				info, err := tasks.Start(ctx, run, args.Command)
				if err != nil {
					return Result{}, err
				}
				return Result{Text: fmt.Sprintf(startedFormat, info.ID)}, nil
			}
			timeout := defaultBashTimeoutMS
			if args.Timeout != nil {
				timeout = *args.Timeout
			}
			d := time.Duration(timeout) * time.Millisecond
			if tasks == nil {
				out, err := runForeground(ctx, run, args.Command, d)
				return Result{Text: out}, err
			}
			out, moved, err := tasks.Run(ctx, run, args.Command, d)
			switch {
			case moved.ID == "":
				return Result{Text: out}, err
			case moved.TimedOut:
				return Result{Text: fmt.Sprintf(timeoutFormat, moved.ID, d)}, nil
			default:
				return Result{Text: fmt.Sprintf(movedFormat, moved.ID)}, nil
			}
		},
		Display: Display{
			Title: func(raw json.RawMessage) string { return bashTitle(raw, unsandboxedHook != nil) },
			Done: func(raw json.RawMessage) string {
				var args bashArgs
				_ = json.Unmarshal(raw, &args)
				done := "Ran " + titleLine(args.Command)
				if args.RunInBackground {
					done = "Started " + titleLine(args.Command) + " in background"
				}
				if unsandboxedHook != nil && args.Unsandboxed {
					done += " · unsandboxed"
				}
				return done
			},
			Summary: func(output string) string {
				id, started, ok := backgroundTask(output)
				switch {
				case !ok && output == noOutputNotice:
					return "no output"
				case !ok:
					return lineCount(countLines(output))
				case started:
					return "task " + id
				}
				return "moved to background · task " + id
			},
			Body: func(output string) string {
				if _, _, ok := backgroundTask(output); ok || output == noOutputNotice {
					return ""
				}
				return output
			},
			Failure: bashFailure,
			Preview: bashPreview,
			Action:  Action{Verb: "Ran", One: "command", Many: "commands"},
		},
	}
}

// bashFailure relies on process.result's error shape: the output, a newline
// and a last line naming the exit status, the timeout or the stop.
func bashFailure(output string) (summary, body string) {
	text, last := "", output
	if i := strings.LastIndex(output, "\n"); i >= 0 {
		text, last = output[:i], output[i+1:]
	}
	switch {
	case strings.HasPrefix(last, ErrExitStatus.Error()+": "):
		summary = strings.TrimPrefix(last, ErrExitStatus.Error()+": ")
	case last == ErrCommandTimeout.Error():
		summary = "timed out"
	case last == ErrStopped.Error():
		summary = "stopped"
	default:
		if line := strings.TrimRight(output, "\n"); line != "" && !strings.Contains(line, "\n") {
			return line, ""
		}
		return "failed", output
	}
	if text == "" || text == noOutputNotice {
		return summary, ""
	}
	return summary + " · " + lineCount(countLines(text)), text
}

// runForeground runs command without the registry: at the timeout or when ctx
// ends it stops the command.
func runForeground(ctx context.Context, sh *Shell, command string, timeout time.Duration) (string, error) {
	p, err := sh.start(ctx, command)
	if err != nil {
		return "", err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.stop(ErrCommandTimeout)
	case <-ctx.Done():
		p.stop(ctx.Err())
	}
	<-p.done
	return p.result()
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

// bashUnsandboxed decodes leniently for the same reason as bashCommand.
func bashUnsandboxed(raw json.RawMessage) bool {
	var args struct {
		Unsandboxed bool `json:"unsandboxed"`
	}
	_ = json.Unmarshal(raw, &args)
	return args.Unsandboxed
}

func bashTitle(raw json.RawMessage, unsandboxedOffered bool) string {
	name := "Bash"
	if unsandboxedOffered && bashUnsandboxed(raw) {
		name = "Bash (unsandboxed)"
	}
	command := bashCommand(raw)
	if command == "" {
		return name
	}
	return name + " " + titleLine(command)
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
	if cut := cutRunes(first, maxBashTitleRunes); cut != first {
		return cut, false
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
