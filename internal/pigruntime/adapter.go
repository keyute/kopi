package pigruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
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
	defer a.gate.Finish(toolCallID)
	res, err := a.tool.Run(ctx, params, func(target policy.Target) (policy.Mode, error) {
		var path *policy.Target
		if a.tool.Target != nil {
			path = &target
		}
		return a.gate.Admit(toolCallID, a.tool.Name, params, path)
	})
	if err != nil {
		return errorResult(err), nil
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
	return agent.AgentToolResult{Content: append([]ai.ToolResultMessageContent{ai.TextContent{Text: text}}, images...)}, nil
}

func (a agentTool) ExecutionMode() agent.ToolExecutionMode {
	if a.tool.Capability == policy.Read {
		return agent.ToolModeParallel
	}
	return agent.ToolModeSequential
}

func beforeToolCall(gate *policy.Gate) agent.BeforeToolCallHook {
	return func(ctx context.Context, toolCallID, toolName string, args json.RawMessage) agent.ToolCallHookResult {
		if err := gate.Before(ctx, toolCallID, toolName, args); err != nil {
			// PiG checks Err before ctx, but replaces a Block's Reason with
			// "Operation aborted" when ctx is done, so only Err keeps the
			// refusal's text when an abort lands at the same moment.
			return agent.ToolCallHookResult{Err: err}
		}
		return agent.ToolCallHookResult{}
	}
}

func errorResult(err error) agent.AgentToolResult {
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: err.Error()}}, IsError: true}
}
