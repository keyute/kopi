package app

import (
	"errors"
	"fmt"

	"github.com/keyute/kopi/internal/tool"
)

var (
	ErrQuit               = errors.New("quit requested")
	ErrShowTasks          = errors.New("task list requested")
	ErrShowUsage          = errors.New("usage requested")
	ErrCopy               = errors.New("copy requested")
	ErrBusy               = errors.New("a session switch or model change is under way")
	ErrBadImage           = errors.New("not a supported image")
	ErrUnknownCommand     = errors.New("unknown command")
	ErrNoSession          = errors.New("no open session")
	ErrWorkStillRunning   = errors.New("work is still running")
	ErrOpenPlan           = errors.New("plan file requested")
	ErrUnknownArgument    = errors.New("unknown argument")
	ErrMissingArgument    = errors.New("missing argument")
	ErrStartupInterrupted = errors.New("failed to start MCP servers: startup was interrupted")
	ErrLoggingIn          = errors.New("a login is under way")
	ErrNotAdded           = errors.New("directory was not added to the workspace")
	ErrTooManyAgents      = fmt.Errorf("%d subagents are already running", maxAgents)

	// ErrTooManyWorkflows is wrapped with the configured limit as "%d workflows are already running".
	ErrTooManyWorkflows = errors.New("workflows are already running")

	ErrTooManyWorkflowCalls   = fmt.Errorf("a workflow may make at most %d agent() calls", tool.WorkflowCalls)
	ErrAgentStopped           = errors.New("the agent was stopped")
	ErrAgentRunning           = errors.New("the agent has not ended")
	ErrNoStructuredOutput     = errors.New("the agent ended without calling StructuredOutput")
	ErrUnknownAgentType       = errors.New("unknown agent type")
	ErrStructuredOutputDenied = errors.New("the StructuredOutput tool is denied by a rule")
	ErrBadSchema              = errors.New("schema must be a JSON Schema of type object")
)
