package pigruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"

	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type agentTool struct {
	tool tool.Tool
	gate *policy.Gate
}

func (a agentTool) Name() string  { return a.tool.Name }
func (a agentTool) Label() string { return a.tool.Label }

func (a agentTool) Schema() ai.ToolSchema {
	return ai.ToolSchema{Name: a.tool.Name, Description: a.tool.Description, Parameters: a.tool.Parameters}
}

// Execute admits the call against the grant recorded by the before-tool hook:
// PiG runs every hook of a parallel batch before executing any call, so the
// mode may have changed in between.
func (a agentTool) Execute(ctx context.Context, toolCallID string, params json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	return a.run(ctx, toolCallID, params), nil
}

func (a agentTool) run(ctx context.Context, toolCallID string, params json.RawMessage) agent.AgentToolResult {
	defer a.gate.Finish(toolCallID)
	res, err := a.tool.Run(ctx, params, func(target policy.Target) (policy.Mode, error) {
		var path *policy.Target
		if a.tool.Target != nil {
			path = &target
		}
		return a.gate.Admit(toolCallID, a.tool.Name, params, path)
	})
	if err != nil {
		return errorResult(err)
	}
	// PiG sends an image it fails to process unchanged, which the provider
	// rejects, so one that does not decode here is left out. The original is
	// kept so PiG's own resize to the model's limits notes the change.
	text := res.Text
	var images []ai.ToolResultMessageContent
	for _, img := range res.Images {
		data := base64.StdEncoding.EncodeToString(img.Data)
		r, err := subprocess.ResizeImage(subprocess.ResizeImageArgs{Data: data, MimeType: img.MimeType})
		if err != nil || r.Image == nil {
			slog.Warn("failed to resize tool result image; omitting it", "tool", a.tool.Name, "mime", img.MimeType, "error", err)
			text += "\n" + tool.OmittedImageNote
			continue
		}
		images = append(images, ai.ImageContent{Data: data, MimeType: img.MimeType})
	}
	return agent.AgentToolResult{Content: append([]ai.ToolResultMessageContent{ai.TextContent{Text: text}}, images...)}
}

// registration is t as the runtime's extension holds it, for a tool that is
// not always callable or that joins a session after it opens. A call runs
// through the gate of the session whose Send supplied its ctx, so one
// registration serves every root session. Its exposure is deferred so that
// only Kopi makes it callable: on every refresh of a session's tools PiG makes
// each tool the allowlist names callable again unless its exposure is deferred
// (PiG v0.8.0 coding/session_tool_registry.go refreshTools; recheck on a bump).
func registration(t tool.Tool) (extension.RegisteredTool, error) {
	parameters, err := json.Marshal(t.Parameters)
	if err != nil {
		return extension.RegisteredTool{}, fmt.Errorf("failed to encode the parameters of tool %s: %w", t.Name, err)
	}
	return extension.RegisteredTool{Definition: extension.ToolDefinition{
		Name:          t.Name,
		Label:         t.Label,
		Description:   t.Description,
		Parameters:    parameters,
		Exposure:      extension.ToolExposureDeferred,
		ExecutionMode: string(agentTool{tool: t}.ExecutionMode()),
		Execute: func(ctx context.Context, toolCallID string, params json.RawMessage, _ extension.AgentToolUpdateCallback) (extension.AgentToolResult, error) {
			s, ok := ctx.Value(turnSessionKey{}).(*Session)
			if !ok {
				return errorResult(fmt.Errorf("failed to call %s: %w", t.Name, ErrNoTurn)), nil
			}
			return agentTool{tool: t, gate: s.gate}.run(ctx, toolCallID, params), nil
		},
	}}, nil
}

// register puts tools in the runtime's extension, replacing those of the same
// name.
func register(joining *extension.Extension, tools []tool.Tool) error {
	registered := make([]extension.RegisteredTool, 0, len(tools))
	for _, t := range tools {
		reg, err := registration(t)
		if err != nil {
			return err
		}
		registered = append(registered, reg)
	}
	joining.SetRegisteredTools(registered)
	return nil
}

func (a agentTool) ExecutionMode() agent.ToolExecutionMode {
	if a.tool.Capability == policy.Read && !a.tool.Exclusive {
		return agent.ToolModeParallel
	}
	return agent.ToolModeSequential
}

func beforeToolCall(gate *policy.Gate) agent.BeforeToolCallHook {
	return func(ctx context.Context, toolCallID, toolName string, args json.RawMessage) agent.ToolCallHookResult {
		if err := gate.Before(ctx, toolCallID, toolName, args); err != nil {
			// PiG replaces a Block's Reason with "Operation aborted" when ctx is
			// done, but keeps an Err's text unless it is the ctx error (PiG
			// v0.8.0 agent/tool_execution.go prepareToolCall; recheck on a bump).
			return agent.ToolCallHookResult{Err: err}
		}
		return agent.ToolCallHookResult{}
	}
}

func errorResult(err error) agent.AgentToolResult {
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: err.Error()}}, IsError: true}
}
