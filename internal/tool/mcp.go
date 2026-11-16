package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

// MCPNameLimit is the longest tool name model providers accept.
const MCPNameLimit = 64

// MCPName is the model-facing name of a server's tool: mcp__<server>__<tool>,
// with every character outside [A-Za-z0-9_-] in either part replaced by "_".
func MCPName(server, tool string) string {
	return policy.MCPPrefix + mcpNamePart(server) + "__" + mcpNamePart(tool)
}

func mcpNamePart(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}

type MCPTool struct {
	// Tool is the server's own name for the tool.
	Server, Tool, Description string
	// Parameters is the JSON schema object for the arguments.
	Parameters map[string]any
	ReadOnly   bool
}

// NewMCP returns t as a tool whose calls go to call. The caller checks the
// name against MCPNameLimit and other tools' names.
func NewMCP(t MCPTool, call func(ctx context.Context, args json.RawMessage) (string, error)) Tool {
	label := t.Server + "/" + t.Tool
	description := t.Description
	if description == "" {
		description = "Tool " + t.Tool + " of MCP server " + t.Server + "."
	}
	capability := policy.Exec
	if t.ReadOnly {
		capability = policy.Read
	}
	return Tool{
		Name:        MCPName(t.Server, t.Tool),
		Label:       label,
		Description: description,
		Parameters:  t.Parameters,
		Capability:  capability,
		Ask:         true,
		Run: func(ctx context.Context, args json.RawMessage, admit func(policy.Target) error) (string, error) {
			if err := admit(policy.Target{}); err != nil {
				return "", err
			}
			text, err := call(ctx, args)
			if err != nil {
				return "", err
			}
			out := &outputBuffer{}
			_, _ = out.Write([]byte(text))
			return out.String(), nil
		},
		Display: Display{
			Title: func(raw json.RawMessage) string {
				title := label
				var compact bytes.Buffer
				if json.Compact(&compact, raw) == nil && compact.String() != "{}" {
					title += " " + compact.String()
				}
				line, whole := bashTitleLine(title)
				if !whole {
					line += " …"
				}
				return line
			},
			Summary: func(output string) string {
				if output == noOutputNotice {
					return "no output"
				}
				return lineCount(countLines(output))
			},
			Preview: func(raw json.RawMessage) []PreviewLine {
				var pretty bytes.Buffer
				if json.Indent(&pretty, raw, "", "  ") != nil {
					return previewLines(Context, string(raw))
				}
				return previewLines(Context, pretty.String())
			},
			Action: Action{Verb: "Called", One: "MCP tool", Many: "MCP tools"},
		},
	}
}
