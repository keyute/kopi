// The mount plan is adapted from src/sandbox/linux-sandbox-utils.ts of
// Anthropic's sandbox-runtime, https://github.com/anthropic-experimental/sandbox-runtime
// at commit 0bbebed0aa9a8e628dc4df8bc30293550a1988eb.
// Copyright 2025 Anthropic
// Licensed under the Apache License, Version 2.0; see srt/LICENSE in this directory.
// Modified by the Kopi authors: ported to Go; kept the placeholders for absent
// denied paths, their removal once no command runs in any Kopi sharing a
// directory, coordinated with a lock and creation-identity records, the ancestor
// pins and the symlink refusal; dropped the / write root cover, 9p and WSL
// handling, the ripgrep
// scan, the network proxy, the argument file and read carve-outs; the seccomp
// filter goes to bwrap's --seccomp rather than through a helper binary; empty
// directories are read-only tmpfs mounts rather than a directory in the
// temporary directory; and a symlink on a denied path inside a write root
// stops the command instead of being masked.

package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/keyute/kopi/internal/policy"
)

// bwrap runs commands under bubblewrap.
type bwrap struct {
	path    string // absolute path of the bwrap binary
	policy  Policy
	shared  *placeholders
	seccomp []byte // see seccompFilter
}

// command builds the arguments from the filesystem as it is now: what exists
// decides what can be mounted.
func (b *bwrap) command(argv []string) (*exec.Cmd, func(), error) {
	// Locked before looking, so that no placeholder seen here is removed
	// before this command's bwrap has mounted it.
	lock, err := b.shared.start()
	if err != nil {
		return nil, nil, err
	}
	args, made, err := bwrapArgs(b.policy)
	if err == nil {
		err = b.shared.prepare(made)
	}
	if err != nil {
		b.shared.finish(lock)
		return nil, nil, err
	}
	filter, err := seccompPipe(b.seccomp)
	if err != nil {
		b.shared.finish(lock)
		return nil, nil, err
	}
	cmd := exec.Command(b.path, slices.Concat(args, argv)...)
	cmd.ExtraFiles = []*os.File{filter}
	return cmd, func() {
		_ = filter.Close()
		b.shared.finish(lock)
	}, nil
}

// bwrapFlags are the namespaces every command runs in. Without the PID
// namespace /proc/<pid>/root of a host process is a way out. bwrap drops
// capabilities by itself only for a caller that is not root, and with them a
// command could unmount its denies. No --new-session: the caller kills the
// command by its process group, which a new session would leave.
func bwrapFlags(network bool) []string {
	flags := []string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--cap-drop", "ALL"}
	if !network {
		flags = append(flags, "--unshare-net")
	}
	return flags
}

var bwrapSpecials = []string{"--dev", "/dev", "--proc", "/proc"}

// The seccomp filter is read from fd 3, the command's ExtraFiles[0].
var bwrapTail = []string{"--seccomp", "3", "--"}

// emptyDir mounts an empty read-only directory at path. A tmpfs needs no
// source directory, which would otherwise have to live where commands cannot write.
func emptyDir(path string) []string {
	return []string{"--perms", "555", "--tmpfs", path, "--remount-ro", path}
}

