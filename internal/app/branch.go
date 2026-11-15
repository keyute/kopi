package app

import (
	"os"
	"path/filepath"
	"strings"
)

// gitBranch returns the branch checked out in the repository containing dir,
// the short hash of a detached HEAD, or "" when dir is in no repository or
// its HEAD cannot be read. It reads .git directly rather than running git.
func gitBranch(dir string) string {
	gitDir, ok := findGitDir(dir)
	if !ok {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	if ref, ok := strings.CutPrefix(line, "ref: "); ok {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(line) < 7 {
		return ""
	}
	return line[:7]
}

// findGitDir walks up from dir to the first .git. A .git file, as in
// worktrees and submodules, names the git directory, possibly relative to
// the file's own directory.
func findGitDir(dir string) (string, bool) {
	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Stat(dotGit)
		if err == nil {
			if info.IsDir() {
				return dotGit, true
			}
			content, err := os.ReadFile(dotGit)
			if err != nil {
				return "", false
			}
			target, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir: ")
			if !ok {
				return "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return target, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
