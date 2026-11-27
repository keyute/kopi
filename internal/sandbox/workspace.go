package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

// Workspace is what the sandboxes of a run are built from. Relative paths in
// AllowWrite and DenyRead start at CWD.
type Workspace struct {
	CWD, UserHome, KopiHome string
	AllowWrite, DenyRead    []string
	Network                 bool
	// Proxy, when set, is the only way commands reach the network.
	Proxy *Proxy
}

// OpenWorkspace returns the sandboxes Bash runs under in execute and in plan
// mode, made in dir. setDirs makes the added directories the workspace's other
// roots in both; validateDirs checks them without applying them. The error is
// New's, so it can still be tested for ErrUnavailable.
func OpenWorkspace(ctx context.Context, dir string, w Workspace) (execute, plan *Sandbox, setDirs, validateDirs func(dirs []string) error, err error) {
	p := workspacePolicy(w)
	sandboxes, err := New(ctx, dir, p, planPolicy(p, w.CWD, nil))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// Keep each root's original protection list even if its git metadata changes.
	protected := map[string][]string{w.CWD: slices.Clone(p.DenyWrite)}
	build := func(dirs []string, remember bool) Policy {
		next := p
		next.Write = slices.Clone(p.Write)
		next.DenyWrite = slices.Clone(p.DenyWrite)
		for _, dir := range dirs {
			paths, ok := protected[dir]
			if !ok {
				paths = protectedWrites(dir)
				if remember {
					protected[dir] = paths
				}
			}
			next.Write = append(next.Write, dir)
			next.DenyWrite = append(next.DenyWrite, paths...)
		}
		return next
	}
	validateDirs = func(dirs []string) error {
		next := build(dirs, false)
		if err := sandboxes[1].ValidatePolicy(planPolicy(next, w.CWD, dirs)); err != nil {
			return err
		}
		return sandboxes[0].ValidatePolicy(next)
	}
	// The plan sandbox first: a failure between the two leaves the directory
	// read-only rather than writable.
	setDirs = func(dirs []string) error {
		next := build(dirs, true)
		if err := sandboxes[1].SetPolicy(planPolicy(next, w.CWD, dirs)); err != nil {
			return err
		}
		return sandboxes[0].SetPolicy(next)
	}
	return sandboxes[0], sandboxes[1], setDirs, validateDirs, nil
}

// workspacePolicy lets commands write only the start directory, temporary
// directories and the paths the user allowed, never read Kopi's home, and
// never write the git files that decide which config and hooks git runs (on
// Linux, nothing in a workspace directory's .git), nor any .mcp.json or what a
// workspace directory's .mcp.json resolves to, either of which would run code
// outside the sandbox.
func workspacePolicy(w Workspace) Policy {
	expand := func(paths []string) []string {
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			switch {
			case filepath.IsAbs(p):
			case p == "~":
				p = w.UserHome
			case strings.HasPrefix(p, "~/"):
				p = w.UserHome + string(filepath.Separator) + p[2:]
			default:
				p = w.CWD + string(filepath.Separator) + p
			}
			out = append(out, p)
		}
		return out
	}
	denyWrite := protectedWrites(w.CWD)
	write := []string{w.CWD, "/tmp", os.TempDir()}
	write = append(write, expand(w.AllowWrite)...)
	return Policy{
		Write:          write,
		DenyRead:       append([]string{w.KopiHome}, expand(w.DenyRead)...),
		DenyWrite:      denyWrite,
		DenyWriteNames: []string{policy.ProjectMCPFile},
		Network:        w.Network,
		Proxy:          w.Proxy,
	}
}

// protectedWrites are the paths under a workspace directory that commands may
// not write.
func protectedWrites(dir string) []string {
	gitDir := filepath.Join(dir, ".git")
	// The names stay listed on Linux, though gitDir covers them: with paths
	// listed beneath it, a missing .git gets a directory placeholder rather than
	// an empty file, so git in a subdirectory of another repository still walks
	// up. The whole directory is denied there because bubblewrap can block
	// creating a path only by mounting a placeholder on it, and git refuses an
	// empty commondir and, in a reftable repository, any commondir.
	var denyWrite []string
	for _, name := range []string{"hooks", "config", "commondir", "config.worktree", "worktrees", "modules"} {
		denyWrite = append(denyWrite, filepath.Join(gitDir, name))
	}
	_, err := os.Lstat(gitDir)
	missing := errors.Is(err, os.ErrNotExist)
	if missing || runtime.GOOS == "linux" {
		// Git takes a directory holding HEAD, objects and refs for a git
		// directory and runs its config. On Linux another command's .git
		// placeholder can hide that .git was missing.
		denyWrite = append(denyWrite, filepath.Join(dir, "HEAD"))
	}
	if runtime.GOOS == "linux" {
		denyWrite = append(denyWrite, gitDir)
	} else if info, err := os.Stat(gitDir); !missing && (err != nil || !info.IsDir()) {
		// A .git file, as in a linked worktree or submodule, names the git directory.
		denyWrite = append(denyWrite, gitDir)
	}
	// Unconditional: the sandbox also denies what a link resolves to, and a
	// linked directory is denied whole rather than left to be swapped for a file.
	return append(denyWrite, filepath.Join(dir, policy.ProjectMCPFile))
}

// planPolicy is p with the workspace denied rather than dropped from the write
// roots: a workspace under /tmp, the temporary directory or an allowWrite path
// would stay writable through that root.
func planPolicy(p Policy, cwd string, dirs []string) Policy {
	p.DenyWrite = slices.Concat(p.DenyWrite, []string{cwd}, dirs)
	return p
}