// probeBwrap fails when bwrap is missing or cannot set up the namespaces and
// mounts commands rely on, as when unprivileged user namespaces are off or
// AppArmor restricts them. Its empty directory goes over /sys, which holds no
// program the probe could need to find.
func probeBwrap(ctx context.Context, path string, seccomp []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	filter, err := seccompPipe(seccomp)
	if err != nil {
		return err
	}
	defer filter.Close()
	args := slices.Concat(bwrapFlags(false), []string{"--ro-bind", "/", "/"}, emptyDir("/sys"), bwrapSpecials, bwrapTail, []string{"true"})
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.ExtraFiles = []*os.File{filter}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: failed to run %s: %w: %s", ErrUnavailable, path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// bwrapArgs returns the bwrap arguments that apply p, through "--", and the
// destinations Kopi must create on the host for denied paths found absent.
//
// Later mounts cover earlier ones, so the order is: the read-only root; the
// pins; the write roots; the device and proc mounts; then the denies, so that
// a write root inside a denied path ends up read-only. Write denies come
// before read denies: a bind
// of a denied directory would uncover a read mask inside it, while a mask
// over a directory hides any bind inside it.
func bwrapArgs(p Policy) (args []string, made []placeholderDestination, err error) {
	var roots []string
	for _, w := range p.Write {
		r := policy.Resolve(w)
		if _, err := os.Stat(r); err == nil {
			roots = appendNew(roots, r)
		}
	}
	slices.SortStableFunc(roots, byDepth)
	inRoot := func(path string) bool {
		return slices.ContainsFunc(roots, func(r string) bool { return within(path, r) })
	}

	// Check each link before resolving it, including links reached through
	// safe parents or through another link's target.
	for _, d := range slices.Concat(p.DenyRead, p.DenyWrite) {
		if err := checkDeniedSymlinks(d, inRoot); err != nil {
			return nil, nil, err
		}
	}

	// A path denied both ways is denied reading.
	var paths []string
	read := map[string]bool{}
	for _, d := range p.DenyRead {
		r := policy.Resolve(d)
		paths = appendNew(paths, r)
		read[r] = true
	}
	for _, d := range p.DenyWrite {
		paths = appendNew(paths, policy.Resolve(d))
	}
	above := map[string]bool{}
	for _, path := range paths {
		for d := filepath.Dir(path); d != filepath.Dir(d); d = filepath.Dir(d) {
			above[d] = true
		}
	}

	states := make([]pathState, len(paths))
	var deniedDirs, readDirs []string
	for i, path := range paths {
		st := inspect(path)
		switch {
		case st.blocked != "" && inRoot(st.blocked):
			// The command could change what blocks the look.
			return nil, nil, fmt.Errorf("failed to inspect denied path %s: %w", path, st.err)
		case st.link != "" && inRoot(st.link):
			return nil, nil, fmt.Errorf("%w: %s", ErrDeniedSymlink, st.link)
		case st.info != nil && st.info.IsDir():
			deniedDirs = append(deniedDirs, path)
			if read[path] {
				readDirs = append(readDirs, path)
			}
		}
		states[i] = st
	}
	// writable reports whether a command may change the entries of dir.
	writable := func(dir string) bool {
		return inRoot(dir) && !slices.ContainsFunc(deniedDirs, func(d string) bool { return within(dir, d) })
	}

	var denyWrite, denyRead, seeds, decided []string
	for i, path := range paths {
		st := states[i]
		switch {
		case st.blocked != "", st.link != "":
			// Outside the write roots, which the command cannot change: nothing
			// is mounted for the path.
		case st.info != nil && read[path]:
			if slices.ContainsFunc(readDirs, func(d string) bool { return path != d && within(path, d) }) {
				continue
			}
			if st.info.IsDir() {
				denyRead = append(denyRead, emptyDir(path)...)
			} else {
				denyRead = append(denyRead, "--ro-bind", "/dev/null", path)
			}
			seeds = append(seeds, path)
		case st.info != nil:
			if inRoot(path) || slices.ContainsFunc(roots, func(r string) bool { return within(r, path) }) {
				denyWrite = append(denyWrite, "--ro-bind", path, path)
				seeds = append(seeds, path)
			}
		case slices.ContainsFunc(decided, func(d string) bool { return within(path, d) }):
		case st.file != "":
			// Nothing can be made under a file while it is there, so it
			// stays there, unless it is denied itself or already read-only.
			if slices.Contains(paths, st.file) || !writable(filepath.Dir(st.file)) {
				continue
			}
			decided = append(decided, st.file)
			denyWrite = append(denyWrite, "--ro-bind", st.file, st.file)
			seeds = append(seeds, st.file)
		case writable(filepath.Dir(st.missing)):
			dest := st.missing
			decided = append(decided, dest)
			// A directory where denied paths lie beneath, so that tools
			// expecting one there still find it.
			if above[dest] {
				denyWrite = append(denyWrite, emptyDir(dest)...)
			} else {
				denyWrite = append(denyWrite, "--ro-bind", "/dev/null", dest)
			}
			seeds = append(seeds, dest)
			made = append(made, placeholderDestination{path: dest, directory: above[dest]})
		}
	}

	// A mount point cannot be renamed or removed, even under a later mount
	// that covers it, so a read-only self-bind of each directory between a
	// denied path and its write root keeps the path where its mount is.
	// Placed before the write roots, which cover them and so keep them
	// writable. A write root needs none: its own bind is a mount point.
	var pins []string
	for _, s := range seeds {
		for d := filepath.Dir(s); d != filepath.Dir(d) && inRoot(d); d = filepath.Dir(d) {
			if !slices.Contains(roots, d) {
				pins = appendNew(pins, d)
			}
		}
	}
	slices.SortStableFunc(pins, byDepth)

	args = append(bwrapFlags(p.Network), "--ro-bind", "/", "/")
	for _, d := range pins {
		args = append(args, "--ro-bind", d, d)
	}
	for _, r := range roots {
		args = append(args, "--bind", r, r)
	}
	args = slices.Concat(args, bwrapSpecials, denyWrite, denyRead, bwrapTail)
	return args, made, nil
}

// checkDeniedSymlinks walks in filesystem order: cleaning first would erase
// a link before "..", and resolving the whole path would erase its entry.
func checkDeniedSymlinks(path string, inRoot func(string) bool) error {
	parts := strings.Split(path, string(filepath.Separator))
	current := string(filepath.Separator)
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if err != nil {
			return nil // inspect handles missing or inaccessible components.
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			current = next
			continue
		}
		if inRoot(next) {
			return fmt.Errorf("%w: %s", ErrDeniedSymlink, next)
		}
		links++
		if links > 255 {
			return nil // Like EvalSymlinks, stop at a link cycle.
		}
		target, err := os.Readlink(next)
		if err != nil {
			return nil
		}
		if filepath.IsAbs(target) {
			current = string(filepath.Separator)
		}
		parts = append(strings.Split(target, string(filepath.Separator)), parts...)
	}
	return nil
}

