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
	// Workspace is the absolute workspace path.
	Workspace string
	Busy      bool
	// Shell reports that a tool that runs commands is available, and
	// Sandboxed that it runs them under the OS sandbox.
	Shell, Sandboxed bool
	// Shells and Agents count the running background shell commands and
	// subagents; Foreground reports that a command runs in the foreground,
	// which Background can move.
	Shells, Agents int
	Foreground     bool
}
