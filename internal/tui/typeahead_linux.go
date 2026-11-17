package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

// discardInput drops what f's terminal has received but not yet been read.
func discardInput(f *os.File) error {
	return unix.IoctlSetInt(int(f.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}