// pathState is what walking a path from the root without following symlinks
// found: the path itself, or what stopped the walk.
type pathState struct {
	info    fs.FileInfo // the path exists
	missing string      // the first component that does not exist
	file    string      // a non-directory above the path
	link    string      // a symlink at or above the path
	blocked string      // a component that could not be looked at, with err
	err     error
}

func inspect(path string) pathState {
	var parts []string
	for d := path; ; d = filepath.Dir(d) {
		parts = append(parts, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	slices.Reverse(parts)
	for _, c := range parts {
		info, err := os.Lstat(c)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return pathState{missing: c}
		case err != nil:
			return pathState{blocked: c, err: err}
		case info.Mode()&fs.ModeSymlink != 0:
			return pathState{link: c}
		case c == path:
			return pathState{info: info}
		case !info.IsDir():
			return pathState{file: c}
		}
	}
	return pathState{}
}

// within reports whether path is dir or lies under it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

func byDepth(a, b string) int {
	return cmp.Compare(strings.Count(a, string(filepath.Separator)), strings.Count(b, string(filepath.Separator)))
}

type placeholderDestination struct {
	path      string
	directory bool
}

type placeholderOwnership struct {
	Path   string  `json:"path"`
	Device *uint64 `json:"device"`
	Inode  *uint64 `json:"inode"`
	Kind   string  `json:"kind"`
}

// placeholders tracks only Kopi's successful creations. Each command holds a
// shared lock for its lifetime; whoever ends last takes the lock exclusively
// and sweeps committed ownership records, including those of killed processes.
// A kill between creation and recording can leave an unowned orphan.
type placeholders struct {
	lockPath   string
	recordPath string
}

func newPlaceholders(dir string) *placeholders {
	return &placeholders{lockPath: filepath.Join(dir, "lock"), recordPath: filepath.Join(dir, "placeholders")}
}

// start takes a shared lock of its own: flock locks belong to the open file,
// so commands of one process must not share one.
func (pl *placeholders) start() (*flock.Flock, error) {
	lock := flock.New(pl.lockPath)
	if err := lock.RLock(); err != nil {
		return nil, fmt.Errorf("failed to lock sandbox placeholders: %w", err)
	}
	return lock, nil
}

// placeholderParent pins each parent component and rejects symlink replacements.
func placeholderParent(path string) (*os.Root, error) {
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	parent, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(volume, filepath.Dir(path))
	if err != nil {
		parent.Close()
		return nil, err
	}
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		if name == "." {
			continue
		}
		child, err := parent.OpenRoot(name)
		if err != nil {
			parent.Close()
			return nil, err
		}
		entry, lerr := parent.Lstat(name)
		opened, serr := child.Stat(".")
		parent.Close()
		if lerr != nil || serr != nil || !entry.IsDir() || !os.SameFile(entry, opened) {
			child.Close()
			return nil, fmt.Errorf("failed to pin parent of %s", path)
		}
		parent = child
	}
	return parent, nil
}

