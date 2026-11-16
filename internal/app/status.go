package app

import (
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

type Status struct {
	SessionID        string
	Mode             policy.Mode
	Model, Workspace string
	MessageCount     int
	// ContextTokens and ContextWindow are zero while the usage is unknown.
	ContextTokens, ContextWindow int
	// Branch is empty when Workspace is not in a git repository.
	Branch string
	Busy   bool
	Tools  []ToolStatus
}

type ToolStatus struct {
	Name       string
	Capability policy.Capability
	Allowed    bool
}

const approvalNote = "A call needs approval when a permission rule asks, or when no rule decides a call by a tool other than Read."

func renderStatus(s Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode: %s\n", s.Mode)
	fmt.Fprintf(&b, "model: %s\n", s.Model)
	fmt.Fprintf(&b, "workspace: %s\n", s.Workspace)
	if s.SessionID == "" {
		b.WriteString("session: none\n")
	} else {
		fmt.Fprintf(&b, "session: %s (%d messages)\n", s.SessionID, s.MessageCount)
	}
	if s.Busy {
		b.WriteString("turn: running\n")
	} else {
		b.WriteString("turn: idle\n")
	}
	b.WriteString("tools:\n")
	for _, t := range s.Tools {
		allowed := "allowed"
		if !t.Allowed {
			allowed = fmt.Sprintf("not allowed in %s mode", s.Mode)
		}
		fmt.Fprintf(&b, "  %s (%s): %s\n", t.Name, t.Capability, allowed)
	}
	b.WriteString(approvalNote)
	return b.String()
}
