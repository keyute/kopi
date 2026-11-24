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

// A background result is one of these, then BackgroundTail.
const (
	startedFormat = "Background task %s started"
	movedFormat   = "Background task %s: the user moved this command to the background, where it keeps running"
	timeoutFormat = "Background task %s: this command did not finish within its %v timeout and keeps running in the background"

	backgroundFile = ", writing its output to %s"
	backgroundRead = "; use Read to read it"
	backgroundTold = ". You are told when it ends."
	backgroundPoll = ". You are told when it ends: do not poll the file."
)

var (
	startedResult = resultPattern(startedFormat)
	movedResult   = resultPattern(movedFormat)
	timeoutResult = resultPattern(timeoutFormat, `((?:\d+ms|(?:\d+m)?\d+(?:\.\d+)?s))`)
)

// resultPattern matches the text format produces, with the task id as the
// first group and any further verbs matching args, then one of BackgroundTail's
// tails; a result saved before those existed ends like the shortest of them.
func resultPattern(format string, args ...string) *regexp.Regexp {
	verbs := append([]any{`(t\d+)`}, make([]any, len(args))...)
	for i, a := range args {
		verbs[i+1] = a
	}
	tail := "(?:" + fmt.Sprintf(regexp.QuoteMeta(backgroundFile), `[^\n]+`) + ")?" +
		`(?:\.|` + regexp.QuoteMeta(backgroundTold) + "|" + regexp.QuoteMeta(backgroundPoll) + ")"
	return regexp.MustCompile("^" + fmt.Sprintf(regexp.QuoteMeta(format), verbs...) + tail + "$")
}

// BackgroundTail ends a background result with where the command's output is
// saved, when it is, and, when the command's end reaches the model whatever
// its turn does, that it need not read the file to wait.
func BackgroundTail(file string, wakes bool) string {
	var tail string
	if file != "" {
		tail = fmt.Sprintf(backgroundFile, file)
	}
	switch {
	case !wakes:
		if file != "" {
			tail += backgroundRead
		}
		return tail + "."
	case file == "":
		return tail + backgroundTold
	}
	return tail + backgroundPoll
}

// waitText is the time a call waited before its command moved, as a row shows
// it: "1s", "500ms", "2m", "1m 30s".
func waitText(d time.Duration) string {
	if d < time.Minute {
		return d.String()
	}
	m, s := d/time.Minute, d%time.Minute
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %s", m, s)
}

// backgroundTask reports whether output is one of the background results, and
// whether the task was started rather than moved.
func backgroundTask(output string) (started, ok bool) {
	if startedResult.MatchString(output) {
		return true, true
	}
	for _, re := range []*regexp.Regexp{movedResult, timeoutResult} {
		if re.MatchString(output) {
			return false, true
		}
	}
	return false, false
}

// NewBash runs execute-mode calls in sh and plan-mode calls in plan, which
// must keep commands from writing the workspace; with plan unsandboxed, plan
// mode cannot run commands. Commands run through tasks, which keeps those
// moved to the background; with nil tasks they run in the foreground only and
// one still running at its timeout is stopped. The unsandboxed parameter is
// offered only when sh is sandboxed and allowUnsandboxed is true. wakes says
// that a background command's end starts a turn when none runs; without it a
// background result promises no report of the end.
func NewBash(sh, plan *Shell, tasks *Tasks, allowUnsandboxed, wakes bool) Tool {
	description := "Runs a shell command (bash, else sh) in the workspace and returns its combined stdout and stderr. " +
		"Every call starts a fresh shell with no input: cd and variables do not carry over. "
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
			"the result then gives the task id and, when one is saved, the file its output is saved to, and TaskStop stops the task."
		timeoutText = "Milliseconds to wait for the command before it moves to the background; default 120000, at most 600000. " +
			"Set it only for a command that needs longer than the default."
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
			"A blocked operation fails, typically with \"Operation not permitted\" or \"Read-only file system\", " +
			"and a directory it blocks reading may appear empty."
		if sh.FiltersNetwork() {
			description += " The network is reachable only through the HTTP proxy named in HTTP_PROXY and HTTPS_PROXY, " +
				"and only for hosts the user listed; the command's output then ends with the hosts the proxy refused."
		}
		if allowUnsandboxed {
			properties["unsandboxed"] = map[string]any{
				"type":        "boolean",
				"description": "Run this command outside the OS sandbox, which asks for approval unless a rule allows it; plan mode refuses such a call.",
			}
			open = NewShell(sh.dir, nil).WithOutputs(sh.outputs)
			unsandboxedHook = bashUnsandboxed
		}
	}
	planSandboxed := plan.Sandboxed()
	unsandboxed := func(args bashArgs) string {
		if unsandboxedHook != nil && args.Unsandboxed {
			return " · unsandboxed"
		}
		return ""
	}
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
				return Result{Text: fmt.Sprintf(startedFormat, info.ID) + BackgroundTail(info.OutputFile, wakes)}, nil
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
				return Result{Text: fmt.Sprintf(timeoutFormat, moved.ID, d) + BackgroundTail(moved.OutputFile, wakes)}, nil
			default:
				return Result{Text: fmt.Sprintf(movedFormat, moved.ID) + BackgroundTail(moved.OutputFile, wakes)}, nil
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
				return done + unsandboxed(args)
			},
			Outcome: func(raw json.RawMessage, output string) (string, Action) {
				started, ok := backgroundTask(output)
				if !ok {
					return "", Action{}
				}
				var args bashArgs
				_ = json.Unmarshal(raw, &args)
				action := Action{Verb: "Started", One: "shell", Many: "shells"}
				if started {
					return "", action
				}
				return "Moved " + titleLine(args.Command) + " to background" + unsandboxed(args), action
			},
			Summary: func(output string) string {
				if m := timeoutResult.FindStringSubmatch(output); m != nil {
					if d, err := time.ParseDuration(m[2]); err == nil {
						return "after " + waitText(d)
					}
				}
				switch _, ok := backgroundTask(output); {
				case !ok && output == noOutputNotice:
					return "no output"
				case !ok:
					return lineCount(countLines(output))
				}
				return ""
			},
			Body: func(output string) string {
				if _, ok := backgroundTask(output); ok || output == noOutputNotice {
					return ""
				}
				return output
			},
			Failure: bashFailure,
			Command: bashFullCommand,
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
	first, rest, _ := strings.Cut(strings.TrimRight(command, "\n"), "\n")
	if cut := cutRunes(first, maxBashTitleRunes); cut != first {
		return cut, false
	}
	return first, rest == ""
}

// bashFullCommand is the command, without trailing newlines, when the title
// does not show all of it, else "".
func bashFullCommand(raw json.RawMessage) string {
	command := strings.TrimRight(bashCommand(raw), "\n")
	if _, whole := bashTitleLine(command); whole {
		return ""
	}
	return command
}

func bashPreview(raw json.RawMessage) []PreviewLine {
	command := bashFullCommand(raw)
	if command == "" {
		return nil
	}
	return previewLines(Context, command)
}
