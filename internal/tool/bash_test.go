package tool

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/sandbox"
)

func newBashTool(t *testing.T) (Tool, string) {
	t.Helper()
	dir := t.TempDir()
	tasks := NewTasks()
	t.Cleanup(tasks.StopAll)
	return NewBash(NewShell(dir, nil), NewShell(dir, nil), tasks, true, true), dir
}

func TestBashDescribesItself(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Bash", tool.Name)
	assert.Equal(t, policy.Exec, tool.Capability)
	assert.Nil(t, tool.Target)
	assert.False(t, tool.Sandboxed)
	assert.False(t, tool.PlanSandboxed)
	assert.NotContains(t, tool.Description, "sandbox")
	for _, s := range []string{"run_in_background", "the file its output is saved to", "TaskStop"} {
		assert.Contains(t, tool.Description, s)
	}
	assert.Equal(t, []string{"command"}, tool.Parameters["required"])
	assert.Equal(t, false, tool.Parameters["additionalProperties"])
	properties, ok := tool.Parameters["properties"].(map[string]any)
	require.True(t, ok)
	background, ok := properties["run_in_background"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "boolean", background["type"])
	assert.Equal(t, Action{Verb: "Ran", One: "command", Many: "commands"}, tool.Display.Action)
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), &sandbox.Sandbox{}), NewTasks(), true, true)
	assert.True(t, sandboxed.Sandboxed)
	assert.True(t, sandboxed.PlanSandboxed)
	assert.Contains(t, sandboxed.Description, "in execute mode, the workspace")
	assert.NotContains(t, sandboxed.Description, "HTTP_PROXY")
	assert.False(t, NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), nil), NewTasks(), true, true).PlanSandboxed)
}

func TestBashRunsCommand(t *testing.T) {
	tool, _ := newBashTool(t)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"command":"echo one; echo two","timeout":60000}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", out.Text)
	assert.Equal(t, "2 lines", tool.Display.Summary(out.Text))
}

func TestBashTimeoutIsMilliseconds(t *testing.T) {
	tool, _ := newBashTool(t)

	out, err := tool.Run(context.Background(), json.RawMessage(`{"command":"sleep 600","timeout":1}`), allowTarget)

	require.NoError(t, err)
	assert.Contains(t, out.Text, "within its 1ms timeout")
}

func TestBashRejectsInvalidArgs(t *testing.T) {
	tool, _ := newBashTool(t)

	for _, tc := range []struct {
		name, args string
	}{
		{"bad json", `{"command":`},
		{"missing command", `{}`},
		{"empty command", `{"command":""}`},
		{"zero timeout", `{"command":"true","timeout":0}`},
		{"timeout too long", `{"command":"true","timeout":600001}`},
		{"unknown field", `{"command":"true","description":"x"}`},
		{"non-boolean run_in_background", `{"command":"true","run_in_background":"yes"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), json.RawMessage(tc.args), allowTarget)
			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestBashDoesNotRunWithoutAdmission(t *testing.T) {
	for _, background := range []string{"false", "true"} {
		t.Run("background "+background, func(t *testing.T) {
			tool, dir := newBashTool(t)
			refused := errors.New("refused")

			_, err := tool.Run(context.Background(), json.RawMessage(`{"command":"touch marker","run_in_background":`+background+`}`),
				func(policy.Target) (policy.Mode, error) { return policy.Plan, refused })

			require.ErrorIs(t, err, refused)
			assert.NoFileExists(t, filepath.Join(dir, "marker"))
		})
	}
}

func TestBashDoesNotStartWhenAdmissionCancels(t *testing.T) {
	for _, background := range []string{"false", "true"} {
		t.Run("background "+background, func(t *testing.T) {
			sh := NewShell(t.TempDir(), nil)
			// A Start attempt must fail differently from cancellation.
			sh.shell = filepath.Join(sh.dir, "missing-shell")
			tasks := NewTasks()
			tool := NewBash(sh, NewShell(sh.dir, nil), tasks, true, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			admitted := false

			out, err := tool.Run(ctx, json.RawMessage(`{"command":"echo unexpected","run_in_background":`+background+`}`), func(policy.Target) (policy.Mode, error) {
				admitted = true
				cancel()
				return policy.Execute, nil
			})

			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, context.Canceled.Error(), err.Error())
			assert.Empty(t, out.Text)
			assert.True(t, admitted)
			assert.Empty(t, tasks.List())
		})
	}
}

func TestBashRunsInTheShellOfTheAdmittedMode(t *testing.T) {
	execDir, planDir := t.TempDir(), t.TempDir()
	tool := NewBash(NewShell(execDir, &sandbox.Sandbox{}), NewShell(planDir, &sandbox.Sandbox{}), NewTasks(), true, true)
	for _, tc := range []struct {
		file string
		mode policy.Mode
	}{
		{"plan", policy.Plan},
		{"execute", policy.Execute},
		{"unknown", policy.Mode(7)},
	} {
		_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch `+tc.file+`"}`), func(policy.Target) (policy.Mode, error) {
			return tc.mode, nil
		})
		require.NoError(t, err, tc.file)
	}
	assert.Equal(t, []string{"plan", "unknown"}, listDir(t, planDir))
	assert.Equal(t, []string{"execute"}, listDir(t, execDir))
}

