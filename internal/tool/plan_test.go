package tool

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/policy"
)

type planFixture struct {
	kopiHome, plansDir string
	plans              *Plans
	ws, plain          *Workspace
}

func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	kopiHome := filepath.Join(t.TempDir(), "home")
	writeFile(t, filepath.Join(kopiHome, "config.json"), "{}")
	plansDir := filepath.Join(kopiHome, "plans")
	plans := NewPlans(plansDir)
	require.NoError(t, plans.Select("s1"))
	plain := newWorkspace(t, t.TempDir(), []string{kopiHome})
	return planFixture{kopiHome: kopiHome, plansDir: plansDir, plans: plans, ws: plain.WithPlans(plans), plain: plain}
}

// readTool runs the model-facing Read on path.
func readTool(t *testing.T, ws *Workspace, path string) (Result, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"file_path": path})
	require.NoError(t, err)
	return NewRead(ws).Run(t.Context(), raw, admitIn(policy.Plan))
}

// runTool runs t with args and returns the targets it admitted.
func runTool(t *testing.T, tl Tool, args map[string]any) ([]policy.Target, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	var admitted []policy.Target
	_, err = tl.Run(t.Context(), raw, func(target policy.Target) (policy.Mode, error) {
		admitted = append(admitted, target)
		return policy.Plan, nil
	})
	return admitted, err
}

func TestPlansSelectRefusesUnusableIDs(t *testing.T) {
	f := newPlanFixture(t)
	require.NotEmpty(t, f.plans.Path())
	for _, id := range []string{"", "a/b", ".hidden", "..", "s1" + string(filepath.Separator) + "x"} {
		assert.Error(t, f.plans.Select(id), id)
		assert.Empty(t, f.plans.Path(), id)
		require.NoError(t, f.plans.Select("s1"))
	}
}

func TestPlansPathAndNilReceiver(t *testing.T) {
	f := newPlanFixture(t)
	assert.Equal(t, filepath.Join(f.plansDir, "s1.md"), f.plans.Path())
	assert.DirExists(t, f.plansDir)
	var none *Plans
	assert.Empty(t, none.Path())
	_, ok, err := none.target("/x")
	assert.False(t, ok)
	assert.NoError(t, err)
}

func TestPlansReadMissingFile(t *testing.T) {
	f := newPlanFixture(t)
	_, err := f.plans.Read()
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestWorkspaceWithPlansWritesReadsAndEditsThePlan(t *testing.T) {
	f := newPlanFixture(t)
	path := f.plans.Path()
	for _, spelling := range []string{path, filepath.Join(f.plansDir, "..", "plans", "s1.md")} {
		_, err := runTool(t, NewWrite(f.ws), map[string]any{"file_path": spelling, "content": "# Plan\nstep one\n"})
		require.NoError(t, err, spelling)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "# Plan\nstep one\n", string(data))
	}

	res, err := readTool(t, f.ws, path)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "step one")

	_, err = runTool(t, NewEdit(f.ws), map[string]any{"file_path": path, "old_string": "one", "new_string": "two"})
	require.NoError(t, err)
	text, err := f.plans.Read()
	require.NoError(t, err)
	assert.Equal(t, "# Plan\nstep two\n", text)
}

func TestWorkspaceWithPlansTargetMatchesDestination(t *testing.T) {
	f := newPlanFixture(t)
	path := f.plans.Path()
	for _, spelling := range []string{path, filepath.Join(f.plansDir, "..", "plans", "s1.md")} {
		target, err := f.ws.Target(spelling)
		require.NoError(t, err)
		assert.True(t, target.PlanFile)
		assert.Equal(t, path, target.Path)
		dest, err := f.ws.prepare(spelling)
		require.NoError(t, err)
		dest.parent.Close()
		assert.Equal(t, target, dest.target)
	}
	plain, err := f.ws.Target(filepath.Join(f.kopiHome, "config.json"))
	require.NoError(t, err)
	assert.False(t, plain.PlanFile)
}

