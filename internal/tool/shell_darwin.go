package tool

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func awaitExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("failed to create kqueue: %w", err)
	}
	defer unix.Close(kq)
	ev := make([]unix.Kevent_t, 1)
	unix.SetKevent(&ev[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev[0].Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, ev, nil, nil); err != nil {
		// The child is unreaped, so ESRCH means it has already exited.
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("failed to watch process exit: %w", err)
	}
	for {
		_, err := unix.Kevent(kq, nil, ev, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to wait for process exit: %w", err)
		}
		return nil
	}
}
