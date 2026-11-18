package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

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
	var out []string
	err = w.walk(context.Background(), ".", nil, func(p string, info fs.FileInfo) bool {
		if info.IsDir() {
			p += "/"
		}
		out = append(out, p)
		return len(out) != limit
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// walk visits the entries below dir breadth-first, each directory's in name order,
// as slash-separated workspace paths with their Lstat info; visit returning false
// ends it. dir has no symlinks; vetted is its identity, nil only for ".". Skips
// .git, ignored and protected entries and does not follow symlinks. Ancestors'
// patterns apply unless they match dir itself. Only failing to list dir is returned.
func (w *Workspace) walk(ctx context.Context, dir string, vetted fs.FileInfo, visit func(p string, info fs.FileInfo) bool) error {
	type queued struct {
		dir      string
		patterns []gitignore.Pattern
		// vetted is the identity dir had when its parent was listed.
		vetted fs.FileInfo
	}
	var ancestors []gitignore.Pattern
	if dir != "." {
		parts := split(dir)
		for i := range parts {
			a := "."
			if i > 0 {
				a = path.Join(parts[:i]...)
			}
			ancestors = append(ancestors, w.gitignore(a, split(a))...)
		}
		// A search pointed explicitly inside an ignored directory searches it,
		// as ripgrep does for an explicit path, so the patterns that ignore dir
		// are dropped and the others keep applying.
		ancestors = slices.DeleteFunc(ancestors, func(p gitignore.Pattern) bool {
			return p.Match(parts, true) != gitignore.NoMatch
		})
	}
	// Breadth-first, so a limit keeps shallow entries and a directory's
	// patterns are loaded before its children's. Each pattern is scoped to its
	// directory, and later patterns take priority, as in git.
	queue := []queued{{dir: dir, patterns: ancestors, vetted: vetted}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		q := queue[0]
		queue = queue[1:]
		entries, err := w.readDir(q.dir, q.vetted)
		if err != nil {
			if q.dir != dir {
				continue
			}
			if dir == "." {
				return fmt.Errorf("failed to list workspace: %w", err)
			}
			return fmt.Errorf("failed to list %s: %w", dir, err)
		}
		// Stat'd after each read, so what is visited was read while not a store.
		stores, err := w.statProtected()
		if err != nil {
			return err
		}
		if q.vetted != nil && isStore(stores, q.vetted) {
			continue
		}
		// Appending must not overwrite patterns shared by queued siblings.
		patterns := q.patterns[:len(q.patterns):len(q.patterns)]
		patterns = append(patterns, w.gitignore(q.dir, split(q.dir))...)
		m := gitignore.NewMatcher(patterns)
		for _, d := range entries {
			p := path.Join(q.dir, d.Name())
			if d.Name() == ".git" {
				continue
			}
			info, err := d.Info()
			if err != nil || isStore(stores, info) {
				continue
			}
			if m.Match(split(p), d.IsDir()) {
				continue
			}
			if d.IsDir() {
				queue = append(queue, queued{dir: p, patterns: patterns, vetted: info})
			}
			if !visit(p, info) {
				return nil
			}
		}
	}
	return nil
}

// split returns the names of a slash-separated workspace path, none for ".".
func split(p string) []string {
	if p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

// gitignore returns the patterns of dir's .gitignore, none if it cannot be read.
// Symlinks are ignored; Open also checks for protected file identities.
func (w *Workspace) gitignore(dir string, domain []string) []gitignore.Pattern {
	name := path.Join(dir, ".gitignore")
	info, err := w.root.Lstat(name)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	f, err := w.Open(name)
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
	f, err := w.openCanonical(dir)
	if err != nil {
		if errors.Is(err, ErrTargetChanged) {
			return nil, fmt.Errorf("%w: %s", ErrOutsideWorkspace, dir)
		}
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
	if err := w.check(f, dir, dir); err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}
