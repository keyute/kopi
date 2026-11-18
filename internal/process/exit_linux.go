package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// AwaitExit observes an owned child without reaping it. The caller must keep
// the child unreaped until all PID-based signals have finished.
func AwaitExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to wait for process exit: %w", err)
		}
		return nil
	}
}
