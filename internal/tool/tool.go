package tool

import (
	"context"
	"encoding/json"

	"github.com/keyute/kopi/internal/policy"
)

type Tool struct {
	Name, Label, Description string
	// Parameters is the JSON schema object for the arguments.
	Parameters map[string]any
	Capability policy.Capability
	// Target is nil for a tool that acts on no path.
	Target func(args json.RawMessage) (policy.Target, error)
	// Sandboxed reports that the OS confines what the tool's calls can do.
	Sandboxed bool
	// Ask makes calls ask by default whatever the capability.
	Ask bool
	// PlanSandboxed reports that in plan mode the OS keeps the tool's calls
	// from writing the workspace.
	PlanSandboxed bool
	// Unsandboxed reports that a call opts out of the OS sandbox; nil for a tool
	// with no such opt-out.
	Unsandboxed func(args json.RawMessage) bool
	// Deferred keeps the tool out of the model's tool list until ToolSearch
	// loads it.
	Deferred bool
	// Exclusive runs each call alone: no other call of the same response runs alongside it.
	Exclusive bool
	// Run must acquire a nonmutating operation, call admit with its bound
	// target before reading content or modifying files, and use that operation.
	// admit returns the mode the call is admitted under.
	Run     func(ctx context.Context, args json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error)
	Display Display
}

// Result is what a call returns to the model: its text, then any images.
type Result struct {
	Text   string
	Images []Image
}

type Image struct {
	MimeType string
	Data     []byte
}

// Specs returns what the policy needs to know of each tool, by name.
func Specs(tools []Tool) map[string]policy.ToolSpec {
	specs := make(map[string]policy.ToolSpec, len(tools))
	for _, t := range tools {
		specs[t.Name] = policy.ToolSpec{Capability: t.Capability, Target: t.Target, Sandboxed: t.Sandboxed, Ask: t.Ask, PlanSandboxed: t.PlanSandboxed, Unsandboxed: t.Unsandboxed}
	}
	return specs
}

// Display is how a call reads in a transcript.
type Display struct {
	// Title names one call: "Read cmd/root.go".
	Title func(args json.RawMessage) string
	// Summary is the line under a successfully finished call: "212 lines".
	Summary func(output string) string
	// Preview is what an approval prompt shows of the change a call would make.
	Preview func(args json.RawMessage) []PreviewLine
	// Action is how a group counts calls: "Read 3 files".
	Action Action
}

type Action struct{ Verb, One, Many string }

type PreviewKind int

const (
	Context PreviewKind = iota
	Removed
	Added
)

type PreviewLine struct {
	Kind PreviewKind
	Text string
}