func TestBashRefusesPlanModeWithoutPlanSandbox(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sandboxed bool
	}{
		{"unsandboxed exec shell", false},
		{"sandboxed exec shell", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execDir, planDir := t.TempDir(), t.TempDir()
			var sb *sandbox.Sandbox
			if tc.sandboxed {
				sb = &sandbox.Sandbox{}
			}
			tool := NewBash(NewShell(execDir, sb), NewShell(planDir, nil), NewTasks(), true, true)

			_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch marker"}`), func(policy.Target) (policy.Mode, error) {
				return policy.Plan, nil
			})

			require.ErrorIs(t, err, policy.ErrDenied)
			assert.False(t, tool.PlanSandboxed)
			assert.NoFileExists(t, filepath.Join(execDir, "marker"))
			assert.NoFileExists(t, filepath.Join(planDir, "marker"))
		})
	}
}

func TestBashDisplayTitle(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Bash go test ./...", tool.Display.Title(json.RawMessage(`{"command":"go test ./..."}`)))
	assert.Equal(t, "Bash ls", tool.Display.Title(json.RawMessage(`{"command":"ls\n"}`)))
	assert.Equal(t, "Bash cd a …", tool.Display.Title(json.RawMessage(`{"command":"cd a\nls"}`)))
	assert.Equal(t, "Bash", tool.Display.Title(json.RawMessage(`{"command":`)))

	exact, err := json.Marshal(map[string]string{"command": strings.Repeat("é", maxBashTitleRunes)})
	require.NoError(t, err)
	assert.Equal(t, "Bash "+strings.Repeat("é", maxBashTitleRunes), tool.Display.Title(exact))

	long, err := json.Marshal(map[string]string{"command": strings.Repeat("a", maxBashTitleRunes-1) + "日本語"})
	require.NoError(t, err)
	title := tool.Display.Title(long)
	assert.Equal(t, "Bash "+strings.Repeat("a", maxBashTitleRunes-1)+"日 …", title)
	assert.True(t, utf8.ValidString(title))
}

func TestBashDisplaySummary(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "no output", tool.Display.Summary(noOutputNotice))
	assert.Equal(t, "1 line", tool.Display.Summary("hi\n"))
	assert.Equal(t, "3 lines", tool.Display.Summary("a\nb\nc"))
	assert.Empty(t, tool.Display.Summary("Background task t1 started."))
	assert.Empty(t, tool.Display.Summary("Background task t1: the user moved this command to the background, where it keeps running."))
	assert.Equal(t, "after 2m", tool.Display.Summary("Background task t1: this command did not finish within its 2m0s timeout and keeps running in the background."))
	assert.Equal(t, "1 line", tool.Display.Summary("Background task t1x"))
}

