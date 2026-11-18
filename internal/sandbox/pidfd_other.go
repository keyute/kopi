//go:build !linux

package sandbox

import "io"

func initPidfd(io.Reader) int { return -1 }

func waitInit(int) {}
