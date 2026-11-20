// Package sandbox confines commands with the operating system's sandbox.
package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"

	"github.com/keyute/kopi/internal/policy"
)

// loopbackAttempts bounds how often listenLoopback picks a fresh port when the
// IPv6 side of the first is taken.
const loopbackAttempts = 5

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
	// Proxy, when set, is the only way commands reach the network. It
	// conflicts with Network.
	Proxy *Proxy
}

// Sandbox runs commands under one Policy.
type Sandbox struct {
	prefix []string // macOS: the sandbox-exec argv
	bwrap  *bwrap   // Linux
	proxy  *Proxy   // the policy's Proxy
}

// New checks once that the platform sandbox works and builds one Sandbox per
// policy, in argument order. It returns ErrUnavailable when this platform has
// no sandbox or it cannot be applied.
//
// dir is where the sandbox keeps what Kopi processes sharing it must agree on,
// such as when the placeholders a command mounts on the host may be removed.
// Commands must not be able to write it. Only Linux uses it.
func New(ctx context.Context, dir string, policies ...Policy) ([]*Sandbox, error) {
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
			sandboxes[i] = &Sandbox{prefix: seatbeltPrefix(p), proxy: p.Proxy}
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
		if err := checkHelper(policies); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create the sandbox directory %s: %w", dir, err)
		}
		// One record for all: the policies mount under the same directories,
		// and their commands run side by side.
		shared := newPlaceholders(dir)
		sandboxes := make([]*Sandbox, len(policies))
		for i, p := range policies {
			sandboxes[i] = &Sandbox{bwrap: &bwrap{path: path, policy: p, shared: shared, seccomp: filter}, proxy: p.Proxy}
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

// checkHelper fails for a proxy whose helper bwrap could not run: it runs in
// the sandbox, where a denied path is masked.
func checkHelper(policies []Policy) error {
	for _, p := range policies {
		if p.Proxy == nil {
			continue
		}
		helper := policy.Resolve(p.Proxy.helper[0])
		for _, d := range p.DenyRead {
			if denied := policy.Resolve(d); within(helper, denied) {
				return fmt.Errorf("sandbox proxy helper %s is under the denied path %s", helper, denied)
			}
		}
	}
	return nil
}

// FiltersNetwork reports whether commands reach the network only through the
// policy's proxy.
func (s *Sandbox) FiltersNetwork() bool { return s.proxy != nil }

// Command returns the command that runs argv inside the sandbox. When err is
// nil, the caller must call done exactly once, after the command has been
// reaped or if it never starts. done returns the destinations the policy's
// proxy refused for this command, nil without a proxy.
func (s *Sandbox) Command(argv []string) (cmd *exec.Cmd, done func() []string, err error) {
	if s.bwrap != nil {
		return s.bwrap.command(argv)
	}
	if s.proxy == nil {
		full := slices.Concat(s.prefix, argv)
		return exec.Command(full[0], full[1:]...), func() []string { return nil }, nil
	}
	l4, l6, err := listenLoopback()
	if err != nil {
		return nil, nil, err
	}
	port := l4.Addr().(*net.TCPAddr).Port
	session := s.proxy.serve(l4, l6)
	// The profile's PROXY parameter is the last one before the trailing "--".
	prefix := slices.Insert(slices.Clone(s.prefix), len(s.prefix)-1, "-DPROXY=localhost:"+strconv.Itoa(port))
	full := slices.Concat(prefix, argv)
	cmd = exec.Command(full[0], full[1:]...)
	cmd.Env = append(os.Environ(), proxyEnv(port)...)
	return cmd, session.close, nil
}

// listenLoopback listens on the same port on 127.0.0.1 and [::1]: Seatbelt's
// localhost:<port> rule allows both, so the proxy must hold both or a foreign
// service on the other address would be reachable.
func listenLoopback() (l4, l6 net.Listener, err error) {
	for range loopbackAttempts {
		l4, err = net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, nil, fmt.Errorf("failed to listen for the sandbox proxy: %w", err)
		}
		l6, err = net.Listen("tcp6", "[::1]:"+strconv.Itoa(l4.Addr().(*net.TCPAddr).Port))
		if err == nil {
			return l4, l6, nil
		}
		_ = l4.Close()
	}
	return nil, nil, fmt.Errorf("failed to listen for the sandbox proxy on [::1]: %w", err)
}