func TestBashDisplayBackgroundNotifications(t *testing.T) {
	tool, _ := newBashTool(t)

	for _, tc := range []struct {
		name, output, summary string
	}{
		{"started", "Background task t1 started" + backgroundTail("/outputs/s1/a.txt", true), ""},
		{"started without a file", "Background task t1 started" + backgroundTail("", true), ""},
		{"started where no end wakes", "Background task t1 started" + backgroundTail("/outputs/s1/a.txt", false), ""},
		{"timeout moved where no end wakes", "Background task t2: this command did not finish within its 10s timeout and keeps running in the background" + backgroundTail("/outputs/s1/a.txt", false), "after 10s"},
		{"user moved", "Background task t12: the user moved this command to the background, where it keeps running" + backgroundTail("/outputs/s1/a.txt", true), ""},
		{"timeout moved", "Background task t123: this command did not finish within its 1s timeout and keeps running in the background" + backgroundTail("/outputs/s1/a.txt", true), "after 1s"},
		{"timeout moved without a file", "Background task t123: this command did not finish within its 1m30s timeout and keeps running in the background" + backgroundTail("", true), "after 1m 30s"},
		{"millisecond timeout", "Background task t1: this command did not finish within its 1ms timeout and keeps running in the background" + backgroundTail("", true), "after 1ms"},
		{"fractional timeout", "Background task t1: this command did not finish within its 1.001s timeout and keeps running in the background" + backgroundTail("", true), "after 1.001s"},
		{"started, saved before the tail", "Background task t1 started.", ""},
		{"user moved, saved before the tail", "Background task t12: the user moved this command to the background, where it keeps running.", ""},
		{"timeout moved, saved before the tail", "Background task t123: this command did not finish within its 2m0s timeout and keeps running in the background.", "after 2m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.summary, tool.Display.Summary(tc.output))
			assert.Empty(t, tool.Display.Body(tc.output))
			for _, suffix := range []string{" more output", "\nreal output", "\n"} {
				output := tc.output + suffix
				assert.Equal(t, lineCount(countLines(output)), tool.Display.Summary(output))
				assert.Equal(t, output, tool.Display.Body(output))
			}
		})
	}
}

func TestBackgroundTailPromisesAReportOnlyWhereTheEndWakes(t *testing.T) {
	assert.Equal(t, ", writing its output to /o/a.txt. You are told when it ends: do not poll the file.", backgroundTail("/o/a.txt", true))
	assert.Equal(t, ". You are told when it ends.", backgroundTail("", true))
	assert.Equal(t, ", writing its output to /o/a.txt.", backgroundTail("/o/a.txt", false))
	assert.Equal(t, ".", backgroundTail("", false))
}

func TestBashDisplayPreservesBackgroundLookalikes(t *testing.T) {
	tool, _ := newBashTool(t)

	for _, output := range []string{
		"Background task t1: ordinary stdout\nreal output\n",
		"Background task t1 started",
		"Background task t1: something started.",
		"Background task t1. ordinary stdout",
		"Background task t1x started.",
		"Background task t1: this command did not finish within its unknown timeout and keeps running in the background.",
	} {
		assert.Equal(t, lineCount(countLines(output)), tool.Display.Summary(output))
		assert.Equal(t, output, tool.Display.Body(output))
	}
}

func TestBashDisplayDone(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "Ran go test", tool.Display.Done(json.RawMessage(`{"command":"go test"}`)))
	assert.Equal(t, "Ran cd a …", tool.Display.Done(json.RawMessage(`{"command":"cd a\nls"}`)))
	assert.Equal(t, "Started sleep 5 in background", tool.Display.Done(json.RawMessage(`{"command":"sleep 5","run_in_background":true}`)))
	assert.Equal(t, "Ran git config -l", tool.Display.Done(json.RawMessage(`{"command":"git config -l","unsandboxed":true}`)))
}

