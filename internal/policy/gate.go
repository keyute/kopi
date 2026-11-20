package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
)

// Target is the path a call acts on: Path as written, made absolute, and
// Real with symlinks resolved. PlanFile marks the root session's plan file:
// the gate admits it in plan mode and does not ask about it, though rules
// still apply.
type Target struct {
	Path, Real string
	PlanFile   bool
}

type ToolSpec struct {
	Capability Capability
	// Target is nil for a tool that acts on no path.
	Target func(args json.RawMessage) (Target, error)
	// Sandboxed reports that the OS confines what a call can do, so a call no
	// rule matches runs without asking.
	Sandboxed bool
	// Ask makes calls ask by default whatever the capability.
	Ask bool
	// PlanSandboxed reports that in plan mode the OS keeps a call from writing
	// the workspace, so plan mode admits it.
	PlanSandboxed bool
	// Unsandboxed reports that a call opts out of the OS sandbox; the gate then
	// treats it as a call of a tool that has none. Nil for a tool with no such
	// opt-out.
	Unsandboxed func(args json.RawMessage) bool
}

// forCall returns the spec that governs one call, and whether the call opted
// out of the sandbox.
func (s ToolSpec) forCall(args json.RawMessage) (ToolSpec, bool) {
	if s.Unsandboxed == nil || !s.Unsandboxed(args) {
		return s, false
	}
	s.Sandboxed, s.PlanSandboxed = false, false
	return s, true
}

type Answer int

const (
	Reject Answer = iota
	Allow
	// AllowEdits allows the call and every later Write call that no rule asks
	// about, for the rest of the session.
	AllowEdits
)

type Request struct {
	CallID, Tool string
	Args         json.RawMessage
	// Rule is the ask rule and its source, e.g. `Edit(.git) (built-in)`; ""
	// when no rule asked.
	Rule string
	// Edits reports that AllowEdits may be offered.
	Edits bool
	// Review is the reviewer's answer when it sent the call to the human; nil
	// otherwise.
	Review *Review
}

// Decision is a human's answer to a Request.
type Decision struct {
	Answer Answer
	// Reason is the human's feedback on a Reject; with one, the turn goes on.
	Reason string
}

// Approver asks a human about a call. It must return once ctx is done.
type Approver func(ctx context.Context, r Request) (Decision, error)

type grant struct {
	args       json.RawMessage
	target     *Target
	tool       string
	generation uint64
}

// Gate checks each tool call twice: Before runs in the pre-dispatch hook for
// the whole batch, Admit runs when the call actually executes, so a mode change
// between the two, including one while an approval is pending, is caught.
type Gate struct {
	authority *Authority
	tools     map[string]ToolSpec
	rules     *Rules
	approve   Approver
	review    Reviewer
	mu        sync.Mutex
	grants    map[string]grant
	// consecutiveDenials and totalDenials count the reviewer's denials.
	consecutiveDenials, totalDenials int
}

// NewGate takes nil rules as no rules and a nil approve as nobody being able
// to answer, so every call that needs approval is denied. A non-nil review
// decides those calls first, and only what it sends back reaches approve;
// a Write call no rule asks about is then allowed without either.
func NewGate(a *Authority, tools map[string]ToolSpec, rules *Rules, approve Approver, review Reviewer) *Gate {
	if rules == nil {
		rules = &Rules{}
	}
	return &Gate{authority: a, tools: maps.Clone(tools), rules: rules, approve: approve, review: review, grants: map[string]grant{}}
}

