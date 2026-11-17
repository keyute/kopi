//go:build !darwin && !linux

package tui

import "os"

// discardInput does nothing here. On Windows Bubble Tea flushes the console
// input buffer itself when it starts reading.
func discardInput(*os.File) error { return nil }
