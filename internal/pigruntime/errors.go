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
	ErrNoTurn          = errors.New("no turn in progress")
	ErrNoAnswer        = errors.New("model returned no text")
	ErrNoSearch        = errors.New("model has no web search")
	ErrUnknownEffort   = errors.New("unknown effort")
	ErrParentResume    = errors.New("a session with a parent cannot resume or continue")
	ErrScript          = errors.New("script failed")
	ErrNotSaved        = errors.New("session has no saved transcript")

	ErrServerStartTimeout = errors.New("MCP server did not start in time")
	ErrServerNameClash    = errors.New("MCP server names give the same tool names")
	ErrServerClosed       = errors.New("MCP server is closed")
	ErrToolError          = errors.New("MCP tool returned an error")
)