func TestBashDisplayOutcome(t *testing.T) {
	tool, _ := newBashTool(t)
	started := Action{Verb: "Started", One: "shell", Many: "shells"}
	moved := "Background task t1: the user moved this command to the background, where it keeps running" + backgroundTail("/outputs/s1/a.txt", true)
	timeout := "Background task t1: this command did not finish within its 2m0s timeout and keeps running in the background" + backgroundTail("/outputs/s1/a.txt", true)

	done, action := tool.Display.Outcome(json.RawMessage(`{"command":"sleep 5","run_in_background":true}`), "Background task t1 started"+backgroundTail("/outputs/s1/a.txt", true))
	assert.Empty(t, done)
	assert.Equal(t, started, action)

	for _, out := range []string{moved, timeout} {
		done, action = tool.Display.Outcome(json.RawMessage(`{"command":"sleep 5\nls"}`), out)
		assert.Equal(t, "Moved sleep 5 … to background", done)
		assert.Equal(t, started, action)
	}

	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), nil), NewTasks(), true, true)
	done, _ = sandboxed.Display.Outcome(json.RawMessage(`{"command":"sleep 5","unsandboxed":true}`), moved)
	assert.Equal(t, "Moved sleep 5 to background · unsandboxed", done)
	done, _ = tool.Display.Outcome(json.RawMessage(`{"command":"sleep 5","unsandboxed":true}`), moved)
	assert.Equal(t, "Moved sleep 5 to background", done)

	done, action = tool.Display.Outcome(json.RawMessage(`{"command":"ls"}`), "a\n")
	assert.Empty(t, done)
	assert.Equal(t, Action{}, action)
}

func TestBashDisplayBody(t *testing.T) {
	tool, _ := newBashTool(t)

	assert.Equal(t, "a\nb\n", tool.Display.Body("a\nb\n"))
	assert.Empty(t, tool.Display.Body(noOutputNotice))
	assert.Empty(t, tool.Display.Body("Background task t1 started."))
	assert.Empty(t, tool.Display.Body("Background task t1: the user moved this command to the background, where it keeps running."))
}

func TestBashDisplayFailure(t *testing.T) {
	tool, _ := newBashTool(t)
	status := ErrExitStatus.Error() + ": "
	for _, tc := range []struct {
		name, output, summary, body string
	}{
		{"exit with output", "a\nb\n\n" + status + "exit code 2", "exit code 2 · 2 lines", "a\nb\n"},
		{"exit with one line", "a\n" + status + "exit code 1", "exit code 1 · 1 line", "a"},
		{"exit without output", noOutputNotice + "\n" + status + "exit code 1", "exit code 1", ""},
		{"signal", "x\n" + status + "signal: killed", "signal: killed · 1 line", "x"},
		{"timeout", "x\ny\n" + ErrCommandTimeout.Error(), "timed out · 2 lines", "x\ny"},
		{"timeout without output", noOutputNotice + "\n" + ErrCommandTimeout.Error(), "timed out", ""},
		{"stopped", "x\n" + ErrStopped.Error(), "stopped · 1 line", "x"},
		{"other one line", "failed to run command: boom\n", "failed to run command: boom", ""},
		{"other many lines", "a\nb", "failed", "a\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, body := tool.Display.Failure(tc.output)
			assert.Equal(t, tc.summary, summary)
			assert.Equal(t, tc.body, body)
		})
	}
}

func TestBashDisplayPreview(t *testing.T) {
	unsandboxed, _ := newBashTool(t)
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), nil), NewTasks(), true, true)
	oneLine := json.RawMessage(`{"command":"ls"}`)
	twoLines := json.RawMessage(`{"command":"cd a\nls\n"}`)
	long, err := json.Marshal(map[string]string{"command": strings.Repeat("echo\n", 15)})
	require.NoError(t, err)
	wideCommand := strings.Repeat("x", maxBashTitleRunes+1)
	wide, err := json.Marshal(map[string]string{"command": wideCommand})
	require.NoError(t, err)

	for _, bash := range []Tool{unsandboxed, sandboxed} {
		assert.Empty(t, bash.Display.Preview(oneLine))
		assert.Equal(t, []PreviewLine{{Kind: Context, Text: "cd a"}, {Kind: Context, Text: "ls"}},
			bash.Display.Preview(twoLines))
		longPreview := bash.Display.Preview(long)
		require.NotEmpty(t, longPreview)
		assert.Equal(t, PreviewLine{Kind: Context, Text: "echo"}, longPreview[0])
		assert.Equal(t, []PreviewLine{{Kind: Context, Text: wideCommand}}, bash.Display.Preview(wide))
	}
}

