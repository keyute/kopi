package sandbox

import "errors"

var (
	ErrUnavailable = errors.New("sandbox unavailable")
	// ErrDeniedSymlink is a denied path crossing a mutable symlink or an
	// unresolved symlink target inside a write root, which cannot be masked safely.
	ErrDeniedSymlink = errors.New("denied path crosses an unsafe symlink")
	// ErrInvalidDomain is a network allow or deny entry that is not a host name
	// pattern.
	ErrInvalidDomain = errors.New("invalid domain entry")
)
