package tui

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func awaitStatusExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to wait for status command: %w", err)
		}
		return nil
	}
}
