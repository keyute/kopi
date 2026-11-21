package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestEditorCommandPrefersVisualOverEditor(t *testing.T) {
	cmd := editorCommand(env(map[string]string{"VISUAL": "code -w", "EDITOR": "vi"}), "/p.md")
	require.NotNil(t, cmd)
	assert.Equal(t, []string{"sh", "-c", `code -w "$1"`, "sh", "/p.md"}, cmd.Args)

	cmd = editorCommand(env(map[string]string{"VISUAL": "  ", "EDITOR": "vi"}), "/p.md")
	require.NotNil(t, cmd)
	assert.Equal(t, `vi "$1"`, cmd.Args[2], "a blank VISUAL is skipped")
}

func TestEditorCommandIsNilWithoutAnEditor(t *testing.T) {
	assert.Nil(t, editorCommand(env(nil), "/p.md"))
	assert.Nil(t, editorCommand(env(map[string]string{"VISUAL": " ", "EDITOR": ""}), "/p.md"))
}

func TestEditorCommandRunsAValueWithArgumentsOnAPathWithSpaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my plan.md")
	cmd := editorCommand(env(map[string]string{"EDITOR": "printf x >"}), path)
	require.NotNil(t, cmd)
	require.NoError(t, cmd.Run())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
}

func TestPlanOpenLaunchesTheEditorOnThePlanFileWithoutRefreshing(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	c.planFile = planPath
	c.submitErr = app.ErrOpenPlan
	m.getenv = env(map[string]string{"EDITOR": "vi"})
	calls := recordExec(m)

	submitText(m, "/plan open")
	require.Len(t, *calls, 1)
	assert.Equal(t, planPath, (*calls)[0].cmd.Args[4])
	assert.Empty(t, m.composer.value())

	m.Update((*calls)[0].done(nil))
	assert.Empty(t, c.refreshes)
}

func TestPlanOpenRefreshesAPlanRequestedWhileTheEditorRuns(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	c.planFile = planPath
	c.submitErr = app.ErrOpenPlan
	m.getenv = env(map[string]string{"EDITOR": "vi"})
	calls := recordExec(m)

	submitText(m, "/plan open")
	require.Len(t, *calls, 1)
	send(m, planRequest(7, "# Heading\n\nstep one"))
	require.NotNil(t, planOf(m))

	m.Update((*calls)[0].done(nil))
	assert.Equal(t, []uint64{7}, c.refreshes)
}

func TestPlanOpenWithoutAnEditorShowsTheNotice(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	c.planFile = planPath
	c.submitErr = app.ErrOpenPlan
	m.getenv = env(nil)
	calls := recordExec(m)

	submitText(m, "/plan open")
	assert.Empty(t, *calls)
	assert.Contains(t, body(m), "κ "+noEditorNotice)
}

func TestOpenEditorMarksEditingUntilTheEditorExits(t *testing.T) {
	m, _ := newTestModel(t, 70, 30)
	m.getenv = env(map[string]string{"EDITOR": "vi"})
	calls := recordExec(m)

	m.openEditor(planPath)
	assert.True(t, m.editing.Load())

	m.Update((*calls)[0].done(nil))
	assert.False(t, m.editing.Load())
}
