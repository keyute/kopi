package event

import (
	"time"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type Event interface{ isEvent() }

// TurnStart precedes every other event of the turn. Prompt is the text sent
// to the model. A Work may run several turns in sequence sharing WorkID.
type TurnStart struct{ WorkID, Prompt string }

// Input is one human message: Text refers to each image as [image #N];
// Images are the files' paths in N order.
type Input struct {
	Text   string
	Images []string
}

// QueueChanged carries the messages queued for the running turn, in order.
type QueueChanged struct{ Pending []string }

// UserMessage is a queued message at the point the model reads it.
type UserMessage struct{ Text string }

type TextDelta struct{ Text string }

type ThinkingDelta struct{ Text string }

// ToolStart carries the call's JSON arguments, compacted. Title names the call
// and Done names it once it succeeded, "" when Title serves. Change is the
// line a file change shows under the row and Preview its removed and added
// lines. Solo marks a row that records a human decision, and Unsandboxed a
// call that opts out of the OS sandbox. Command is the command when Title does
// not show all of it.
type ToolStart struct {
	CallID, Name, Args string
	Title, Done        string
	Command            string
	Action             tool.Action
	Change             string
	Preview            []tool.PreviewLine
	Solo, Unsandboxed  bool
}

// EndKind tells how a call ended; the zero value is that it ran.
type EndKind int

const (
	// EndRan is a call that ran: it succeeded, or failed when IsError is set.
	EndRan EndKind = iota
	// EndDenied is a call the policy refused.
	EndDenied
	// EndRejected is a call the human rejected.
	EndRejected
	// EndInterrupted is a call cancelled before it finished.
	EndInterrupted
)

// ToolEnd's Output is the raw result text. Summary is the line under the call,
// set for a call that did not succeed too. Body is what an opened row shows:
// the result of a successful call, the detail of one that did not.
type ToolEnd struct {
	CallID, Name, Output string
	IsError              bool
	Kind                 EndKind
	Summary, Body        string
}

// ApprovalRequested asks the human to approve one call; answer with
// Controller.Answer. It is not ordered relative to the call's ToolStart, so it
// carries everything a prompt shows.
type ApprovalRequested struct {
	// Token identifies this approval for the controller's lifetime, across sessions.
	Token         uint64
	CallID, Title string
	Preview       []tool.PreviewLine
	// Rule is the ask rule and its source; "" when no rule asked.
	Rule string
	// Review is the classifier's verdict when it sent the call to the human;
	// nil when it did not review.
	Review *policy.Review
	// Edits reports that "allow all edits this session" may be offered.
	Edits bool
	// Agent names the subagent whose call this is, and TaskID is that call's
	// background task; both are "" for the root session.
	Agent, TaskID string
}

// ApprovalEnded follows every ApprovalRequested, whether it was answered or
// cancelled.
type ApprovalEnded struct{ Token uint64 }

// QuestionRequested asks the human the questions of one AskUserQuestion call;
// answer with Controller.AnswerQuestion.
type QuestionRequested struct {
	Token     uint64
	Questions []tool.Question
}

// QuestionEnded follows every QuestionRequested: answered, dismissed or cancelled.
type QuestionEnded struct{ Token uint64 }

// PlanRequested asks the human to approve the plan of one ExitPlanMode call;
// answer with Controller.AnswerPlan. It repeats with the same Token and the new
// Plan when the plan file was read again, after the human edited it.
type PlanRequested struct {
	Token      uint64
	Path, Plan string
}

// PlanEnded follows every PlanRequested: answered, dismissed or cancelled.
type PlanEnded struct{ Token uint64 }

// LoginRequested asks the human one question of a running login; answer with
// Controller.AnswerLogin. There may be no session yet.
type LoginRequested struct {
	// Token identifies this question for the controller's lifetime.
	Token             uint64
	Provider, Message string
	Placeholder       string
	// Secret asks for input that must not be echoed or kept.
	Secret bool
	// Options, when set, are the only valid answers; the answer is an option ID.
	Options []LoginOption
}

type LoginOption struct{ ID, Label, Description string }

// LoginEnded follows every LoginRequested, whether it was answered or
// cancelled.
type LoginEnded struct{ Token uint64 }

// Notice reports retry and compaction progress, the MCP server tools the
// first session skipped, and a login's progress and result.
type Notice struct{ Text string }

// Activity is what the Controller is doing outside a turn, such as waiting
// for the MCP servers; an empty Text means it has finished.
type Activity struct{ Text string }

type TurnEnd struct {
	WorkID  string
	Outcome Outcome
	// Elapsed is the turn's wall time.
	Elapsed time.Duration
	// Unsent is the queued input handed back when the turn ends without
	// continuing.
	Unsent []Input
}

// TaskEnded is a background task's end, at the point the model reads about it.
// Agent names the subagent that ran; it is "" for a shell command, whose
// Command is the command. For an agent, Command is the call's description.
// For a workflow, Agents counts the agent() calls, Failed those that ended
// without completing (stopped ones included), Reason is why a failed run
// failed and Result the value it returned; all are zero for any other task.
type TaskEnded struct {
	ID, Command, Status string
	Agent               string
	Elapsed             time.Duration
	Agents, Failed      int
	Reason, Result      string
}

// TasksChanged reports that the set or state of background tasks changed;
// frontends re-read their status.
type TasksChanged struct{}

// LimitsChanged reports a new reading of the plan limits; frontends re-read
// their status.
type LimitsChanged struct{}

type ModeChanged struct{ Mode policy.Mode }

type SessionChanged struct {
	SessionID    string
	Resumed      bool
	MessageCount int
	// History replays a resumed session's stored conversation, in order;
	// nil for a new session.
	History []Event
}

// StatusCommandResult is ephemeral footer output, not session history.
type StatusCommandResult struct {
	Name, Output string
	Err          error
}

// Changes is ephemeral footer data, not session history: the lines changed
// against HEAD.
type Changes struct{ Added, Removed int }

type Error struct{ Err error }

// Session carries an event of a session other than the main one; ID is that
// session's id.
type Session struct {
	ID    string
	Event Event
}

// SessionClosed is the last event of the non-main session ID, which closed
// while the main session stays open. It is not wrapped in Session.
type SessionClosed struct{ ID string }

func (TurnStart) isEvent()           {}
func (QueueChanged) isEvent()        {}
func (UserMessage) isEvent()         {}
func (TextDelta) isEvent()           {}
func (ThinkingDelta) isEvent()       {}
func (ToolStart) isEvent()           {}
func (ToolEnd) isEvent()             {}
func (ApprovalRequested) isEvent()   {}
func (ApprovalEnded) isEvent()       {}
func (QuestionRequested) isEvent()   {}
func (QuestionEnded) isEvent()       {}
func (PlanRequested) isEvent()       {}
func (PlanEnded) isEvent()           {}
func (LoginRequested) isEvent()      {}
func (LoginEnded) isEvent()          {}
func (Notice) isEvent()              {}
func (Activity) isEvent()            {}
func (TurnEnd) isEvent()             {}
func (TaskEnded) isEvent()           {}
func (TasksChanged) isEvent()        {}
func (LimitsChanged) isEvent()       {}
func (ModeChanged) isEvent()         {}
func (SessionChanged) isEvent()      {}
func (StatusCommandResult) isEvent() {}
func (Changes) isEvent()             {}
func (Error) isEvent()               {}
func (Session) isEvent()             {}
func (SessionClosed) isEvent()       {}

type Status int

const (
	Completed Status = iota
	Interrupted
	Failed
)

func (s Status) String() string {
	switch s {
	case Completed:
		return "completed"
	case Interrupted:
		return "interrupted"
	case Failed:
		return "failed"
	}
	return "unknown"
}

type Outcome struct {
	Status Status
	// Message is the provider or tool error text of a Failed turn.
	Message string
}
