//go:build unix

package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func (shellRunner) run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	r, w, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("failed to create status output pipe: %w", err)
	}
	defer r.Close()
	if err := ctx.Err(); err != nil {
		_ = w.Close()
		return "", err
	}
	cmd.Stdout = w
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		return "", fmt.Errorf("failed to run status command: %w", err)
	}
	var out bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&out, r); close(drained) }()
	exited := make(chan error, 1)
	go func() { exited <- awaitStatusExit(cmd.Process.Pid) }()
	// Keep the leader unreaped until every group kill is finished: its PID
	// pins the process group ID against reuse, even after the shell exits.
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var reason error
	select {
	case err = <-exited:
	case <-ctx.Done():
		reason = ctx.Err()
		kill()
		err = <-exited
	}
	if err != nil {
		reason = err
		kill()
	} else {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-drained:
		case <-ctx.Done():
			reason = ctx.Err()
		case <-timer.C:
			reason = exec.ErrWaitDelay
		}
	}
	kill()
	_ = r.Close()
	<-drained
	waitErr := cmd.Wait()
	if reason != nil {
		return "", reason
	}
	return out.String(), waitErr
}
