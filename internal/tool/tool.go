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
	Run        func(ctx context.Context, args json.RawMessage) (string, error)
	Display    Display
}

// Display is how a call reads in a transcript.
type Display struct {
	// Title names one call: "Read cmd/root.go".
	Title func(args json.RawMessage) string
	// Summary is the line under a successfully finished call: "212 lines".
	Summary func(args json.RawMessage, output string) string
	// Action is how a group counts calls: "Read 3 files".
	Action Action
}

type Action struct{ Verb, One, Many string }
