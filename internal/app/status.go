package app

import (
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

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

// AgentStatus describes one session for a frontend: the main session or a subagent's.
type AgentStatus struct {
	ID          string // Main for the main session, else the task id
	Agent       string // the definition's name; "" for the main session
	Description string // what the subagent was started for; "" for the main session
	Model       string
	State       tool.TaskStatus // a subagent's task status; "" for the main session
	// ContextTokens and ContextWindow are zero while the usage is unknown.
	ContextTokens, ContextWindow int
	// Effort is empty when the model does not reason.
	Effort    string
	ToolCalls int
	// Cost is in dollars, estimated from the model's price.
	Cost float64
}
