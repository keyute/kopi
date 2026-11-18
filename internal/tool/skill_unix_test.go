//go:build unix

package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/skill"
)

func TestSkillRefusesFIFOWithoutBlocking(t *testing.T) {
	sk := skillDir(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(sk.Dir, "pipe"), 0o600))

	_, err := runSkill(t, sk, `{"name":"deploy","file":"pipe"}`)

	assert.ErrorIs(t, err, ErrNotRegular)
}

func TestSkillRefusesFilesAfterDirectoryBecomesSymlink(t *testing.T) {
	sk := skillDir(t)
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "ref.md"), []byte("secret"), 0o644))
	tool := NewSkill([]skill.Skill{sk}, "")
	require.NoError(t, os.RemoveAll(sk.Dir))
	require.NoError(t, os.Symlink(other, sk.Dir))

	out, err := tool.Run(t.Context(), json.RawMessage(`{"name":"deploy","file":"ref.md"}`), admitAll)

	assert.ErrorIs(t, err, ErrSkillChanged)
	assert.NotContains(t, out, "secret")
}

func TestSkillRefusesSymlinkedDirectoryIntoHome(t *testing.T) {
	home := skillHome(t)
	dir := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(filepath.Join(home, "agent"), dir))

	out, err := runSkillAt(t, home, dir, "auth.json")

	assert.ErrorIs(t, err, ErrProtected)
	assert.NotContains(t, out, "secret")
}

func TestSkillRefusesWhenHomeIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	home := skillHome(t)
	sk := skillDir(t)
	require.NoError(t, os.Chmod(home, 0))
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	out, err := NewSkill([]skill.Skill{sk}, home).Run(t.Context(), json.RawMessage(`{"name":"deploy","file":"ref.md"}`), admitAll)

	require.Error(t, err)
	assert.NotContains(t, out, "one")
}

func TestSkillSymlinkToHomeFileIsRefused(t *testing.T) {
	home := skillHome(t)
	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(home, "agent", "auth.json"), filepath.Join(dir, "link")))

	out, err := runSkillAt(t, home, dir, "link")

	require.Error(t, err)
	assert.NotContains(t, out, "secret")
}

func TestSkillExemptsTheSkillsEntryNotWhatItPointsTo(t *testing.T) {
	home := skillHome(t)
	outside := filepath.Join(t.TempDir(), "dotfiles")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "s"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "s", "ref.md"), []byte("ref"), 0o644))
	require.NoError(t, os.RemoveAll(filepath.Join(home, "skills")))
	require.NoError(t, os.Symlink(outside, filepath.Join(home, "skills")))

	out, err := runSkillAt(t, home, filepath.Join(home, "skills", "s"), "ref.md")
	require.NoError(t, err)
	assert.Contains(t, out, "ref")

	require.NoError(t, os.RemoveAll(outside))
	require.NoError(t, os.Symlink(filepath.Join(home, "agent"), outside))
	for _, file := range []string{"agent/auth.json", "skills/auth.json"} {
		out, err := runSkillAt(t, home, home, file)

		assert.ErrorIs(t, err, ErrProtected, file)
		assert.NotContains(t, out, "secret")
	}
}

func TestSkillWalksTheUncleanedPath(t *testing.T) {
	sk := skillDir(t)
	require.NoError(t, os.Symlink(".", filepath.Join(sk.Dir, "scripts")))

	out, err := runSkill(t, sk, `{"name":"deploy","file":"scripts/../ref.md"}`)

	// os.Root refuses the path as an escape; the walk must agree rather than
	// judge a cleaned path.
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSkillChanged)
	assert.NotContains(t, out, "one")
}

func TestSkillReadsThroughALinkedDotDotAsTheRootDoes(t *testing.T) {
	sk := skillDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(sk.Dir, "sub", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sk.Dir, "sub", "ref.md"), []byte("inner"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("sub", "deep"), filepath.Join(sk.Dir, "scripts")))

	out, err := runSkill(t, sk, `{"name":"deploy","file":"scripts/../ref.md"}`)

	require.NoError(t, err)
	assert.Contains(t, out, "inner")
}
