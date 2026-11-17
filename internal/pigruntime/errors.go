package pigruntime

import "errors"

var (
	ErrUnknownModel    = errors.New("unknown model")
	ErrJoinTimeout     = errors.New("session run did not stop in time")
	ErrNoCredential    = errors.New("no credential")
	ErrUnknownProvider = errors.New("unknown provider")
	ErrNoOAuth         = errors.New("provider does not offer OAuth login")
	ErrSessionNotFound = errors.New("session not found")
	ErrSendInProgress  = errors.New("session send already in progress")
	ErrNoTurn          = errors.New("no turn in progress")

	ErrServerStartTimeout = errors.New("MCP server did not start in time")
	ErrServerNameClash    = errors.New("MCP server names give the same tool names")
	ErrServerClosed       = errors.New("MCP server is closed")
	ErrToolError          = errors.New("MCP tool returned an error")
)
