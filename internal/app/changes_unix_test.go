//go:build unix

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs git in dir with a hermetic configuration and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cmd := exec.Command("git", append([]string{"-c", "user.name=kopi", "-c", "user.email=kopi@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func newCommittedRepo(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "f")
	gitIn(t, dir, "init", "-q")
	require.NoError(t, os.WriteFile(file, []byte("a\nb\nc\n"), 0o600))
	gitIn(t, dir, "add", "f")
	gitIn(t, dir, "commit", "-q", "-m", "init")
	return dir, file
}

func TestChangesCommandCountsAgainstHEAD(t *testing.T) {
	dir, file := newCommittedRepo(t)

	out, err := shellRunner{}.run(t.Context(), dir, changesCommand)
	require.NoError(t, err)
	assert.Empty(t, out, "a clean tree prints nothing")

	require.NoError(t, os.WriteFile(file, []byte("a\nB\nc\nd\n"), 0o600))
	out, err = shellRunner{}.run(t.Context(), dir, changesCommand)
	require.NoError(t, err)
	added, removed := parseShortstat(out)
	assert.Equal(t, 2, added)
	assert.Equal(t, 1, removed)
}

func TestChangesCommandIgnoresEmbeddedBareRepository(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	inner := filepath.Join(dir, "inner")
	wt := filepath.Join(dir, "wt")
	require.NoError(t, os.Mkdir(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".gitattributes"), []byte("* filter=x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "f"), []byte("a\n"), 0o600))

	gitIn(t, dir, "init", "-q", "--bare", "-b", "main", inner)
	inside := func(args ...string) {
		gitIn(t, dir, append([]string{"--git-dir", inner, "--work-tree", wt}, args...)...)
	}
	inside("add", ".")
	inside("commit", "-q", "-m", "init")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "f"), []byte("a\nb\n"), 0o600))
	gitIn(t, dir, "--git-dir", inner, "config", "core.bare", "false")
	gitIn(t, dir, "--git-dir", inner, "config", "core.worktree", "../wt")
	gitIn(t, dir, "--git-dir", inner, "config", "filter.x.clean", "touch '"+marker+"'; cat")

	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", "inner")
	gitIn(t, dir, "commit", "-q", "-m", "outer")

	_, _ = shellRunner{}.run(t.Context(), inner, changesCommand)
	assert.NoFileExists(t, marker, "the embedded repository's clean filter must not run")
}

func TestChangesCommandDoesNotWriteIndex(t *testing.T) {
	dir, file := newCommittedRepo(t)
	marker := filepath.Join(t.TempDir(), "marker")
	hook := filepath.Join(dir, ".git", "hooks", "post-index-change")
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755))
	require.NoError(t, os.WriteFile(file, []byte("a\nb\nc\n"), 0o600))
	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(file, later, later))

	out, err := shellRunner{}.run(t.Context(), dir, changesCommand)
	require.NoError(t, err)
	added, removed := parseShortstat(out)
	assert.Zero(t, added)
	assert.Zero(t, removed)
	assert.NoFileExists(t, marker, "the index must not be rewritten")
}

func TestChangesCommandWithFileNamedHEAD(t *testing.T) {
	dir, file := newCommittedRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), nil, 0o600))
	require.NoError(t, os.WriteFile(file, []byte("a\nB\nc\nd\n"), 0o600))

	out, err := shellRunner{}.run(t.Context(), dir, changesCommand)
	require.NoError(t, err)
	added, removed := parseShortstat(out)
	assert.Equal(t, 2, added)
	assert.Equal(t, 1, removed)
}
