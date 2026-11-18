package classifier

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/event"
)

const (
	maxTask     = 8000
	maxArgs     = 2000
	keptCalls   = historySize + 1
	historySize = 20
)

// Conversation is what a review is told about the session.
type Conversation struct {
	Task    string
	History []Call
}

// Call is a tool call as the review sees it.
type Call struct {
	Tool, Args string
	Failed     bool
}

type recorded struct {
	id string
	Call
}

// Record keeps a session's prompts and recent tool calls for its reviews.
type Record struct {
	mu    sync.Mutex
	task  string
	calls []recorded
}

// Observe takes in the events a review needs; the rest are ignored.
func (r *Record) Observe(ev event.Event) {
	if r == nil {
		return
	}
	switch e := ev.(type) {
	case event.TurnStart:
		if e.Prompt != "" {
			r.addTask(e.Prompt)
		}
	case event.UserMessage:
		r.addTask(e.Text)
	case event.ToolStart:
		r.addCall(recorded{id: e.CallID, Call: Call{Tool: e.Name, Args: cutHead(e.Args, maxArgs)}})
	case event.ToolEnd:
		r.markEnd(e.CallID, e.IsError)
	}
}

// Conversation returns the task and the last calls other than exclude, oldest
// first. exclude is the call under review: its start event precedes the gate.
func (r *Record) Conversation(exclude string) Conversation {
	if r == nil {
		return Conversation{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	conv := Conversation{Task: r.task}
	skip := -1
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].id == exclude {
			skip = i
			break
		}
	}
	for i, c := range r.calls {
		if i != skip {
			conv.History = append(conv.History, c.Call)
		}
	}
	if len(conv.History) > historySize {
		conv.History = conv.History[len(conv.History)-historySize:]
	}
	return conv
}

func (r *Record) addTask(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.task != "" {
		r.task += "\n\n"
	}
	r.task = cutTail(r.task+text, maxTask)
}

func (r *Record) addCall(c recorded) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
	if len(r.calls) > keptCalls {
		r.calls = append(r.calls[:0], r.calls[len(r.calls)-keptCalls:]...)
	}
}

func (r *Record) markEnd(id string, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].id == id {
			r.calls[i].Failed = failed
			return
		}
	}
}

// cutHead keeps the first n bytes of s, ending on a rune boundary.
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutTail keeps the last n bytes of s, starting on a rune boundary.
func cutTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return strings.Clone(s[i:])
}
