package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// ValidateMCPServerName keeps the normalized server component unambiguous in
// permission prefixes such as mcp__github__*.
func ValidateMCPServerName(name string) error {
	part := mcpNamePart(name)
	if part == "" {
		return fmt.Errorf("%w: empty server name", ErrInvalidMCPServerName)
	}
	if strings.Contains(part, "__") || strings.HasSuffix(part, "_") {
		return fmt.Errorf("%w: normalized server name must not contain __ or end in _", ErrInvalidMCPServerName)
	}
	return nil
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

type mcpError struct {
	text string
	err  error
}

func (e *mcpError) Error() string { return e.text }
func (e *mcpError) Unwrap() error { return e.err }

type MCPTool struct {
	// Tool is the server's own name for the tool.
	Server, Tool, Description string
	// Parameters is the JSON schema object for the arguments.
	Parameters map[string]any
	ReadOnly   bool
}

// NewMCP returns t as a tool whose calls go to call. The caller checks the
// name against MCPNameLimit and other tools' names. Errors and supplemental
// text share the same output bound.
func NewMCP(t MCPTool, call func(ctx context.Context, args json.RawMessage) (string, error)) Tool {
	label := t.Server + "/" + t.Tool
	// It runs before the arguments are validated, so it decodes leniently.
	title := func(raw json.RawMessage) string {
		line := label
		var compact bytes.Buffer
		if json.Compact(&compact, raw) == nil && compact.String() != "{}" {
			line += " " + compact.String()
		}
		return titleLine(line)
	}
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
		Deferred:    true,
		Run: func(ctx context.Context, args json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return Result{}, err
			}
			text, err := call(ctx, args)
			out := &outputBuffer{}
			if err != nil {
				_, _ = out.Write([]byte(err.Error()))
				if text != "" {
					_, _ = out.Write([]byte(": "))
					_, _ = out.Write([]byte(text))
				}
				return Result{}, &mcpError{text: out.String(), err: err}
			}
			_, _ = out.Write([]byte(text))
			return Result{Text: out.String()}, nil
		},
		Display: Display{
			Title: title,
			Done:  func(raw json.RawMessage) string { return "Called " + title(raw) },
			Body: func(output string) string {
				if output == noOutputNotice {
					return ""
				}
				return output
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
