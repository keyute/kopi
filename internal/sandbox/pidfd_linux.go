package sandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// initPidfd reads the info JSON bwrap writes to r and returns a pidfd for the
// child it reports, or -1 when there is nothing to wait for: bwrap wrote no
// report, the child is not the namespace's PID 1, or it has already exited.
func initPidfd(r io.Reader) int {
	var info struct {
		ChildPID int `json:"child-pid"`
	}
	if json.NewDecoder(r).Decode(&info) != nil {
		return -1
	}
	fd, err := unix.PidfdOpen(info.ChildPID, 0)
	if err != nil {
		return -1
	}
	// The status is read by number, so the pidfd must still name a live
	// process afterwards: the number was then never reused.
	status, err := os.ReadFile("/proc/" + strconv.Itoa(info.ChildPID) + "/status")
	if err == nil && unix.PidfdSendSignal(fd, 0, nil, 0) == nil && lastNSpid(status) == "1" {
		return fd
	}
	_ = unix.Close(fd)
	return -1
}

// lastNSpid is the process's id in its innermost PID namespace.
func lastNSpid(status []byte) string {
	for line := range bytes.Lines(status) {
		if rest, ok := bytes.CutPrefix(line, []byte("NSpid:")); ok {
			fields := bytes.Fields(rest)
			if len(fields) > 0 {
				return string(fields[len(fields)-1])
			}
		}
	}
	return ""
}

// waitInit returns once the process behind fd has exited, and closes fd. The
// namespace's PID 1 exits only after the kernel has killed the rest of it.
func waitInit(fd int) {
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err != unix.EINTR {
			return
		}
	}
}
