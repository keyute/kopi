//go:build !unix

package app

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// prefixWriter keeps the first statusOutputLimit bytes and reports every write as done, so the child never blocks.
type prefixWriter struct{ strings.Builder }

func (w *prefixWriter) Write(p []byte) (int, error) {
	if n := statusOutputLimit - w.Len(); n > 0 {
		w.Builder.Write(p[:min(n, len(p))])
	}
	return len(p), nil
}

func (shellRunner) run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	var out prefixWriter
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}
