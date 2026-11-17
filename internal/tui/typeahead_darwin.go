package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

// discardInput drops what f's terminal has received but not yet been read.
func discardInput(f *os.File) error {
	// TIOCFLUSH takes a pointer to its flag, FREAD, which is TCIFLUSH here.
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, unix.TCIFLUSH)
}
