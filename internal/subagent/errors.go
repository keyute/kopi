package subagent

import "errors"

var (
	ErrInvalidDefinition = errors.New("invalid subagent definition")
	ErrDuplicateName     = errors.New("duplicate subagent name")
)
