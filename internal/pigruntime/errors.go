package pigruntime

import "errors"

var (
	ErrUnknownModel    = errors.New("unknown model")
	ErrJoinTimeout     = errors.New("session run did not stop in time")
	ErrNoCredential    = errors.New("no credential")
	ErrUnknownProvider = errors.New("unknown provider")
	ErrNoLogin         = errors.New("provider offers no login")
	ErrNotLoggedIn     = errors.New("not logged in")
	ErrSessionNotFound = errors.New("session not found")
	ErrSendInProgress  = errors.New("session send already in progress")
)
