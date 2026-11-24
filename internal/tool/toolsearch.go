package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const (
	defaultToolSearchLimit = 8
	maxToolSearchLimit     = 20
	toolSearchNoMatch      = "No deferred tool matches "
)

type toolSearchArgs struct {
	Query string `json:"query"`
	Limit *int   `json:"limit"`
}

// NewToolSearch returns the tool that loads deferred tools into the session.
// deferred is the tools to search when a call runs, and connecting, which may
// be nil, names the MCP servers whose tools are not among them yet. rank picks at
// most limit names from tools for a keyword query; load makes the named tools
// callable from the next response on.
func NewToolSearch(deferred func() []Tool, connecting func() []string, rank func(query string, tools []Tool, limit int) []string, load func(ctx context.Context, names []string) error) Tool {
	return Tool{
		Name:  "ToolSearch",
		Label: "ToolSearch",
		Description: "Loads deferred tools, such as the MCP server tools the system prompt lists by name, so that they can be called. " +
			"query is keywords for what you need, or exact tool names separated by spaces to load just those. " +
			"Returns the name and description of each tool it loaded; call them in your next response.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Keywords for the tools you need, or exact tool names separated by spaces.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     maxToolSearchLimit,
					"default":     defaultToolSearchLimit,
					"description": "Maximum number of tools a keyword query loads.",
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeToolSearchArgs(raw)
			if err != nil {
				return Result{}, err
			}
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			tools := deferred()
			names := exactToolNames(args.Query, tools)
			if names == nil {
				names = rank(args.Query, tools, *args.Limit)
			}
			if len(names) == 0 {
				text := fmt.Sprintf("%s%q. Try other keywords, or exact names from the system prompt.", toolSearchNoMatch, args.Query)
				if connecting != nil {
					if servers := connecting(); len(servers) > 0 {
						text += " MCP servers still connecting, whose tools cannot be found yet: " + strings.Join(servers, ", ") + "."
					}
				}
				return Result{Text: text}, nil
			}
			if err := load(ctx, names); err != nil {
				return Result{}, fmt.Errorf("failed to load tools: %w", err)
			}
			return Result{Text: loadedText(names, tools)}, nil
		},
		Display: Display{
			Title: queryTitle("ToolSearch", false),
			Done:  queryTitle("Searched tools", true),
			Body: func(output string) string {
				if !strings.HasPrefix(output, "Loaded ") {
					return ""
				}
				return output
			},
			Summary: func(output string) string {
				var n int
				if _, err := fmt.Sscanf(output, "Loaded %d tool", &n); err != nil {
					return "no match"
				}
				if n == 1 {
					return "loaded 1 tool"
				}
				return fmt.Sprintf("loaded %d tools", n)
			},
			Action: Action{Verb: "Searched", One: "tools"},
		},
	}
}

func decodeToolSearchArgs(raw json.RawMessage) (toolSearchArgs, error) {
	args, err := decodeArgs[toolSearchArgs](raw)
	if err != nil {
		return args, err
	}
	if strings.TrimSpace(args.Query) == "" {
		return args, fmt.Errorf("%w: query is required", ErrInvalidArgs)
	}
	if args.Limit == nil {
		limit := defaultToolSearchLimit
		args.Limit = &limit
	}
	if *args.Limit < 1 || *args.Limit > maxToolSearchLimit {
		return args, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidArgs, maxToolSearchLimit)
	}
	return args, nil
}

// exactToolNames returns query's words, deduplicated, when every one names a
// deferred tool, and nil otherwise.
func exactToolNames(query string, deferred []Tool) []string {
	var names []string
	for _, word := range strings.Fields(query) {
		if !slices.ContainsFunc(deferred, func(t Tool) bool { return t.Name == word }) {
			return nil
		}
		if !slices.Contains(names, word) {
			names = append(names, word)
		}
	}
	return names
}

func loadedText(names []string, deferred []Tool) string {
	out := &outputBuffer{}
	if len(names) == 1 {
		_, _ = out.Write([]byte("Loaded 1 tool; call it in your next response:"))
	} else {
		_, _ = fmt.Fprintf(out, "Loaded %d tools; call them in your next response:", len(names))
	}
	for _, name := range names {
		line := "\n- " + name
		if i := slices.IndexFunc(deferred, func(t Tool) bool { return t.Name == name }); i >= 0 && deferred[i].Description != "" {
			first, _, _ := strings.Cut(deferred[i].Description, "\n")
			line += ": " + first
		}
		_, _ = out.Write([]byte(line))
	}
	return out.String()
}

// queryTitle returns a Display.Title naming the call by its query, in quotes
// after verb and "for" when quoted. It runs before the arguments are validated, so it
// decodes leniently.
func queryTitle(verb string, quoted bool) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &args)
		switch {
		case args.Query == "":
			return verb
		case quoted:
			return verb + ` for "` + titleLine(args.Query) + `"`
		}
		return titleLine(verb + " " + args.Query)
	}
}
