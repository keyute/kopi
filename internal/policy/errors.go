package policy

import "errors"

var (
	ErrChangedCall = errors.New("approved call changed")
	ErrUnknownTool = errors.New("unknown tool")
	ErrDenied      = errors.New("denied by policy")
	ErrRevoked     = errors.New("authority revoked")
	ErrNoGrant     = errors.New("no grant for call")
	ErrRejected    = errors.New("rejected by the user")
	ErrInvalidRule = errors.New("invalid permission rule")
)
