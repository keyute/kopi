package tool

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/keyute/kopi/internal/policy"
)

// Hidden reports whether a search by tool rooted at root leaves t out.
type Hidden func(tool string, root, t policy.Target) bool

// searchTarget returns a Tool.Target for a tool whose optional path argument
// names where it searches, the workspace by default. It runs before the
// arguments are validated, so it decodes leniently.
func searchTarget(ws *Workspace) func(json.RawMessage) (policy.Target, error) {
	return func(raw json.RawMessage) (policy.Target, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &args)
		if args.Path == "" {
			args.Path = "."
		}
		return ws.Target(args.Path)
	}
}

// searchRoot resolves where a search of path starts without reading any
// content. rel is the start's symlink-free, slash-separated workspace path and
// info its identity, which walk and readDir take as vetted (for "." as well).
// t is bound to the file Open vetted, so the gate judges what is searched.
func (w *Workspace) searchRoot(path string) (t policy.Target, rel string, info fs.FileInfo, err error) {
	f, err := w.Open(path)
	if err != nil {
		return policy.Target{}, "", nil, err
	}
	info, err = f.Stat()
	f.Close()
	if err != nil {
		return policy.Target{}, "", nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	t, err = w.Target(path)
	if err != nil {
		return policy.Target{}, "", nil, err
	}
	resolved, err := os.Lstat(t.Real)
	if err != nil || !os.SameFile(resolved, info) {
		return policy.Target{}, "", nil, fmt.Errorf("%w: %s", ErrTargetChanged, path)
	}
	rel, err = filepath.Rel(w.realDir, t.Real)
	if err != nil || !filepath.IsLocal(rel) {
		return policy.Target{}, "", nil, fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
	}
	return t, filepath.ToSlash(rel), info, nil
}

// openWalked opens the regular file walk visited at p with info. A file
// swapped since, say for a symlink or a FIFO, is refused rather than read.
func (w *Workspace) openWalked(p string, info fs.FileInfo) (*os.File, error) {
	// O_NONBLOCK: p may have been swapped for a FIFO since it was walked.
	f, err := w.root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", p, err)
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(opened, info) || !opened.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrTargetChanged, p)
	}
	if err := w.check(f, filepath.FromSlash(p), p); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// under returns walked path p relative to the search start rel, and the target
// it names below root, the start's target.
func under(root policy.Target, rel, p string) (sub string, t policy.Target) {
	sub = p
	if rel != "." {
		sub = strings.TrimPrefix(p, rel+"/")
	}
	return sub, policy.Target{
		Path: filepath.Join(root.Path, filepath.FromSlash(sub)),
		Real: filepath.Join(root.Real, filepath.FromSlash(sub)),
	}
}

// compileGlob returns a matcher of paths relative to the search start. A
// pattern without "/" matches a name at any depth; an empty one matches all.
func compileGlob(pattern string) (func(sub string) bool, error) {
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	if strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%w: glob %q must be relative to path", ErrInvalidArgs, pattern)
	}
	if !doublestar.ValidatePattern(pattern) {
		return nil, fmt.Errorf("%w: invalid glob %q", ErrInvalidArgs, pattern)
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	return func(sub string) bool { return doublestar.MatchUnvalidated(pattern, sub) }, nil
}

type found struct {
	path    string
	modTime time.Time
	count   int
}

func newestFirst(a, b found) int {
	if c := b.modTime.Compare(a.modTime); c != 0 {
		return c
	}
	return strings.Compare(a.path, b.path)
}

// keepNewest appends f to kept and, once kept holds 2*limit entries, trims it
// to the limit newest, so collecting stays bounded; callers still sort and cut
// the result.
func keepNewest(kept []found, f found, limit int) []found {
	kept = append(kept, f)
	if len(kept) >= 2*limit {
		slices.SortFunc(kept, newestFirst)
		kept = kept[:limit]
	}
	return kept
}

// foundSummary relies on the search tools' output starting with a "Found …"
// line or one of the no-result lines.
func foundSummary(output string) string {
	first, _, _ := strings.Cut(output, "\n")
	switch first {
	case "No matches found":
		return "no matches"
	case "No files found":
		return "no files"
	}
	return strings.TrimPrefix(first, "Found ")
}

// searchTitle returns a Display.Title such as "Grep TODO in internal". It
// runs before the arguments are validated, so it decodes leniently.
func searchTitle(verb string) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(raw, &args)
		switch {
		case args.Pattern == "":
			return verb
		case args.Path == "":
			return verb + " " + args.Pattern
		}
		return verb + " " + args.Pattern + " in " + args.Path
	}
}
