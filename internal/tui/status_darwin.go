package tui

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func awaitStatusExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("failed to create status kqueue: %w", err)
	}
	defer unix.Close(kq)
	ev := make([]unix.Kevent_t, 1)
	unix.SetKevent(&ev[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev[0].Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, ev, nil, nil); err != nil {
		// An unreaped child cannot disappear except by already exiting.
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("failed to watch status command: %w", err)
	}
	for {
		_, err := unix.Kevent(kq, nil, ev, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to wait for status command: %w", err)
		}
		return nil
	}
}
