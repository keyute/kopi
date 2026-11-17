package app

import "github.com/keyute/kopi/internal/policy"

type Status struct {
	SessionID    string
	Mode         policy.Mode
	Model        string
	MessageCount int
	// ContextTokens and ContextWindow are zero while the usage is unknown.
	ContextTokens, ContextWindow int
	// Branch is empty when the workspace is not in a git repository.
	Branch string
	Busy   bool
	// Shell reports that a tool that runs commands is available, and
	// Sandboxed that it runs them under the OS sandbox.
	Shell, Sandboxed bool
}
