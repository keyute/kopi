package app

import (
	"time"

	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/quota"
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
	// subagents and workflows; ForegroundSince is when the earliest command running
	// in the foreground began, zero when none, and Background can move it.
	Shells, Agents, Workflows int
	ForegroundSince           time.Time
	// TotalCost is the current session's whole estimated spend in dollars: the
	// main session, its subagents, the workflow agents that ended, and the
	// classifier, WebFetch and WebSearch requests.
	TotalCost float64
	// Limits is the last reading of the main model's provider's plan limits;
	// zero until one is read, and kept while a later read fails.
	Limits quota.Limits
	// Ultracode reports that the main session's Workflow tool is callable.
	Ultracode bool
}

type (
	Tokens     = pigruntime.Tokens
	ModelUsage = pigruntime.ModelUsage
	Cache      = pigruntime.Cache
)

// Usage is what the /usage view shows of the current session.
type Usage struct {
	SessionID           string
	Messages, ToolCalls int // the main session's, over its whole log
	Model               string
	// ContextTokens and ContextWindow are the main session's; zero while unknown.
	ContextTokens, ContextWindow int
	// Context is ContextTokens by category; zero while that is unknown.
	Context Context
	// CompactAt is the context size above which the main session compacts, zero
	// while its window is unknown; Compactions is how many its log holds.
	CompactAt, Compactions int
	Limits                 quota.Limits
	Tokens                 Tokens
	// Models are by cost descending, then model; their costs sum to
	// Status.TotalCost.
	Models []ModelUsage
	Cache  Cache // the main session's
	// ModelTime and ToolTime are what the main session's requests and tool runs
	// took, over its whole log.
	ModelTime, ToolTime time.Duration
}

// Context is a session's context by category, in tokens. Messages is what the
// others leave of the reported total; the others are estimates.
type Context struct{ SystemPrompt, Tools, Instructions, Skills, Agents, MCP, Messages int }

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
