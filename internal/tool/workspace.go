package tool

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/keyute/kopi/internal/policy"
)

// rootEscapeMessage is the text of os.Root's unexported errPathEscapes. Go
// exports no sentinel for it (os.ErrPathEscapes exists only in os's own
// tests), so the inner error of the *fs.PathError is compared by text.
const rootEscapeMessage = "path escapes from parent"

// maxSymlinks bounds symlink expansion in resolve; os.Root gives up after the
// same number (rootMaxSymlinks in go1.27).
const maxSymlinks = 8

type Workspace struct {
	root *os.Root
	// given is the absolute workspace path as passed to NewWorkspace; realDir
	// is its symlink-resolved form. Absolute paths may be expressed against
	// either.
	given, realDir string
	// protected holds paths, not identities: stores are rewritten by
	// rename-over (new inode) and may appear after startup, so they are
	// stat'd afresh on every Open.
	protected []string
}

func NewWorkspace(dir string, protected []string) (*Workspace, error) {
	given, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace %s: %w", dir, err)
	}
	realDir, err := filepath.EvalSymlinks(given)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace %s: %w", dir, err)
	}
	for _, p := range protected {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("protected path %q is not absolute", p)
		}
	}
	root, err := os.OpenRoot(realDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open workspace %s: %w", dir, err)
	}
	return &Workspace{root: root, given: given, realDir: realDir, protected: protected}, nil
}

func (w *Workspace) Open(path string) (*os.File, error) {
	rel, err := w.relative(path)
	if err != nil {
		return nil, err
	}
	// O_NONBLOCK stops a FIFO open from waiting for a writer, which nothing
	// could interrupt; it has no effect on regular files and directories.
	f, err := w.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// The protected check runs before the failure is reported, so a path
		// into a protected store is refused alike whether or not it exists.
		_, protected, rerr := w.resolve(rel)
		if rerr != nil {
			return nil, rerr
		}
		if protected {
			return nil, fmt.Errorf("%w: %s", ErrProtected, path)
		}
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			if pe.Err.Error() == rootEscapeMessage {
				return nil, fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
			}
			err = pe.Err
		}
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	if err := w.check(f, rel, path); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (w *Workspace) Close() error { return w.root.Close() }

// Target applies no confinement: the permission rules and the tool's own Run
// decide what may be touched. Real is walked as Open and WriteFile walk it, so
// the rules judge the file the tool acts on.
func (w *Workspace) Target(path string) (policy.Target, error) {
	abs := path
	if !filepath.IsAbs(path) {
		abs = filepath.Join(w.given, path)
	}
	// A path inside the workspace is walked from realDir, as Open and WriteFile
	// walk it, so a repointed workspace symlink cannot split the two.
	start, rest := string(filepath.Separator), path
	if rel, err := w.relative(path); err == nil {
		start, rest = w.realDir, rel
	}
	resolved, _, err := walk(start, rest, nil)
	if err != nil {
		return policy.Target{}, err
	}
	return policy.Target{Path: filepath.Clean(abs), Real: resolved}, nil
}

// WriteFile creates path, or replaces it atomically, with data. A symlink is
// written through: its target is replaced, not the link.
//
// The checks run before the temp file is renamed over the target, so a
// directory swapped for a symlink in between can still redirect the rename,
// within the workspace only (os.Root confines it). Kopi runs write calls one
// at a time, so only another process could race them.
func (w *Workspace) WriteFile(path string, data []byte) error {
	rel, err := w.relative(path)
	if err != nil {
		return err
	}
	target, protected, err := w.resolve(rel)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("%w: %s", ErrProtected, path)
	}
	// resolve compares identities, so it cannot see a store that does not
	// exist yet and that this write would create.
	inStore, err := w.inProtectedPath(target)
	if err != nil {
		return err
	}
	if inStore {
		return fmt.Errorf("%w: %s", ErrProtected, path)
	}
	// Every root call uses the resolved path, so what was checked is what is
	// written.
	rel, err = filepath.Rel(w.realDir, target)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
	}
	// A nil mode leaves a new file with the umask applied to 0o666.
	var mode *fs.FileMode
	info, err := w.root.Lstat(rel)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("%w: %s", ErrIsDirectory, path)
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s", ErrNotRegular, path)
	case err == nil:
		perm := info.Mode().Perm()
		mode = &perm
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if err := w.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return fmt.Errorf("failed to create parent of %s: %w", path, err)
	}
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+"."+rand.Text()+".tmp")
	tempMode := fs.FileMode(0o666)
	if mode != nil {
		// Keep replacement data private until the original mode is restored.
		tempMode = 0o600
	}
	f, err := w.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tempMode)
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", path, err)
	}
	if err := w.fillTemp(f, tmp, path, data, mode); err != nil {
		_ = w.root.Remove(tmp)
		return err
	}
	if err := w.root.Rename(tmp, rel); err != nil {
		_ = w.root.Remove(tmp)
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

// fillTemp writes data to the temp file f and closes it. check binds the temp
// file's identity to its resolved path, as it does for a read.
func (w *Workspace) fillTemp(f *os.File, tmp, path string, data []byte, mode *fs.FileMode) error {
	err := w.check(f, tmp, path)
	if err == nil {
		if _, werr := f.Write(data); werr != nil {
			err = fmt.Errorf("failed to write %s: %w", path, werr)
		}
	}
	if err == nil {
		if serr := f.Sync(); serr != nil {
			err = fmt.Errorf("failed to sync %s: %w", path, serr)
		}
	}
	if err == nil && mode != nil {
		if cerr := f.Chmod(*mode); cerr != nil {
			err = fmt.Errorf("failed to set mode of %s: %w", path, cerr)
		}
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("failed to close %s: %w", path, cerr)
	}
	return err
}

// inProtectedPath reports whether target lies lexically in a protected path,
// itself walked to its real path. APFS is case-insensitive, so darwin compares
// case-folded.
func (w *Workspace) inProtectedPath(target string) (bool, error) {
	for _, p := range w.protected {
		store, _, err := walk(string(filepath.Separator), p, nil)
		if err != nil {
			return false, err
		}
		if runtime.GOOS == "darwin" {
			store, target = strings.ToLower(store), strings.ToLower(target)
		}
		if rel, err := filepath.Rel(store, target); err == nil && filepath.IsLocal(rel) {
			return true, nil
		}
	}
	return false, nil
}

func (w *Workspace) relative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return path, nil
	}
	// filepath.Rel would clean the path, applying a ".." before the symlink it
	// follows has been expanded; only no-op components are dropped here, and
	// os.Root walks the rest.
	parts := components(path)
	for _, base := range []string{w.given, w.realDir} {
		prefix := components(base)
		if len(parts) >= len(prefix) && slices.Equal(parts[:len(prefix)], prefix) {
			if rest := parts[len(prefix):]; len(rest) > 0 {
				return strings.Join(rest, string(filepath.Separator)), nil
			}
			return ".", nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
}

// components splits path into its names, dropping the empty and "." ones and
// keeping "..".
func components(path string) []string {
	return slices.DeleteFunc(strings.Split(path, string(filepath.Separator)), func(name string) bool {
		return name == "" || name == "."
	})
}

// check runs after f was opened through os.Root. It refuses f if resolving
// rel enters a protected entry, if f is not the file rel resolves to now, or
// if it is neither a regular file nor a directory.
func (w *Workspace) check(f *os.File, rel, path string) error {
	target, protected, err := w.resolve(rel)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("%w: %s", ErrProtected, path)
	}
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}
	resolved, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}
	// rel was resolved after os.Root opened it, so a rename or symlink swap in
	// between could make the protected check judge a different file than the
	// one handed back; requiring the same identity binds the check to f.
	if !os.SameFile(opened, resolved) {
		return fmt.Errorf("%w: %s", ErrOutsideWorkspace, path)
	}
	if !opened.Mode().IsRegular() && !opened.IsDir() {
		return fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	return nil
}

