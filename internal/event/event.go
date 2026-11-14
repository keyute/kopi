package event

import (
	"time"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type Event interface{ isEvent() }

// TurnStart precedes every other event of the turn. Prompt is the text sent
// to the model.
type TurnStart struct{ WorkID, Prompt string }

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

// Notice reports retry and compaction progress.
type Notice struct{ Text string }

type TurnEnd struct {
	WorkID  string
	Outcome Outcome
	// Elapsed is the turn's wall time.
	Elapsed time.Duration
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

func (TurnStart) isEvent()      {}
func (TextDelta) isEvent()      {}
func (ThinkingDelta) isEvent()  {}
func (ToolStart) isEvent()      {}
func (ToolEnd) isEvent()        {}
func (Notice) isEvent()         {}
func (TurnEnd) isEvent()        {}
func (ModeChanged) isEvent()    {}
func (SessionChanged) isEvent() {}
func (Error) isEvent()          {}

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
