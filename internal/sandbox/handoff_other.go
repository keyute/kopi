//go:build !linux

package sandbox

import (
	"fmt"
	"os"
	"runtime"
)

// ExecListening is Linux only.
func ExecListening([]string) error {
	return fmt.Errorf("failed to listen in the sandbox: not supported on %s", runtime.GOOS)
}

type handoff struct {
	child *os.File
}

func (p *Proxy) handoff() (*handoff, error) {
	return nil, fmt.Errorf("failed to hand off the sandbox listener: not supported on %s", runtime.GOOS)
}

func (h *handoff) close() []string { return nil }
