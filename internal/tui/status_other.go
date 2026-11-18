//go:build !unix

package tui

import (
	"context"
	"os/exec"
	"time"
)

func (shellRunner) run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}