func TestWorkspaceWithPlansStillRefusesEverythingElse(t *testing.T) {
	f := newPlanFixture(t)
	writeFile(t, filepath.Join(f.plansDir, "other.md"), "other")
	require.NoError(t, f.plans.Select("s2"))
	old := filepath.Join(f.plansDir, "s1.md")
	writeFile(t, old, "old")
	cases := map[string]string{
		"config":      filepath.Join(f.kopiHome, "config.json"),
		"other plan":  filepath.Join(f.plansDir, "other.md"),
		"plans dir":   f.plansDir,
		"case change": filepath.Join(f.plansDir, "S2.md"),
		"old plan":    old,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			target, err := f.ws.Target(path)
			require.NoError(t, err)
			assert.False(t, target.PlanFile)
			_, err = runTool(t, NewWrite(f.ws), map[string]any{"file_path": path, "content": "x"})
			assert.ErrorIs(t, err, ErrOutsideWorkspace)
			_, err = runTool(t, NewEdit(f.ws), map[string]any{"file_path": path, "old_string": "a", "new_string": "b"})
			assert.ErrorIs(t, err, ErrOutsideWorkspace)
			_, err = f.ws.prepare(path)
			assert.ErrorIs(t, err, ErrOutsideWorkspace)
			res, err := readTool(t, f.ws, path)
			require.ErrorIs(t, err, ErrOutsideWorkspace)
			assert.NotContains(t, res.Text, "other")
			assert.NotContains(t, res.Text, "old")
		})
	}
	data, err := os.ReadFile(filepath.Join(f.plansDir, "other.md"))
	require.NoError(t, err)
	assert.Equal(t, "other", string(data))
}

func TestWorkspaceWithoutPlansRefusesThePlan(t *testing.T) {
	f := newPlanFixture(t)
	target, err := f.plain.Target(f.plans.Path())
	require.NoError(t, err)
	assert.False(t, target.PlanFile)
	_, err = runTool(t, NewWrite(f.plain), map[string]any{"file_path": f.plans.Path(), "content": "x"})
	assert.ErrorIs(t, err, ErrOutsideWorkspace)
	assert.NoFileExists(t, f.plans.Path())
}

func TestPlansRefuseSymlinkedDirectory(t *testing.T) {
	f := newPlanFixture(t)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "s1.md"), "outside")
	require.NoError(t, os.RemoveAll(f.plansDir))
	require.NoError(t, os.Symlink(elsewhere, f.plansDir))

	_, err := runTool(t, NewWrite(f.ws), map[string]any{"file_path": f.plans.Path(), "content": "x"})
	assert.ErrorIs(t, err, ErrProtected)
	_, err = f.plans.Read()
	assert.ErrorIs(t, err, ErrProtected)
	data, err := os.ReadFile(filepath.Join(elsewhere, "s1.md"))
	require.NoError(t, err)
	assert.Equal(t, "outside", string(data))
}

func TestPlansSymlinkedLeafIsRefusedOnReadAndReplacedOnWrite(t *testing.T) {
	f := newPlanFixture(t)
	victim := filepath.Join(t.TempDir(), "victim.md")
	writeFile(t, victim, "victim")
	require.NoError(t, os.Symlink(victim, f.plans.Path()))

	target, err := f.ws.Target(f.plans.Path())
	require.NoError(t, err)
	realDir, err := filepath.EvalSymlinks(f.plansDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(realDir, "s1.md"), target.Real)
	assert.True(t, target.PlanFile)

	// os.Root reports a symlink leaf that leaves its root with no exported sentinel.
	_, err = f.plans.Read()
	assert.ErrorContains(t, err, "path escapes from parent")
	res, err := readTool(t, f.ws, f.plans.Path())
	require.ErrorContains(t, err, "path escapes from parent")
	assert.NotContains(t, res.Text, "victim")
	_, err = runTool(t, NewEdit(f.ws), map[string]any{"file_path": f.plans.Path(), "old_string": "victim", "new_string": "x"})
	assert.ErrorContains(t, err, "path escapes from parent")

	_, err = runTool(t, NewWrite(f.ws), map[string]any{"file_path": f.plans.Path(), "content": "plan"})
	require.NoError(t, err)
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "victim", string(data))
	info, err := os.Lstat(f.plans.Path())
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	text, err := f.plans.Read()
	require.NoError(t, err)
	assert.Equal(t, "plan", text)
}

