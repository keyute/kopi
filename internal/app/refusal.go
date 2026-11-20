package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
)

// interrupted is the summary of a call a cancellation cut short.
const interrupted = "interrupted"

// classify reads the error text of tool name's call as what the policy, the
// human or a cancellation made of it, anchored on the policy's sentinel errors;
// a restored session has only this text. ok is false for a call that failed.
func classify(name, text string) (kind event.EndKind, summary, body string, ok bool) {
	p := fmt.Sprintf("tool %q: ", name)
	if feedback, found := strings.CutPrefix(text, p+policy.ErrRejected.Error()+", with this feedback:\n"); found {
		return event.EndRejected, "rejected", feedback, true
	}
	if strings.HasPrefix(text, p) && strings.HasSuffix(text, ": "+policy.ErrRejected.Error()) {
		return event.EndRejected, "rejected", "", true
	}
	if strings.HasSuffix(text, ": "+policy.ErrRevoked.Error()) {
		return event.EndDenied, "denied · the mode changed after approval", "", true
	}
	if rest, found := strings.CutSuffix(text, ": "+policy.ErrDenied.Error()); found {
		return event.EndDenied, denial(strings.TrimPrefix(rest, fmt.Sprintf("tool %q ", name))), "", true
	}
	canceled := context.Canceled.Error()
	if text == canceled || strings.HasSuffix(text, ": "+canceled) {
		return event.EndInterrupted, interrupted, "", true
	}
	return event.EndRan, "", "", false
}

// denial is the line for a call the policy refused; rest is its error text
// without the sentinel and the leading tool name.
func denial(rest string) string {
	switch {
	case strings.HasPrefix(rest, "is denied by rule "):
		return "denied by rule " + strings.TrimPrefix(rest, "is denied by rule ")
	case strings.HasPrefix(rest, "was denied by the approval classifier"):
		reason := strings.TrimPrefix(rest, "was denied by the approval classifier")
		return "denied by the classifier" + reason
	case strings.HasSuffix(rest, "never runs outside the OS sandbox"), strings.HasSuffix(rest, "an unsandboxed command runs only in execute mode"):
		return "denied · plan mode never runs a command outside the sandbox"
	case strings.HasSuffix(rest, "which is off or unavailable"):
		return "denied · plan mode needs the sandbox, which is off or unavailable"
	case strings.HasPrefix(rest, "needs execute mode"):
		return "denied · plan mode"
	case strings.HasPrefix(rest, "rule "):
		if rule, _, found := strings.Cut(strings.TrimPrefix(rest, "rule "), " requires approval for tool "); found {
			return "denied · rule " + rule + " asks and this run cannot ask"
		}
	case strings.Contains(rest, "classifier could not decide"):
		return "denied · the classifier could not decide and this run cannot ask"
	case strings.HasPrefix(rest, "needs approval"):
		return "denied · this run cannot ask for approval"
	}
	return "denied"
}
