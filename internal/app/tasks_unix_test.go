//go:build unix

package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// taskCommand holds a child shell on input. EOF on alive proves the child
// has exited, including when StopAll kills its process group.
func taskCommand(t *testing.T) (command string, input *os.File, started, dead <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	in, alive := filepath.Join(dir, "input"), filepath.Join(dir, "alive")
	require.NoError(t, syscall.Mkfifo(in, 0o600))
	require.NoError(t, syscall.Mkfifo(alive, 0o600))
	f, err := os.OpenFile(in, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	startedCh, deadCh := make(chan struct{}), make(chan struct{})
	go func() {
		r, err := os.Open(alive)
		if err != nil {
			t.Errorf("failed to open task liveness pipe: %v", err)
			return
		}
		defer r.Close()
		close(startedCh)
		_, _ = io.Copy(io.Discard, r)
		close(deadCh)
	}()
	return "(exec 3>'" + alive + "'; read x <'" + in + "'; echo old-task-output) & wait", f, startedCh, deadCh
}

func TestRealTasksDrainCompletionDuringSessionTeardown(t *testing.T) {
	for _, operation := range []string{"close", "new-command", "new-session"} {
		t.Run(operation, func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			tasks := tool.NewTasks()
			sh := tool.NewShell(sb.Workspace, nil)
			bash := tool.NewBash(sh, sh, tasks)
			rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{bash}), policy.Source{Name: "test", Allow: []string{"Bash"}})
			require.NoError(t, err)
			h := newHarness(t, sb, Config{Tasks: tasks, Tools: []tool.Tool{bash}, Rules: rules, Mode: policy.Execute})
			require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
			oldID := h.c.Status().SessionID
			changes := subscribeTo[event.SessionChanged](h.c)

			completing, input, completingStarted, completingDead := taskCommand(t)
			holding, _, holdingStarted, holdingDead := taskCommand(t)
			h.faux.SetResponses([]ai.FauxResponseStep{
				pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Bash", ID: "complete", Args: map[string]any{"command": completing, "run_in_background": true}}),
				pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Bash", ID: "hold", Args: map[string]any{"command": holding, "run_in_background": true}}),
				pigruntimetest.Text("started"),
			})
			h.turn(t, "start commands")
			wait(t, completingStarted)
			wait(t, holdingStarted)
			require.Len(t, tasks.List(), 2)

			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			// Pause an actual watch callback before it re-enters the controller.
			// The other command's death proves teardown has reached StopAll.
			tasks.OnChange(func(info tool.TaskInfo) {
				if info.Status == tool.TaskCompleted {
					close(entered)
					<-release
					h.c.taskChanged(info)
					close(returned)
					return
				}
				h.c.taskChanged(info)
			})
			_, err = input.WriteString("finish\n")
			require.NoError(t, err)
			wait(t, completingDead)
			wait(t, entered)

			result := make(chan error, 1)
			go func() {
				switch operation {
				case "close":
					result <- h.c.Close(context.Background())
				case "new-command":
					_, err := h.c.Submit(context.Background(), "/new")
					if err == nil {
						select {
						case <-changes:
						case <-time.After(waitLimit):
							t.Error("timed out waiting for replacement session")
						}
						assertSwitchDone(t, h)
					}
					result <- err
				case "new-session":
					result <- h.c.NewSession(context.Background())
				}
			}()
			wait(t, holdingDead)
			select {
			case err := <-result:
				require.NoError(t, err)
				t.Fatal("teardown returned before its task callback drained")
			default:
			}
			unblock.Do(func() { close(release) })
			select {
			case err := <-result:
				require.NoError(t, err)
			case <-time.After(waitLimit):
				t.Fatal("timed out draining task callback during teardown")
			}
			wait(t, returned)
			assert.Empty(t, tasks.List())
			assert.False(t, tasks.Foreground())
			assert.Empty(t, h.queuedNotices())
			if operation == "close" {
				assert.Empty(t, h.c.Status().SessionID)
				return
			}
			assert.NotEqual(t, oldID, h.c.Status().SessionID)
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "fresh")})
			h.turn(t, "after replacement")
			req := receive(t, requests)
			assert.Equal(t, []string{"after replacement"}, req.UserTexts)
			assert.NotContains(t, req.Raw, "old-task-output")
			assert.Empty(t, ofType[event.TaskEnded](h.rec.all()))
		})
	}
}