func TestPlansInsideAWorkspaceThatContainsKopiHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".kopi")
	config := filepath.Join(home, "config.json")
	writeFile(t, config, "{}")
	plans := NewPlans(filepath.Join(home, "plans"))
	require.NoError(t, plans.Select("s1"))
	plain := newWorkspace(t, root, []string{home})
	ws := plain.WithPlans(plans)
	plan := plans.Path()
	rel := filepath.Join(".kopi", "plans", "s1.md")
	run := func(w *Workspace, tl func(*Workspace) Tool, args map[string]any) error {
		_, err := runTool(t, tl(w), args)
		return err
	}

	for _, spelling := range []string{plan, rel} {
		require.NoError(t, run(ws, NewWrite, map[string]any{"file_path": spelling, "content": "a\n"}), spelling)
		require.NoError(t, run(ws, NewEdit, map[string]any{"file_path": spelling, "old_string": "a", "new_string": "b"}), spelling)
		text, err := plans.Read()
		require.NoError(t, err)
		assert.Equal(t, "b\n", text)
	}
	res, err := readTool(t, ws, rel)
	require.NoError(t, err)
	assert.Contains(t, res.Text, "b")

	require.NoError(t, os.Symlink(filepath.Join(home, "plans"), filepath.Join(root, "plink")))
	for name, path := range map[string]string{
		"config":              filepath.Join(".kopi", "config.json"),
		"other plan":          filepath.Join(".kopi", "plans", "other.md"),
		"new plan":            filepath.Join(".kopi", "plans", "new.md"),
		"through link":        filepath.Join("plink", "s1.md"),
		"dot dot":             rel + "/../other.md",
		"absolute other plan": filepath.Join(home, "plans", "other.md"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, run(ws, NewWrite, map[string]any{"file_path": path, "content": "x"}), ErrProtected)
			assert.ErrorIs(t, run(ws, NewEdit, map[string]any{"file_path": path, "old_string": "{", "new_string": "x"}), ErrProtected)
			_, err := ws.prepare(path)
			assert.ErrorIs(t, err, ErrProtected)
			res, err := readTool(t, ws, path)
			require.ErrorIs(t, err, ErrProtected)
			assert.NotContains(t, res.Text, "{}")
		})
	}
	assert.NoFileExists(t, filepath.Join(home, "plans", "other.md"))
	assert.NoFileExists(t, filepath.Join(home, "plans", "new.md"))
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	assert.Equal(t, "{}", string(data))

	assert.ErrorIs(t, run(plain, NewWrite, map[string]any{"file_path": plan, "content": "x"}), ErrProtected)
	assert.ErrorIs(t, run(plain, NewWrite, map[string]any{"file_path": rel, "content": "x"}), ErrProtected)
	text, err := plans.Read()
	require.NoError(t, err)
	assert.Equal(t, "b\n", text)

	require.NoError(t, os.Remove(plan))
	require.NoError(t, os.Symlink(filepath.Join("..", "config.json"), plan))
	_, err = plans.Read()
	assert.ErrorContains(t, err, "path escapes from parent")
	res, err = readTool(t, ws, plan)
	require.ErrorContains(t, err, "path escapes from parent")
	assert.NotContains(t, res.Text, "{}")
	assert.ErrorContains(t, run(ws, NewEdit, map[string]any{"file_path": plan, "old_string": "{", "new_string": "x"}), "path escapes from parent")
	require.NoError(t, run(ws, NewWrite, map[string]any{"file_path": plan, "content": "plan"}))
	data, err = os.ReadFile(config)
	require.NoError(t, err)
	assert.Equal(t, "{}", string(data))
	info, err := os.Lstat(plan)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}
