package instruction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestFindWithoutGitSearchesOnlyStartDir(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "outer")
	dir := filepath.Join(root, "a", "b")
	inner := write(t, filepath.Join(dir, "AGENTS.md"), "inner")

	assert.Equal(t, []string{inner}, Find(dir, nil))
}

func TestFindWalksFromGitRootOutermostFirst(t *testing.T) {
	above := t.TempDir()
	write(t, filepath.Join(above, "AGENTS.md"), "above")
	repo := filepath.Join(above, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o700))
	top := write(t, filepath.Join(repo, "AGENTS.md"), "top")
	mid := write(t, filepath.Join(repo, "a", "CLAUDE.md"), "mid")
	dir := filepath.Join(repo, "a", "b", "c")
	leaf := write(t, filepath.Join(dir, "AGENTS.md"), "leaf")

	assert.Equal(t, []string{top, mid, leaf}, Find(dir, []string{"CLAUDE.md"}))
}

func TestFindTreatsGitFileAsRoot(t *testing.T) {
	above := t.TempDir()
	write(t, filepath.Join(above, "AGENTS.md"), "above")
	worktree := filepath.Join(above, "wt")
	write(t, filepath.Join(worktree, ".git"), "gitdir: elsewhere")
	top := write(t, filepath.Join(worktree, "AGENTS.md"), "top")
	dir := filepath.Join(worktree, "sub")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	assert.Equal(t, []string{top}, Find(dir, nil))
}

func TestFindPicksOneFilePerDirectory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     []string
		fallbacks []string
		want      string
	}{
		{"AGENTS.md beats fallback", []string{"AGENTS.md", "CLAUDE.md"}, []string{"CLAUDE.md"}, "AGENTS.md"},
		{"fallbacks in order", []string{"CLAUDE.md", "GEMINI.md"}, []string{"GEMINI.md", "CLAUDE.md"}, "GEMINI.md"},
		{"later fallback when earlier absent", []string{"CLAUDE.md"}, []string{"GEMINI.md", "CLAUDE.md"}, "CLAUDE.md"},
		{"no fallbacks", []string{"CLAUDE.md"}, []string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				write(t, filepath.Join(dir, f), f)
			}
			var want []string
			if tc.want != "" {
				want = []string{filepath.Join(dir, tc.want)}
			}
			assert.Equal(t, want, Find(dir, tc.fallbacks))
		})
	}
}

func TestFindSkipsNonRegularAndFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "AGENTS.md"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "DANGLING.md")))
	target := write(t, filepath.Join(t.TempDir(), "shared.md"), "shared")
	link := filepath.Join(dir, "CLAUDE.md")
	require.NoError(t, os.Symlink(target, link))

	assert.Equal(t, []string{link}, Find(dir, []string{"DANGLING.md", "CLAUDE.md"}))
}

func TestLoadGlobalFirstThenTrustedRoots(t *testing.T) {
	home := t.TempDir()
	global := write(t, filepath.Join(home, "AGENTS.md"), "global")
	project := t.TempDir()
	projectFile := write(t, filepath.Join(project, "AGENTS.md"), "project")
	untrusted := t.TempDir()
	write(t, filepath.Join(untrusted, "AGENTS.md"), "untrusted")

	groups, errs := Load(global, []Root{{Dir: untrusted}, {Dir: project, Trusted: true}}, nil)
	assert.Empty(t, errs)
	assert.Equal(t, []Group{
		{Dir: home, Global: true, Files: []File{{Path: global, Text: "global"}}},
		{Dir: project, Files: []File{{Path: projectFile, Text: "project"}}},
	}, groups)
}

func TestLoadReadsEachFileOnce(t *testing.T) {
	home := t.TempDir()
	global := write(t, filepath.Join(home, "AGENTS.md"), "global")

	groups, errs := Load(global, []Root{{Dir: home, Trusted: true}}, nil)
	assert.Empty(t, errs)
	assert.Equal(t, []Group{{Dir: home, Global: true, Files: []File{{Path: global, Text: "global"}}}}, groups)

	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o700))
	top := write(t, filepath.Join(repo, "AGENTS.md"), "top")
	a := filepath.Join(repo, "a")
	aFile := write(t, filepath.Join(a, "AGENTS.md"), "a")
	b := filepath.Join(repo, "b")
	bFile := write(t, filepath.Join(b, "AGENTS.md"), "b")

	groups, errs = Load(filepath.Join(t.TempDir(), "AGENTS.md"), []Root{{Dir: a, Trusted: true}, {Dir: b, Trusted: true}}, nil)
	assert.Empty(t, errs)
	assert.Equal(t, []Group{
		{Dir: a, Files: []File{{Path: top, Text: "top"}, {Path: aFile, Text: "a"}}},
		{Dir: b, Files: []File{{Path: bFile, Text: "b"}}},
	}, groups)
}

func TestLoadGlobalAndProjectShareDirectory(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o700))
	project := write(t, filepath.Join(repo, "AGENTS.md"), "project")
	home := filepath.Join(repo, "home")
	global := write(t, filepath.Join(home, "AGENTS.md"), "global")

	groups, errs := Load(global, []Root{{Dir: home, Trusted: true}}, nil)
	assert.Empty(t, errs)
	assert.Equal(t, []Group{
		{Dir: home, Global: true, Files: []File{{Path: global, Text: "global"}}},
		{Dir: home, Files: []File{{Path: project, Text: "project"}}},
	}, groups)
}

func TestLoadGlobalFileTakesNoFallback(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "CLAUDE.md"), "not read")

	groups, errs := Load(filepath.Join(home, "AGENTS.md"), nil, []string{"CLAUDE.md"})
	assert.Empty(t, errs)
	assert.Empty(t, groups)
}

func TestLoadTrimsTextAndDropsBlankFiles(t *testing.T) {
	home := t.TempDir()
	global := write(t, filepath.Join(home, "AGENTS.md"), "\n  global  \n\n")
	project := t.TempDir()
	write(t, filepath.Join(project, "AGENTS.md"), " \n\t\n")
	write(t, filepath.Join(project, "CLAUDE.md"), "blocked by the blank AGENTS.md")

	groups, errs := Load(global, []Root{{Dir: project, Trusted: true}}, []string{"CLAUDE.md"})
	assert.Empty(t, errs)
	assert.Equal(t, []Group{{Dir: home, Global: true, Files: []File{{Path: global, Text: "global"}}}}, groups)
}

func TestLoadReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	project := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(project, ".git"), 0o700))
	top := write(t, filepath.Join(project, "AGENTS.md"), "top")
	dir := filepath.Join(project, "sub")
	unreadable := write(t, filepath.Join(dir, "AGENTS.md"), "secret")
	require.NoError(t, os.Chmod(unreadable, 0))
	write(t, filepath.Join(dir, "CLAUDE.md"), "no fallback after a read error")

	groups, errs := Load(filepath.Join(t.TempDir(), "AGENTS.md"), []Root{{Dir: dir, Trusted: true}}, []string{"CLAUDE.md"})
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0], os.ErrPermission)
	assert.Contains(t, errs[0].Error(), "failed to read instruction file "+unreadable)
	assert.Equal(t, []Group{{Dir: dir, Files: []File{{Path: top, Text: "top"}}}}, groups)
}