func TestForegroundBashDescribesItself(t *testing.T) {
	tool := NewBash(NewShell(t.TempDir(), nil), NewShell(t.TempDir(), nil), nil, true, true)

	assert.Contains(t, tool.Description, "stopped")
	assert.NotContains(t, tool.Description, "background")
	assert.NotContains(t, tool.Description, "TaskOutput")
	properties, ok := tool.Parameters["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, properties, "command")
	assert.Contains(t, properties, "timeout")
	assert.NotContains(t, properties, "run_in_background")
	_, err := json.Marshal(tool.Parameters)
	require.NoError(t, err)
}

func TestForegroundBashRunsCommand(t *testing.T) {
	tool := NewBash(NewShell(t.TempDir(), nil), NewShell(t.TempDir(), nil), nil, true, true)

	out, err := tool.Run(t.Context(), json.RawMessage(`{"command":"echo one; exit 0"}`), allowTarget)

	require.NoError(t, err)
	assert.Equal(t, "one\n", out.Text)
	_, err = tool.Run(t.Context(), json.RawMessage(`{"command":"exit 3"}`), allowTarget)
	require.ErrorIs(t, err, ErrExitStatus)
}

func TestForegroundBashRefusesBackground(t *testing.T) {
	tool := NewBash(NewShell(t.TempDir(), nil), NewShell(t.TempDir(), nil), nil, true, true)

	_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"true","run_in_background":true}`), allowTarget)

	require.ErrorIs(t, err, ErrInvalidArgs)
}

func TestBashOffersUnsandboxedOnlyWhenSandboxed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sb      *sandbox.Sandbox
		tasks   *Tasks
		allow   bool
		offered bool
	}{
		{"sandboxed", &sandbox.Sandbox{}, NewTasks(), true, true},
		{"sandboxed without tasks", &sandbox.Sandbox{}, nil, true, true},
		{"sandboxed but not allowed", &sandbox.Sandbox{}, NewTasks(), false, false},
		{"unsandboxed", nil, NewTasks(), true, false},
		{"unsandboxed without tasks", nil, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewBash(NewShell(t.TempDir(), tc.sb), NewShell(t.TempDir(), tc.sb), tc.tasks, tc.allow, true)

			properties, ok := tool.Parameters["properties"].(map[string]any)
			require.True(t, ok)
			if !tc.offered {
				assert.NotContains(t, properties, "unsandboxed")
				assert.NotContains(t, tool.Description, "unsandboxed")
				assert.Nil(t, tool.Unsandboxed)
				return
			}
			assert.Equal(t, map[string]any{"type": "boolean", "description": "Run this command outside the OS sandbox; plan mode refuses such a call."}, properties["unsandboxed"])
			assert.Contains(t, tool.Description, "unsandboxed runs one command outside it; that call asks for approval unless a rule allows it.")
			require.NotNil(t, tool.Unsandboxed)
			_, err := json.Marshal(tool.Parameters)
			require.NoError(t, err)
		})
	}
}

func TestBashUnsandboxed(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		want       bool
	}{
		{"true", `{"command":"ls","unsandboxed":true}`, true},
		{"false", `{"command":"ls","unsandboxed":false}`, false},
		{"missing", `{"command":"ls"}`, false},
		{"malformed", `{"command":"ls","unsandboxed":`, false},
		{"non-boolean", `{"command":"ls","unsandboxed":"yes"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bashUnsandboxed(json.RawMessage(tc.args)))
		})
	}
}

func TestSpecsCarryTheUnsandboxedHook(t *testing.T) {
	sandboxed := NewBash(NewShell(t.TempDir(), &sandbox.Sandbox{}), NewShell(t.TempDir(), &sandbox.Sandbox{}), NewTasks(), true, true)
	plain, _ := newBashTool(t)

	specs := Specs([]Tool{sandboxed})
	require.NotNil(t, specs["Bash"].Unsandboxed)
	assert.True(t, specs["Bash"].Unsandboxed(json.RawMessage(`{"command":"ls","unsandboxed":true}`)))
	assert.False(t, specs["Bash"].Unsandboxed(json.RawMessage(`{"command":"ls"}`)))
	assert.Nil(t, Specs([]Tool{plain})["Bash"].Unsandboxed)
}

