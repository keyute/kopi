package headless

import "errors"

var (
	ErrFailed      = errors.New("the turn failed")
	ErrInterrupted = errors.New("interrupted")
)
