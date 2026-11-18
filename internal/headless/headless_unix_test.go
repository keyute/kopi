//go:build unix

package headless

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func TestBackgroundShellNeitherHoldsNorWakesTheRun(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	in := filepath.Join(t.TempDir(), "input")
	require.NoError(t, syscall.Mkfifo(in, 0o600))
	f, err := os.OpenFile(in, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	tasks := tool.NewTasks()
	spawn := spawnTool(tasks, tool.NewShell(sb.Workspace, nil), "read x <'"+in+"'")
	steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Spawn", ID: "c1"}), pigruntimetest.Text("final")}

	h := startRun(t, sb, app.Config{Tools: []tool.Tool{spawn}, Tasks: tasks}, policy.Plan, "spawn", steps)

	require.NoError(t, h.wait(t))
	assert.Equal(t, "final\n", h.out.String())
	require.Len(t, tasks.List(), 1)
	assert.Equal(t, tool.TaskRunning, tasks.List()[0].Status)
	assert.Equal(t, 2, h.faux.CallCount())
}
