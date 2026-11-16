//go:build unix

package tool

import (
	"os/exec"
	"syscall"
)

// killGroup puts the shell in a new session so cancelling kills everything it
// started. A session rather than a process group: a child in a background
// group that touched /dev/tty would stop or corrupt the fullscreen UI, while
// one in its own session has no controlling terminal.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
