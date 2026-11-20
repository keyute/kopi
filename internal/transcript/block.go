package transcript

import (
	"strconv"
	"time"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

type State int

const (
	Running State = iota
	OK
	Failed
	// Unfinished is a call whose turn ended before it settled; it never will.
	Unfinished
	// Denied is a call the policy refused, Rejected one the human rejected.
	Denied
	Rejected
)

// Call is one tool call. Title is the call's name until it succeeds, then its
// done form; Preview is a successful file change's lines.
type Call struct {
	ID, Title, Summary, Body string
	Action                   tool.Action
	State                    State
	Preview                  []tool.PreviewLine
	// Unsandboxed is a call that opted out of the OS sandbox.
	Unsandboxed bool
	// solo is a call that records a human decision and never joins a group;
	// done and change come from the start event and are spent at its end.
	solo         bool
	done, change string
}

// Block is one drawable unit. Key is unique within the transcript and stable
// as it grows, so a frontend can cache per block.
type Block interface{ Key() string }

type User struct {
	key  string
	Text string
}

type Assistant struct {
	key  string
	Text string
}

type Row struct{ Call Call }

// Group folds two or more consecutive ok calls. Sealed is false only while
// nothing but running calls follows it.
type Group struct {
	key    string
	Calls  []Call
	Sealed bool
}

type Notice struct {
	key  string
	Text string
}

type Error struct {
	key  string
	Text string
}

type Turn struct {
	key     string
	Outcome event.Outcome
	Elapsed time.Duration
}

// Task is a background task's end; Status is completed, failed or stopped.
type Task struct {
	key                 string
	ID, Command, Status string
	// Agent is the subagent's name, "" for a shell command.
	Agent   string
	Elapsed time.Duration
}

// Plan is a plan presented for approval; a repeat of the same ExitPlanMode
// call replaces Text.
type Plan struct {
	key  string
	Text string
}

func (b User) Key() string      { return b.key }
func (b Assistant) Key() string { return b.key }
func (b Row) Key() string       { return "call:" + b.Call.ID }
func (b Group) Key() string     { return b.key }
func (b Notice) Key() string    { return b.key }
func (b Error) Key() string     { return b.key }
func (b Turn) Key() string      { return b.key }
func (b Task) Key() string      { return b.key }
func (b Plan) Key() string      { return b.key }

func (t *Transcript) Blocks() []Block {
	var (
		blocks  []Block
		stretch []Call
	)
	flush := func(historical bool) {
		blocks = appendStretch(blocks, stretch, historical)
		stretch = nil
	}
	for i, it := range t.items {
		if i == t.boundary {
			flush(true)
		}
		if !it.visible() {
			continue
		}
		if it.kind == callItem {
			stretch = append(stretch, it.call)
			continue
		}
		flush(i < t.boundary)
		key := "item:" + strconv.Itoa(i)
		switch it.kind {
		case userItem:
			blocks = append(blocks, User{key: key, Text: it.text})
		case assistantItem:
			blocks = append(blocks, Assistant{key: key, Text: it.text})
		case noticeItem:
			blocks = append(blocks, Notice{key: key, Text: it.text})
		case errorItem:
			blocks = append(blocks, Error{key: key, Text: it.text})
		case turnItem:
			blocks = append(blocks, Turn{key: key, Outcome: it.outcome, Elapsed: it.elapsed})
		case planItem:
			blocks = append(blocks, Plan{key: key, Text: it.text})
		case taskItem:
			blocks = append(blocks, Task{key: key, ID: it.task.ID, Command: it.task.Command, Agent: it.task.Agent, Status: it.task.Status, Elapsed: it.task.Elapsed})
		}
	}
	flush(len(t.items) <= t.boundary)

	followed := false
	for i := len(blocks) - 1; i >= 0; i-- {
		switch b := blocks[i].(type) {
		case Group:
			b.Sealed = b.Sealed || followed
			blocks[i] = b
			followed = true
		case Row:
			if b.Call.State != Running || b.Call.solo {
				followed = true
			}
		default:
			followed = true
		}
	}
	return blocks
}

// appendStretch splits the stretch at each call that did not succeed and each
// solo call; each segment's ok calls fold into a group, with its running calls
// below.
func appendStretch(blocks []Block, stretch []Call, historical bool) []Block {
	var segment []Call
	flush := func() {
		blocks = appendSegment(blocks, segment, historical)
		segment = nil
	}
	for _, c := range stretch {
		if c.solo || c.State != OK && c.State != Running {
			flush()
			blocks = append(blocks, Row{Call: c})
			continue
		}
		segment = append(segment, c)
	}
	flush()
	return blocks
}

func appendSegment(blocks []Block, segment []Call, historical bool) []Block {
	var ok, running []Call
	for _, c := range segment {
		if c.State == OK {
			ok = append(ok, c)
		} else {
			running = append(running, c)
		}
	}
	switch len(ok) {
	case 0:
	case 1:
		blocks = append(blocks, Row{Call: ok[0]})
	default:
		// Keyed by the segment's first call, not its first ok call, so the key
		// holds while an earlier running call joins.
		blocks = append(blocks, Group{key: "group:" + segment[0].ID, Calls: ok, Sealed: historical})
	}
	for _, c := range running {
		blocks = append(blocks, Row{Call: c})
	}
	return blocks
}
