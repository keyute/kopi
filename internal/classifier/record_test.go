package classifier

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

func TestRecordTask(t *testing.T) {
	var r Record
	r.Observe(event.TurnStart{Prompt: "first"})
	r.Observe(event.TurnStart{})
	r.Observe(event.UserMessage{Text: "second"})
	assert.Equal(t, "first\n\nsecond", r.Conversation("").Task)
}

func TestRecordTaskKeepsNewestOnRuneBoundary(t *testing.T) {
	var r Record
	r.Observe(event.TurnStart{Prompt: strings.Repeat("é", 3000)})
	r.Observe(event.TurnStart{Prompt: strings.Repeat("é", 3000)})
	task := r.Conversation("").Task
	assert.LessOrEqual(t, len(task), maxTask)
	assert.Greater(t, len(task), maxTask-4)
	assert.True(t, utf8.ValidString(task))
	assert.True(t, strings.HasSuffix(task, "é"))
	assert.LessOrEqual(t, len(r.task), maxTask)
}

func TestRecordCallWindow(t *testing.T) {
	var r Record
	for i := range 25 {
		r.Observe(event.ToolStart{CallID: fmt.Sprint("c", i), Name: "Read"})
	}
	conv := r.Conversation("c24")
	require.Len(t, conv.History, historySize)
	assert.Len(t, r.calls, keptCalls)
	assert.Equal(t, "c4", r.calls[0].id)
	assert.Equal(t, r.calls[len(r.calls)-2].Call, conv.History[len(conv.History)-1])
}

func TestRecordExcludesCall(t *testing.T) {
	var r Record
	r.Observe(event.ToolStart{CallID: "a", Name: "Read", Args: `{"x":1}`})
	r.Observe(event.ToolStart{CallID: "b", Name: "Bash"})
	conv := r.Conversation("b")
	assert.Equal(t, []Call{{Tool: "Read", Args: `{"x":1}`}}, conv.History)
}

func TestRecordCutsArgsOnRuneBoundary(t *testing.T) {
	var r Record
	r.Observe(event.ToolStart{CallID: "a", Name: "Write", Args: "x" + strings.Repeat("é", 2000)})
	args := r.Conversation("").History[0].Args
	assert.Equal(t, 1999, len(args))
	assert.True(t, utf8.ValidString(args))
}

func TestRecordToolEndMarksFailed(t *testing.T) {
	var r Record
	r.Observe(event.ToolStart{CallID: "a", Name: "Read"})
	r.Observe(event.ToolStart{CallID: "b", Name: "Bash"})
	r.Observe(event.ToolEnd{CallID: "a", IsError: true})
	r.Observe(event.ToolEnd{CallID: "b"})
	conv := r.Conversation("")
	assert.True(t, conv.History[0].Failed)
	assert.False(t, conv.History[1].Failed)
}

func TestRecordNil(t *testing.T) {
	var r *Record
	assert.NotPanics(t, func() {
		r.Observe(event.TurnStart{Prompt: "x"})
		assert.Equal(t, Conversation{}, r.Conversation(""))
	})
}
