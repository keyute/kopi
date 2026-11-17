package trust

import "errors"

var (
	ErrUnsupportedVersion = errors.New("unsupported trust file version")
	ErrInvalidDecision    = errors.New("invalid trust decision")
)
