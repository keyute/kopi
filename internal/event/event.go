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

// ToolStart carries the call's JSON arguments, compacted.
type ToolStart struct {
	CallID, Name, Args string
	Title              string
	Action             tool.Action
}

// ToolEnd's Summary is empty for a failed call.
type ToolEnd struct {
	CallID, Name, Output string
	IsError              bool
	Summary              string
}

// ApprovalRequested asks the human to approve one call; answer with
// Controller.Answer. It is not ordered relative to the call's ToolStart, so it
// carries everything a prompt shows.
type ApprovalRequested struct {
	CallID, Title string
	Preview       []tool.PreviewLine
	// Rule is the ask rule and its source; "" when no rule asked.
	Rule string
	// Edits reports that "allow all edits this session" may be offered.
	Edits bool
}

// ApprovalEnded follows every ApprovalRequested, whether it was answered or
// cancelled.
type ApprovalEnded struct{ CallID string }

// Notice reports retry and compaction progress.
type Notice struct{ Text string }

type TurnEnd struct {
	WorkID  string
	Outcome Outcome
	// Elapsed is the turn's wall time.
	Elapsed time.Duration
	// Unsent is the queued input handed back when the turn ends without
	// continuing.
	Unsent []Input
}

type ModeChanged struct{ Mode policy.Mode }

type SessionChanged struct {
	SessionID    string
	Resumed      bool
	MessageCount int
	// History replays a resumed session's stored conversation, in order;
	// nil for a new session.
	History []Event
}

type Error struct{ Err error }

func (TurnStart) isEvent()         {}
func (QueueChanged) isEvent()      {}
func (UserMessage) isEvent()       {}
func (TextDelta) isEvent()         {}
func (ThinkingDelta) isEvent()     {}
func (ToolStart) isEvent()         {}
func (ToolEnd) isEvent()           {}
func (ApprovalRequested) isEvent() {}
func (ApprovalEnded) isEvent()     {}
func (Notice) isEvent()            {}
func (TurnEnd) isEvent()           {}
func (ModeChanged) isEvent()       {}
func (SessionChanged) isEvent()    {}
func (Error) isEvent()             {}

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
