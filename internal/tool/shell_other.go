//go:build !unix

package tool

import "os/exec"

// newSession leaves the shell in Kopi's own process group.
func newSession(*exec.Cmd) {}

// kill stops only the shell itself.
func kill(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}

// watchExit reaps the shell as soon as it exits; reap returns that result.
func watchExit(cmd *exec.Cmd) (exited <-chan error, reap func() error) {
	ch := make(chan error, 1)
	var err error
	go func() {
		err = cmd.Wait()
		ch <- nil
	}()
	return ch, func() error { return err }
}
