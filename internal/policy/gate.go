package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
)

// Target is the path a call acts on: Path as written, made absolute, and
// Real with symlinks resolved.
type Target struct{ Path, Real string }

type ToolSpec struct {
	Capability Capability
	// Target is nil for a tool that acts on no path.
	Target func(args json.RawMessage) (Target, error)
	// Sandboxed reports that the OS confines what a call can do, so a call no
	// rule matches runs without asking.
	Sandboxed bool
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
}

// Approver asks a human about a call. It must return once ctx is done.
type Approver func(ctx context.Context, r Request) (Answer, error)

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
	mu        sync.Mutex
	grants    map[string]grant
	edits     bool
}

// NewGate takes nil rules as no rules and a nil approve as nobody being able
// to answer, so every call that needs approval is denied.
func NewGate(a *Authority, tools map[string]ToolSpec, rules *Rules, approve Approver) *Gate {
	if rules == nil {
		rules = &Rules{}
	}
	return &Gate{authority: a, tools: maps.Clone(tools), rules: rules, approve: approve, grants: map[string]grant{}}
}

func (g *Gate) Before(ctx context.Context, callID, tool string, args json.RawMessage) error {
	args = bytes.Clone(args)
	spec, ok := g.tools[tool]
	if !ok {
		return fmt.Errorf("tool %q: %w", tool, ErrUnknownTool)
	}
	gen, ok := g.authority.Permits(spec.Capability)
	if !ok {
		// Only plan mode denies anything, so the mode is known without a racy re-read.
		return fmt.Errorf("tool %q needs execute mode; the session is in %s mode: %w", tool, Plan, ErrDenied)
	}
	var target *Target
	if spec.Target != nil {
		t, err := spec.Target(args)
		if err != nil {
			return fmt.Errorf("failed to resolve the path of tool %q: %w", tool, err)
		}
		target = &t
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
		if spec.Capability != Read && !spec.Sandboxed && (spec.Capability != Write || !g.editsGranted()) {
			req.Edits = spec.Capability == Write
			allow := tool
			if target != nil {
				allow += "(" + escape(filepath.ToSlash(target.Path)) + ")"
			}
			if err := g.ask(ctx, req, allow); err != nil {
				return err
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants[callID] = grant{tool: tool, generation: gen, args: args, target: target}
	return nil
}

func (g *Gate) editsGranted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.edits
}

// ask asks the approver about r. allow is the rule that would allow the call
// when no rule asked about it.
func (g *Gate) ask(ctx context.Context, r Request, allow string) error {
	if g.approve == nil {
		if r.Rule != "" {
			return fmt.Errorf("rule %s requires approval for tool %q and this run cannot ask for it: %w", r.Rule, r.Tool, ErrDenied)
		}
		return fmt.Errorf("tool %q needs approval and this run cannot ask for it; add an allow rule to permissions.allow in config.json or pass --allowed-tools, such as %s: %w", r.Tool, allow, ErrDenied)
	}
	a, err := g.approve(ctx, r)
	if err != nil {
		return fmt.Errorf("failed to get approval for tool %q: %w", r.Tool, err)
	}
	switch {
	case a == AllowEdits && r.Edits:
		g.mu.Lock()
		g.edits = true
		g.mu.Unlock()
		return nil
	case a == Allow || a == AllowEdits:
		return nil
	default:
		return fmt.Errorf("tool %q: the user rejected this call; stop and wait for their instructions: %w", r.Tool, ErrRejected)
	}
}

// Admit consumes the grant and compares the execution arguments and the
// target bound to the tool's prepared operation with the approved call.
func (g *Gate) Admit(callID, tool string, args json.RawMessage, target *Target) error {
	g.mu.Lock()
	gr, ok := g.grants[callID]
	delete(g.grants, callID)
	g.mu.Unlock()
	if !ok || gr.tool != tool {
		return fmt.Errorf("tool %q: %w", tool, ErrNoGrant)
	}
	if !bytes.Equal(gr.args, args) || (gr.target == nil) != (target == nil) || (target != nil && *gr.target != *target) {
		return fmt.Errorf("tool %q: arguments or target changed; make a new call: %w", tool, ErrChangedCall)
	}
	c := g.tools[tool].Capability
	gen, ok := g.authority.Permits(c)
	if !ok || (c.NeedsExecute() && gen != gr.generation) {
		return fmt.Errorf("tool %q: the session mode changed after the call was approved: %w", tool, ErrRevoked)
	}
	return nil
}

// Finish discards any unused grant when execution fails before admission.
func (g *Gate) Finish(callID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.grants, callID)
}
