package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeWriteArgs(raw)
			if err != nil {
				return Result{}, err
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			dest, err := ws.prepare(args.FilePath)
			if err != nil {
				return Result{}, err
			}
			defer func() { dest.parent.Close() }()
			if _, err := admit(dest.target); err != nil {
				return Result{}, err
			}
			if err := dest.write(ws, []byte(*args.Content)); err != nil {
				return Result{}, err
			}
			return Result{Text: fmt.Sprintf("wrote %s to %s", lineCount(countLines(*args.Content)), args.FilePath)}, nil
		},
		Display: Display{
			Title:   pathTitle(ws, "Write"),
			Done:    pathTitle(ws, "Wrote"),
			Change:  writeChange,
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
	return append([]PreviewLine{{Kind: Context, Text: lineCount(countLines(args.Content))}}, previewLines(Added, args.Content)...)
}

// writeChange runs before the arguments are validated, so it decodes leniently.
func writeChange(raw json.RawMessage) (string, []PreviewLine) {
	var args struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &args)
	return fmt.Sprintf("+%d", countLines(args.Content)), changeLines(Added, args.Content)
}

// changeLines is every line of text as kind, each cut to maxLineChars runes.
func changeLines(kind PreviewKind, text string) []PreviewLine {
	lines := previewLinesCap(kind, text, math.MaxInt)
	for i, l := range lines {
		n := 0
		for j := range l.Text {
			if n == maxLineChars {
				lines[i].Text = l.Text[:j]
				break
			}
			n++
		}
	}
	return lines
}

// pathTitle returns a Display.Title naming the call by its file_path. It runs
// before the arguments are validated, so it decodes leniently.
func pathTitle(ws *Workspace, verb string) func(json.RawMessage) string {
	return func(raw json.RawMessage) string {
		var args struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(raw, &args)
		if args.FilePath == "" {
			return verb
		}
		return verb + " " + ws.shown(args.FilePath)
	}
}

// previewLines shows up to maxPreviewLines of lines as kind, then a marker
// counting the rest.
func previewLines(kind PreviewKind, text string) []PreviewLine {
	return previewLinesCap(kind, text, maxPreviewLines)
}

// previewLinesCap shows up to limit of lines as kind, then a marker counting
// the rest.
func previewLinesCap(kind PreviewKind, text string, limit int) []PreviewLine {
	n := countLines(text)
	out := make([]PreviewLine, 0, min(n, limit)+1)
	restText := strings.TrimSuffix(text, "\n")
	for range min(n, limit) {
		line, rest, _ := strings.Cut(restText, "\n")
		out = append(out, PreviewLine{Kind: kind, Text: line})
		restText = rest
	}
	switch rest := n - limit; {
	case rest == 1:
		out = append(out, PreviewLine{Kind: Context, Text: "… 1 more line"})
	case rest > 1:
		out = append(out, PreviewLine{Kind: Context, Text: fmt.Sprintf("… %d more lines", rest)})
	}
	return out
}

// A final newline ends the last line rather than starting another.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func lineCount(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
