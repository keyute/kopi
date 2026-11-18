//go:build unix

package tool

import (
	"os/exec"
	"syscall"

	childprocess "github.com/keyute/kopi/internal/process"
)

// newSession puts the shell in a new session so a kill reaches everything it
// started. A session rather than a process group: a child in a background
// group that touched /dev/tty would stop or corrupt the fullscreen UI, while
// one in its own session has no controlling terminal.
func newSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// kill must run before the shell is reaped, while its PID still names the
// group.
func kill(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// watchExit reports the shell's exit without reaping it; reap then collects
// its status. childprocess.AwaitExit exists only for Linux and macOS, so other Unix
// systems deliberately fail to build rather than lose group kills.
func watchExit(cmd *exec.Cmd) (exited <-chan error, reap func() error) {
	ch := make(chan error, 1)
	pid := cmd.Process.Pid
	go func() { ch <- childprocess.AwaitExit(pid) }()
	return ch, cmd.Wait
}