func TestBashUnsandboxedRunsInTheUnsandboxedShell(t *testing.T) {
	dir, planDir := t.TempDir(), t.TempDir()
	sh, plan := NewShell(dir, &sandbox.Sandbox{}), NewShell(planDir, &sandbox.Sandbox{})
	// A command that starts in the sandboxed or the plan shell must fail differently from one that runs.
	sh.shell = filepath.Join(dir, "missing-shell")
	plan.shell = filepath.Join(planDir, "missing-shell")
	tool := NewBash(sh, plan, NewTasks(), true, true)
	execute := func(policy.Target) (policy.Mode, error) { return policy.Execute, nil }

	out, err := tool.Run(t.Context(), json.RawMessage(`{"command":"echo outside","unsandboxed":true}`), execute)
	require.NoError(t, err)
	assert.Equal(t, "outside\n", out.Text)

	_, err = tool.Run(t.Context(), json.RawMessage(`{"command":"echo inside"}`), execute)
	require.Error(t, err)
	_, err = tool.Run(t.Context(), json.RawMessage(`{"command":"echo inside","unsandboxed":false}`), execute)
	require.Error(t, err)
}

func TestBashUnsandboxedIsDeniedOutsideExecute(t *testing.T) {
	for _, background := range []string{"false", "true"} {
		t.Run("background "+background, func(t *testing.T) {
			execDir, planDir := t.TempDir(), t.TempDir()
			tasks := NewTasks()
			tool := NewBash(NewShell(execDir, &sandbox.Sandbox{}), NewShell(planDir, &sandbox.Sandbox{}), tasks, true, true)

			_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"touch marker","unsandboxed":true,"run_in_background":`+background+`}`), func(policy.Target) (policy.Mode, error) {
				return policy.Plan, nil
			})

			require.ErrorIs(t, err, policy.ErrDenied)
			assert.Empty(t, tasks.List())
			assert.NoFileExists(t, filepath.Join(execDir, "marker"))
			assert.NoFileExists(t, filepath.Join(planDir, "marker"))
		})
	}
}

func TestBashUnsandboxedIsInvalidWhenNotOffered(t *testing.T) {
	execute := func(policy.Target) (policy.Mode, error) { return policy.Execute, nil }
	dir := t.TempDir()
	noSandbox, _ := newBashTool(t)
	notAllowed := NewBash(NewShell(dir, &sandbox.Sandbox{}), NewShell(dir, &sandbox.Sandbox{}), NewTasks(), false, true)

	for name, tool := range map[string]Tool{"no sandbox": noSandbox, "not allowed": notAllowed} {
		t.Run(name, func(t *testing.T) {
			_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"true","unsandboxed":true}`), execute)

			require.ErrorIs(t, err, ErrInvalidArgs)
		})
	}
}

