// Package sandbox confines commands with the operating system's sandbox.
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
)

// Policy is what a sandboxed command may do. Paths are absolute.
type Policy struct {
	Write     []string // roots commands may write under
	DenyRead  []string // paths commands may neither read nor write
	DenyWrite []string // paths commands may read but not write
	// DenyWriteNames are file names commands may not write, create, rename or
	// remove in any directory, matched in any letter case. Only macOS
	// enforces them.
	DenyWriteNames []string
	Network        bool
}

// Sandbox runs commands under one Policy.
type Sandbox struct {
	prefix []string // macOS: the sandbox-exec argv
	bwrap  *bwrap   // Linux
}

// New checks once that the platform sandbox works and builds one Sandbox per
// policy, in argument order. It returns ErrUnavailable when this platform has
// no sandbox or it cannot be applied.
func New(ctx context.Context, policies ...Policy) ([]*Sandbox, error) {
	switch runtime.GOOS {
	case "darwin":
		if err := checkAbsolute(policies); err != nil {
			return nil, err
		}
		if err := probe(ctx); err != nil {
			return nil, err
		}
		sandboxes := make([]*Sandbox, len(policies))
		for i, p := range policies {
			sandboxes[i] = &Sandbox{prefix: seatbeltPrefix(p)}
		}
		return sandboxes, nil
	case "linux":
		if err := checkAbsolute(policies); err != nil {
			return nil, err
		}
		if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
			return nil, fmt.Errorf("%w on %s/%s", ErrUnavailable, runtime.GOOS, runtime.GOARCH)
		}
		path, err := exec.LookPath("bwrap")
		if err == nil {
			path, err = filepath.Abs(path)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: failed to find bwrap: %w", ErrUnavailable, err)
		}
		filter, err := seccompFilter()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if err := probeBwrap(ctx, path, filter); err != nil {
			return nil, err
		}
		// One record for all: the policies mount under the same directories,
		// and their commands run side by side.
		shared := &placeholders{}
		sandboxes := make([]*Sandbox, len(policies))
		for i, p := range policies {
			sandboxes[i] = &Sandbox{bwrap: &bwrap{path: path, policy: p, shared: shared, seccomp: filter}}
		}
		return sandboxes, nil
	default:
		return nil, fmt.Errorf("%w on %s", ErrUnavailable, runtime.GOOS)
	}
}

func checkAbsolute(policies []Policy) error {
	for _, p := range policies {
		for _, list := range [][]string{p.Write, p.DenyRead, p.DenyWrite} {
			for _, path := range list {
				if !filepath.IsAbs(path) {
					return fmt.Errorf("sandbox path %q is not absolute", path)
				}
			}
		}
	}
	return nil
}

// Command returns the command that runs argv inside the sandbox. The caller
// must call done exactly once, after the command has been reaped or when it
// never started.
func (s *Sandbox) Command(argv []string) (cmd *exec.Cmd, done func(), err error) {
	if s.bwrap != nil {
		return s.bwrap.command(argv)
	}
	full := slices.Concat(s.prefix, argv)
	return exec.Command(full[0], full[1:]...), func() {}, nil
}
