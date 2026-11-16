//go:build !unix

package tool

import "os/exec"

// killGroup leaves exec's default of killing only the shell itself.
func killGroup(*exec.Cmd) {}
