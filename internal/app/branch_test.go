package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitBranch(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name string
		// files are written under the fixture root; "{root}" in content is
		// replaced with the root's path.
		files map[string]string
		dir   string
		want  string
	}{
		{name: "branch ref", files: map[string]string{"repo/.git/HEAD": "ref: refs/heads/main\n"}, dir: "repo", want: "main"},
		{name: "nested branch name", files: map[string]string{"repo/.git/HEAD": "ref: refs/heads/feature/tui-status\n"}, dir: "repo", want: "feature/tui-status"},
		{name: "detached head", files: map[string]string{"repo/.git/HEAD": hash + "\n"}, dir: "repo", want: "0123456"},
		{
			name: "gitdir file with absolute path",
			files: map[string]string{
				"wt/.git":                          "gitdir: {root}/main/.git/worktrees/wt\n",
				"main/.git/worktrees/wt/HEAD":      "ref: refs/heads/side\n",
				"main/.git/HEAD":                   "ref: refs/heads/main\n",
				"main/.git/worktrees/wt/commondir": "../..\n",
			},
			dir:  "wt",
			want: "side",
		},
		{
			name: "gitdir file with relative path",
			files: map[string]string{
				"super/.git/HEAD":             "ref: refs/heads/main\n",
				"super/sub/.git":              "gitdir: ../.git/modules/sub\n",
				"super/.git/modules/sub/HEAD": hash + "\n",
			},
			dir:  "super/sub",
			want: "0123456",
		},
		{name: "workspace in a subdirectory", files: map[string]string{"repo/.git/HEAD": "ref: refs/heads/dev\n", "repo/a/b/keep": ""}, dir: "repo/a/b", want: "dev"},
		{name: "no repository", files: map[string]string{"plain/keep": ""}, dir: "plain", want: ""},
		{name: "gitdir file pointing nowhere", files: map[string]string{"repo/.git": "gitdir: missing\n"}, dir: "repo", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range tc.files {
				path := filepath.Join(root, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				content = strings.ReplaceAll(content, "{root}", root)
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			assert.Equal(t, tc.want, gitBranch(filepath.Join(root, tc.dir)))
		})
	}
}
