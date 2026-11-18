package sandbox

import "errors"

var (
	ErrUnavailable = errors.New("sandbox unavailable")
	// ErrDeniedSymlink is a denied path that crosses a symlink inside a write
	// root, where a command could swap it to move the path from under its
	// mount.
	ErrDeniedSymlink = errors.New("denied path crosses a symlink inside a write root")
)
