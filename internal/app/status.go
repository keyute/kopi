package app

import (
	"time"

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
	// Shells, Agents and Workflows count the running background shell commands,
	// subagents and workflows; Foreground reports that a command runs in the
	// foreground, which Background can move.
	Shells, Agents, Workflows int
	Foreground                bool
	// TotalCost is the current session's whole estimated spend in dollars: the
	// main session, its subagents, the workflow agents that ended, and the
	// classifier, WebFetch and WebSearch requests.
	TotalCost float64
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

// WorkflowQueued is a workflow agent's state before a slot is free for it.
const WorkflowQueued tool.TaskStatus = "queued"

// WorkflowStatus describes one workflow run of the current session.
type WorkflowStatus struct {
	ID, Description string
	State           tool.TaskStatus
	Started, Ended  time.Time
	// Failed counts every ended call that did not complete, stopped ones included.
	Done, Failed, Running, Queued int
	// Phase is the phase of the latest running call that has one.
	Phase string
}

// WorkflowAgent is one agent() call of a workflow run.
type WorkflowAgent struct {
	// ID is the agent's session id, "" until it opens; Agent is the
	// definition's name, Description the call's label and State queued,
	// running, completed, failed or stopped.
	AgentStatus
	// N is the call's number in the script.
	N              int
	Phase          string
	Started, Ended time.Time // Started is zero while queued
	// Reason is why a failed call failed.
	Reason string
}
