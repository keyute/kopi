package app

import "errors"

var (
	ErrQuit             = errors.New("quit requested")
	ErrShowTasks        = errors.New("task list requested")
	ErrBusy             = errors.New("a session switch is under way")
	ErrBadImage         = errors.New("not a supported image")
	ErrUnknownCommand   = errors.New("unknown command")
	ErrNoSession        = errors.New("no open session")
	ErrWorkStillRunning = errors.New("work is still running")
	ErrMissingArgument  = errors.New("missing argument")
	ErrLoggingIn        = errors.New("a login is under way")
)
