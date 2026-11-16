package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

// maxPreviewLines bounds each side of an approval preview.
const maxPreviewLines = 10

type writeArgs struct {
	FilePath string `json:"file_path"`
	// Content is a pointer so a missing content is told apart from an empty one.
	Content *string `json:"content"`
}

func NewWrite(ws *Workspace) Tool {
	return Tool{
		Name:  "Write",
		Label: "Write",
		Description: "Writes a file inside the workspace, creating it or replacing its whole content. " +
			"file_path is absolute or relative to the workspace.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path of the file to write, absolute or relative to the workspace.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The complete new content of the file.",
				},
			},
			"required":             []string{"file_path", "content"},
			"additionalProperties": false,
		},
		Capability: policy.Write,
		Target:     pathTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args, err := decodeWriteArgs(raw)
			if err != nil {
				return "", err
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := ws.WriteFile(args.FilePath, []byte(*args.Content)); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %s to %s", lineCount(len(splitLines(*args.Content))), args.FilePath), nil
		},
		Display: Display{
			Title: pathTitle("Write"),
			// Relies on Run's output shape: "wrote N lines to P".
			Summary: func(output string) string {
				n, _, _ := strings.Cut(strings.TrimPrefix(output, "wrote "), " to ")
				return n
			},
			Preview: writePreview,
			Action:  Action{Verb: "Wrote", One: "file", Many: "files"},
		},
	}
}

func decodeWriteArgs(raw json.RawMessage) (writeArgs, error) {
	args, err := decodeArgs[writeArgs](raw)
	if err != nil {
		return args, err
	}
	if args.FilePath == "" {
		return args, fmt.Errorf("%w: file_path is required", ErrInvalidArgs)
	}
	if args.Content == nil {
		return args, fmt.Errorf("%w: content is required", ErrInvalidArgs)
	}
	return args, nil
}

// writePreview runs before the arguments are validated, so it decodes leniently.
func writePreview(raw json.RawMessage) []PreviewLine {
	var args struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &args)
	lines := splitLines(args.Content)
	return append([]PreviewLine{{Kind: Context, Text: lineCount(len(lines))}}, previewLines(Added, lines)...)
}

// pathTitle returns a Display.Title naming the call by its file_path. It runs
// before the arguments are validated, so it decodes leniently.
func pathTitle(verb string) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(raw, &args)
		if args.FilePath == "" {
			return verb
		}
		return verb + " " + args.FilePath
	}
}

// previewLines shows up to maxPreviewLines of lines as kind, then a marker
// counting the rest.
func previewLines(kind PreviewKind, lines []string) []PreviewLine {
	var out []PreviewLine
	for _, l := range lines[:min(len(lines), maxPreviewLines)] {
		out = append(out, PreviewLine{Kind: kind, Text: l})
	}
	switch rest := len(lines) - maxPreviewLines; {
	case rest == 1:
		out = append(out, PreviewLine{Kind: Context, Text: "… 1 more line"})
	case rest > 1:
		out = append(out, PreviewLine{Kind: Context, Text: fmt.Sprintf("… %d more lines", rest)})
	}
	return out
}

// splitLines treats a final newline as ending the last line, not starting
// another.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func lineCount(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