// resolve walks rel from the workspace and returns the real path reached and
// whether any step lies in a protected entry. Every step is checked, not only
// the end, so a path that fails or leaves again inside a store (a dangling or
// outward symlink there) is refused too, and a store's contents cannot be
// probed through error kinds.
//
// The workspace may be the user's home, so stores can lie inside it. Identity
// is compared with os.SameFile rather than path prefixes because APFS is
// case-insensitive (~/.SSH is ~/.ssh) and symlinks can name a store under
// another spelling.
func (w *Workspace) resolve(rel string) (string, bool, error) {
	stores, err := w.statProtected()
	if err != nil {
		return "", false, err
	}
	isProtected := func(info os.FileInfo) bool {
		return slices.ContainsFunc(stores, func(s os.FileInfo) bool { return os.SameFile(info, s) })
	}
	// The workspace may itself lie inside a store. Every ancestor of a later
	// step is one of these or an earlier step, so steps need no ancestor walk.
	for p := w.realDir; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return "", false, fmt.Errorf("failed to stat %s: %w", p, err)
		}
		if isProtected(info) {
			return p, true, nil
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return walk(w.realDir, rel, isProtected)
}

// walk follows path from dir, a real directory, one component at a time as
// os.Root does: each symlink is expanded before a later ".." is applied, so
// path must not be cleaned first. A missing component is taken as the
// directory WriteFile would create, and the walk goes on past it. stop, if not
// nil, sees every existing step that is not a symlink; walk returns that step
// and true as soon as stop reports true. Failures other than a missing
// component are returned, so callers fail closed.
func walk(dir, path string, stop func(os.FileInfo) bool) (string, bool, error) {
	sep := string(filepath.Separator)
	dest := dir
	parts := strings.Split(path, sep)
	for links := 0; len(parts) > 0; {
		name := parts[0]
		parts = parts[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			dest = filepath.Dir(dest)
			continue
		}
		next := filepath.Join(dest, name)
		info, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			dest = next
			continue
		case err != nil:
			return "", false, fmt.Errorf("failed to resolve %s: %w", path, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			links++
			if links > maxSymlinks {
				return "", false, fmt.Errorf("failed to resolve %s: %w", path, syscall.ELOOP)
			}
			link, err := os.Readlink(next)
			if err != nil {
				return "", false, fmt.Errorf("failed to resolve %s: %w", path, err)
			}
			if filepath.IsAbs(link) {
				dest = sep
			}
			parts = append(strings.Split(link, sep), parts...)
			continue
		}
		dest = next
		if stop != nil && stop(info) {
			return dest, true, nil
		}
	}
	return dest, false, nil
}

// statProtected returns the current identities of the protected paths that
// exist. Any other stat failure is returned so the caller fails closed rather
// than treating an unknown store as absent.
func (w *Workspace) statProtected() ([]os.FileInfo, error) {
	var stores []os.FileInfo
	for _, p := range w.protected {
		info, err := os.Stat(p)
		// ENOTDIR means a parent is not a directory, so the path cannot name a
		// store any more than a missing one can.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to check protected path: %w", err)
		}
		stores = append(stores, info)
	}
	return stores, nil
}
