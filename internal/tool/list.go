package tool

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// List returns up to limit workspace entries, shallowest first: slash-separated paths
// relative to the workspace, directories ending in "/". It skips .git, entries that
// .gitignore files ignore, and protected entries; symlinks are listed, not followed.
func (w *Workspace) List(limit int) ([]string, error) {
	_, protected, err := w.resolve(".")
	if err != nil {
		return nil, err
	}
	if protected {
		return nil, nil
	}
	stores, err := w.statProtected()
	if err != nil {
		return nil, err
	}
	type queued struct {
		dir string
		// vetted is the identity dir had when its parent was listed; nil for ".".
		vetted fs.FileInfo
	}
	var out []string
	// Breadth-first, so the limit keeps shallow entries and a directory's
	// patterns are loaded before its children's. Each pattern is scoped to its
	// directory, and later patterns take priority, as in git.
	queue := []queued{{dir: "."}}
	var patterns []gitignore.Pattern
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		var domain []string
		if q.dir != "." {
			domain = strings.Split(q.dir, "/")
		}
		patterns = append(patterns, w.gitignore(q.dir, domain)...)
		m := gitignore.NewMatcher(patterns)
		entries, err := w.readDir(q.dir, q.vetted)
		if err != nil {
			if q.dir == "." {
				return nil, fmt.Errorf("failed to list workspace: %w", err)
			}
			continue
		}
		for _, d := range entries {
			p := path.Join(q.dir, d.Name())
			if d.IsDir() && d.Name() == ".git" {
				continue
			}
			info, err := d.Info()
			if err != nil || slices.ContainsFunc(stores, func(s os.FileInfo) bool { return os.SameFile(info, s) }) {
				continue
			}
			if m.Match(strings.Split(p, "/"), d.IsDir()) {
				continue
			}
			if d.IsDir() {
				out = append(out, p+"/")
				queue = append(queue, queued{dir: p, vetted: info})
			} else {
				out = append(out, p)
			}
			if len(out) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// gitignore returns the patterns of dir's .gitignore, none if it cannot be read.
// It goes through Open so that a .gitignore linking into a store is not read.
func (w *Workspace) gitignore(dir string, domain []string) []gitignore.Pattern {
	f, err := w.Open(path.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var patterns []gitignore.Pattern
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, gitignore.ParsePattern(line, domain))
		}
	}
	return patterns
}

// readDir returns dir's entries sorted by name. dir is opened once and must still
// be the directory vetted was taken from, so a swap after vetting (say for a
// symlink into a store) is not followed.
func (w *Workspace) readDir(dir string, vetted fs.FileInfo) ([]fs.DirEntry, error) {
	// O_NONBLOCK: dir may have been swapped for a FIFO since it was vetted.
	f, err := w.root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if vetted != nil {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !os.SameFile(info, vetted) {
			return nil, fmt.Errorf("%w: %s", ErrOutsideWorkspace, dir)
		}
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