func (g *Gate) Before(ctx context.Context, callID, tool string, args json.RawMessage) error {
	args = bytes.Clone(args)
	spec, ok := g.tools[tool]
	if !ok {
		return fmt.Errorf("tool %q: %w", tool, ErrUnknownTool)
	}
	spec, optOut := spec.forCall(args)
	_, gen, permitted := g.authority.Permits(spec)
	var target *Target
	var targetErr error
	var planFile bool
	if spec.Target != nil {
		t, err := spec.Target(args)
		// A target that failed to resolve still says whether it is the plan file.
		planFile = t.PlanFile
		if err != nil {
			targetErr = err
		} else {
			target = &t
		}
	}
	if !permitted && !planFile {
		if optOut {
			return fmt.Errorf("tool %q needs execute mode; the session is in plan mode, where it never runs outside the OS sandbox: %w", tool, ErrDenied)
		}
		if spec.Capability == Exec && !strings.HasPrefix(tool, MCPPrefix) {
			return fmt.Errorf("tool %q needs execute mode; the session is in plan mode, where it runs only in the OS sandbox, which is off or unavailable: %w", tool, ErrDenied)
		}
		return fmt.Errorf("tool %q needs execute mode; the session is in %s mode: %w", tool, Plan, ErrDenied)
	}
	if targetErr != nil {
		return fmt.Errorf("failed to resolve the path of tool %q: %w", tool, targetErr)
	}
	req := Request{CallID: callID, Tool: tool, Args: bytes.Clone(args)}
	switch eff, rule, src := g.rules.decide(tool, target); eff {
	case effectDeny:
		return fmt.Errorf("tool %q is denied by rule %s (%s): %w", tool, rule, src, ErrDenied)
	case effectAsk:
		req.Rule = rule + " (" + src + ")"
		if err := g.ask(ctx, req, ""); err != nil {
			return err
		}
	case effectAllow:
	default:
		if !planFile && (spec.Ask || spec.Capability != Read && !spec.Sandboxed && (spec.Capability != Write || !g.authority.EditsGranted())) {
			req.Edits = spec.Capability == Write
			allow := ""
			if !optOut {
				allow = tool
				if target != nil {
					allow += "(" + escape(filepath.ToSlash(target.Path)) + ")"
				}
			}
			switch {
			case g.review == nil:
				if err := g.ask(ctx, req, allow); err != nil {
					return err
				}
			case spec.Capability == Write && !spec.Ask:
			default:
				if err := g.reviewed(ctx, req, allow); err != nil {
					return err
				}
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants[callID] = grant{tool: tool, generation: gen, args: args, target: target}
	return nil
}

// ask asks the approver about r. allow is the rule that would allow the call
// when no rule asked about it; empty when no rule should be suggested.
func (g *Gate) ask(ctx context.Context, r Request, allow string) error {
	if g.approve == nil {
		if r.Rule != "" {
			return fmt.Errorf("rule %s requires approval for tool %q and this run cannot ask for it: %w", r.Rule, r.Tool, ErrDenied)
		}
		return fmt.Errorf("tool %q needs approval and this run cannot ask for it%s: %w", r.Tool, allowHint(allow), ErrDenied)
	}
	d, err := g.approve(ctx, r)
	if err != nil {
		return fmt.Errorf("failed to get approval for tool %q: %w", r.Tool, err)
	}
	switch {
	case d.Answer == AllowEdits && r.Edits:
		g.authority.GrantEdits()
		g.mu.Lock()
		g.consecutiveDenials = 0
		g.mu.Unlock()
		return nil
	case d.Answer == Allow || d.Answer == AllowEdits:
		g.mu.Lock()
		g.consecutiveDenials = 0
		g.mu.Unlock()
		return nil
	case d.Reason != "":
		return fmt.Errorf("tool %q: %w, with this feedback:\n%s", r.Tool, ErrRejected, d.Reason)
	default:
		return fmt.Errorf("tool %q: the user rejected this call; stop and wait for their instructions: %w", r.Tool, ErrRejected)
	}
}

// allowHint is the suffix suggesting the rule that would allow the call.
func allowHint(allow string) string {
	if allow == "" {
		return ""
	}
	return "; add an allow rule to permissions.allow in config.json or pass --allowed-tools, such as " + allow
}

// reviewed has the reviewer decide r, and asks the human only when it cannot
// decide or has denied too often. allow is as for ask.
func (g *Gate) reviewed(ctx context.Context, r Request, allow string) error {
	rev, err := g.review(ctx, r)
	if ctx.Err() != nil {
		return fmt.Errorf("failed to review tool %q: %w", r.Tool, ctx.Err())
	}
	if err != nil {
		rev = Review{Verdict: VerdictAsk}
	}
	switch rev.Verdict {
	case VerdictAllow:
		g.mu.Lock()
		g.consecutiveDenials = 0
		g.mu.Unlock()
		return nil
	case VerdictDeny:
		g.mu.Lock()
		g.consecutiveDenials++
		g.totalDenials++
		escalate := g.consecutiveDenials >= maxConsecutiveDenials || g.totalDenials >= maxTotalDenials
		if g.totalDenials >= maxTotalDenials {
			g.totalDenials = 0
		}
		g.mu.Unlock()
		if g.approve == nil || !escalate {
			reason := ""
			if rev.Reason != "" {
				reason = ": " + rev.Reason
			}
			return fmt.Errorf("tool %q was denied by the approval classifier%s: %w", r.Tool, reason, ErrDenied)
		}
	default:
		if g.approve == nil {
			return fmt.Errorf("tool %q needs approval, the approval classifier could not decide and this run cannot ask for it%s: %w", r.Tool, allowHint(allow), ErrDenied)
		}
	}
	r.Review = &rev
	return g.ask(ctx, r, allow)
}

// Admit consumes the grant and compares the execution arguments and the
// target bound to the tool's prepared operation with the approved call. It
// returns the mode the call is admitted under, Plan on any error.
func (g *Gate) Admit(callID, tool string, args json.RawMessage, target *Target) (Mode, error) {
	g.mu.Lock()
	gr, ok := g.grants[callID]
	delete(g.grants, callID)
	g.mu.Unlock()
	if !ok || gr.tool != tool {
		return Plan, fmt.Errorf("tool %q: %w", tool, ErrNoGrant)
	}
	if !bytes.Equal(gr.args, args) || (gr.target == nil) != (target == nil) || (target != nil && *gr.target != *target) {
		return Plan, fmt.Errorf("tool %q: arguments or target changed; make a new call: %w", tool, ErrChangedCall)
	}
	spec, _ := g.tools[tool].forCall(args)
	mode, gen, ok := g.authority.Permits(spec)
	if (target == nil || !target.PlanFile) && (!ok || (spec.Capability.NeedsExecute() && gen != gr.generation)) {
		return Plan, fmt.Errorf("tool %q: the session mode changed after the call was approved: %w", tool, ErrRevoked)
	}
	return mode, nil
}

// Finish discards any unused grant when execution fails before admission.
func (g *Gate) Finish(callID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.grants, callID)
}