func TestBashDisplayTitleMarksUnsandboxedOnlyWhenOffered(t *testing.T) {
	dir := t.TempDir()
	offered := NewBash(NewShell(dir, &sandbox.Sandbox{}), NewShell(dir, &sandbox.Sandbox{}), NewTasks(), true, true)
	notOffered := NewBash(NewShell(dir, &sandbox.Sandbox{}), NewShell(dir, &sandbox.Sandbox{}), NewTasks(), false, true)

	assert.Equal(t, "Bash (unsandboxed) git config -l", offered.Display.Title(json.RawMessage(`{"command":"git config -l","unsandboxed":true}`)))
	assert.Equal(t, "Bash (unsandboxed) cd a …", offered.Display.Title(json.RawMessage(`{"command":"cd a\nls","unsandboxed":true}`)))
	assert.Equal(t, "Bash (unsandboxed)", offered.Display.Title(json.RawMessage(`{"unsandboxed":true}`)))
	assert.Equal(t, "Bash git config -l", offered.Display.Title(json.RawMessage(`{"command":"git config -l","unsandboxed":false}`)))
	assert.Equal(t, "Bash git config -l", notOffered.Display.Title(json.RawMessage(`{"command":"git config -l","unsandboxed":true}`)))
	assert.Equal(t, "Ran git config -l · unsandboxed", offered.Display.Done(json.RawMessage(`{"command":"git config -l","unsandboxed":true}`)))
	assert.Equal(t, "Started make in background · unsandboxed", offered.Display.Done(json.RawMessage(`{"command":"make","run_in_background":true,"unsandboxed":true}`)))
	assert.Equal(t, "Ran git config -l", offered.Display.Done(json.RawMessage(`{"command":"git config -l"}`)))
	assert.Equal(t, "Ran git config -l", notOffered.Display.Done(json.RawMessage(`{"command":"git config -l","unsandboxed":true}`)))
}

func TestBashPlanRefusalsCarryTheTextTheAppMatches(t *testing.T) {
	plan := func(policy.Target) (policy.Mode, error) { return policy.Plan, nil }
	dir := t.TempDir()

	_, err := NewBash(NewShell(dir, &sandbox.Sandbox{}), NewShell(dir, &sandbox.Sandbox{}), NewTasks(), true, true).
		Run(t.Context(), json.RawMessage(`{"command":"true","unsandboxed":true}`), plan)
	require.ErrorIs(t, err, policy.ErrDenied)
	assert.Contains(t, err.Error(), "an unsandboxed command runs only in execute mode")

	_, err = NewBash(NewShell(dir, nil), NewShell(dir, nil), NewTasks(), true, true).
		Run(t.Context(), json.RawMessage(`{"command":"true"}`), plan)
	require.ErrorIs(t, err, policy.ErrDenied)
	assert.Contains(t, err.Error(), "which is off or unavailable")
}

func TestBashFailureOfRealCommands(t *testing.T) {
	tool, _ := newBashTool(t)
	execute := func(policy.Target) (policy.Mode, error) { return policy.Execute, nil }
	for _, tc := range []struct {
		command, summary, body string
	}{
		{"echo a; exit 2", "exit code 2 · 1 line", "a\n"},
		{"exit 1", "exit code 1", ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"`+tc.command+`"}`), execute)
			require.Error(t, err)

			summary, body := tool.Display.Failure(err.Error())
			assert.Equal(t, tc.summary, summary)
			assert.Equal(t, tc.body, body)
		})
	}
}

func TestBashUnsandboxedShellInheritsOutputs(t *testing.T) {
	outputs, dir := newSavedOutputs(t)
	sh := NewShell(t.TempDir(), &sandbox.Sandbox{}).WithOutputs(outputs)
	tool := NewBash(sh, NewShell(t.TempDir(), nil), NewTasks(), true, true)
	execute := func(policy.Target) (policy.Mode, error) { return policy.Execute, nil }

	out, err := tool.Run(t.Context(), json.RawMessage(`{"command":"seq 1 20000","unsandboxed":true}`), execute)

	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Contains(t, out.Text, "full output: "+files[0])
}

func TestBashDisplayCommand(t *testing.T) {
	tool, _ := newBashTool(t)
	command := func(c string) string {
		raw, err := json.Marshal(map[string]string{"command": c})
		require.NoError(t, err)
		return tool.Display.Command(raw)
	}
	wide := strings.Repeat("x", maxBashTitleRunes+1)

	assert.Empty(t, command("ls"))
	assert.Empty(t, command("ls\n\n"), "trailing newlines do not cut the title")
	assert.Equal(t, "cd a\nls", command("cd a\nls\n"))
	assert.Equal(t, wide, command(wide+"\n"))
	assert.Empty(t, command(strings.Repeat("x", maxBashTitleRunes)))
	assert.Empty(t, tool.Display.Command(json.RawMessage(`{"command":`)))
	assert.Equal(t, "Bash ls", tool.Display.Title(json.RawMessage(`{"command":"ls\n\n"}`)))
}