func createPlaceholder(dest placeholderDestination) (*placeholderOwnership, error) {
	parent, err := placeholderParent(dest.path)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare sandbox placeholder: %w", err)
	}
	defer parent.Close()
	name := filepath.Base(dest.path)
	var info fs.FileInfo
	var f *os.File
	kind := "file"
	if dest.directory {
		kind = "directory"
		err = parent.Mkdir(name, 0o700)
		if err == nil {
			f, err = parent.Open(name)
			if err == nil {
				defer f.Close()
				info, err = f.Stat()
				entry, lerr := parent.Lstat(name)
				if err == nil && (lerr != nil || !entry.IsDir() || !os.SameFile(entry, info)) {
					return nil, fmt.Errorf("failed to identify sandbox placeholder %s", dest.path)
				}
			}
		}
	} else {
		f, err = parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			defer f.Close()
			info, err = f.Stat()
		}
	}
	if errors.Is(err, fs.ErrExist) {
		return nil, nil // Existing entries never confer ownership.
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox placeholder: %w", err)
	}
	device, inode, ok := placeholderIdentity(f, info)
	if !ok {
		return nil, fmt.Errorf("failed to identify sandbox placeholder %s", dest.path)
	}
	return &placeholderOwnership{Path: dest.path, Device: &device, Inode: &inode, Kind: kind}, nil
}

func (pl *placeholders) prepare(destinations []placeholderDestination) error {
	for _, dest := range destinations {
		owned, err := createPlaceholder(dest)
		if err != nil {
			return err
		}
		if owned != nil {
			if err := pl.record([]placeholderOwnership{*owned}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (pl *placeholders) record(made []placeholderOwnership) error {
	if len(made) == 0 {
		return nil
	}
	line, err := json.Marshal(made)
	if err != nil {
		return fmt.Errorf("failed to record sandbox placeholders: %w", err)
	}
	f, err := os.OpenFile(pl.recordPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("failed to record sandbox placeholders: %w", err)
	}
	line = append(append([]byte{'\n'}, line...), '\n')
	n, err := f.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("failed to record sandbox placeholders: %w", err)
	}
	return nil
}

// finish ends one command, holding lock. If no other command runs anywhere, it
// removes the recorded placeholders while no command can start meanwhile.
func (pl *placeholders) finish(lock *flock.Flock) {
	if err := lock.Unlock(); err != nil {
		slog.Warn("failed to unlock sandbox placeholders", "err", err)
	}
	sweep := flock.New(pl.lockPath)
	ok, err := sweep.TryLock()
	if err != nil {
		slog.Warn("failed to lock sandbox placeholders", "err", err)
		return
	}
	if !ok {
		return
	}
	defer func() {
		if err := sweep.Unlock(); err != nil {
			slog.Warn("failed to unlock sandbox placeholders", "err", err)
		}
	}()
	data, err := os.ReadFile(pl.recordPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to read the sandbox placeholder record", "err", err)
		}
		return
	}
	var paths []placeholderOwnership
	for line := range bytes.Lines(data) {
		// A trailing fragment is not a committed record.
		if len(line) == 0 || line[len(line)-1] != '\n' {
			continue
		}
		var made []placeholderOwnership
		if json.Unmarshal(line, &made) == nil {
			paths = append(paths, made...)
		}
	}
	// Deepest first: a directory goes only once empty.
	slices.SortFunc(paths, func(a, b placeholderOwnership) int { return cmp.Compare(len(b.Path), len(a.Path)) })
	for _, p := range paths {
		removePlaceholder(p)
	}
	if err := os.Truncate(pl.recordPath, 0); err != nil {
		slog.Warn("failed to clear the sandbox placeholder record", "err", err)
	}
}

// Identity checks are best-effort: outside writers can race the final removal.
func removePlaceholder(owned placeholderOwnership) {
	if !filepath.IsAbs(owned.Path) || owned.Device == nil || owned.Inode == nil ||
		(owned.Kind != "file" && owned.Kind != "directory") {
		return
	}
	path := owned.Path
	parent, err := placeholderParent(path)
	if err != nil {
		return
	}
	defer parent.Close()
	name := filepath.Base(path)
	info, err := parent.Lstat(name)
	if err != nil {
		return
	}
	device, inode, ok := placeholderEntryIdentity(parent, name, info)
	if !ok || device != *owned.Device || inode != *owned.Inode {
		return
	}
	switch {
	case info.Mode().IsRegular() && owned.Kind == "file":
		if info.Size() != 0 {
			return
		}
	case info.IsDir() && owned.Kind == "directory":
		if !isEmptyDir(parent, name) {
			return
		}
	default:
		return
	}
	if err := parent.Remove(name); err != nil {
		slog.Warn("failed to remove a sandbox placeholder", "path", path, "err", err)
	}
}

func isEmptyDir(parent *os.Root, name string) bool {
	f, err := parent.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return errors.Is(err, io.EOF)
}
