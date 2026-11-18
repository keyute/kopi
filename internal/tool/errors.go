package tool

import "errors"

var (
	ErrInvalidMCPServerName = errors.New("invalid MCP server name")
	ErrEditConflict         = errors.New("file changed during edit")
	ErrTargetChanged        = errors.New("workspace traversal changed")
	ErrOutsideWorkspace     = errors.New("path is outside the workspace")
	ErrProtected            = errors.New("path is protected")
	ErrBinary               = errors.New("file is binary")
	ErrIsDirectory          = errors.New("path is a directory")
	ErrNotRegular           = errors.New("path is not a regular file")
	ErrInvalidArgs          = errors.New("invalid arguments")
	ErrTooLarge             = errors.New("file is too large")
	ErrNoMatch              = errors.New("old_string was not found")
	ErrAmbiguousMatch       = errors.New("old_string matches more than once")
	ErrTimedOut             = errors.New("command timed out")
	ErrExitStatus           = errors.New("command failed")
	ErrStopped              = errors.New("command stopped")
	ErrUnknownTask          = errors.New("unknown task")
)
