package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const maxEditBytes = 10 << 20

type editArgs struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	// NewString is a pointer so a missing new_string is told apart from an
	// empty one, which deletes the match.
	NewString  *string `json:"new_string"`
	ReplaceAll bool    `json:"replace_all"`
}

func NewEdit(ws *Workspace) Tool {
	return Tool{
		Name:  "Edit",
		Label: "Edit",
		Description: "Replaces old_string with new_string in a text file inside the workspace. " +
			"old_string must match exactly once unless replace_all is set.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path of the file to edit, absolute or relative to the workspace.",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "The exact text to replace.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "The text to replace it with; must differ from old_string.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence of old_string instead of exactly one.",
				},
			},
			"required":             []string{"file_path", "old_string", "new_string"},
			"additionalProperties": false,
		},
		Capability: policy.Write,
		Target:     pathTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeEditArgs(raw)
			if err != nil {
				return Result{}, err
			}
			return edit(ctx, ws, args, admit)
		},
		Display: Display{
			Title: func(raw json.RawMessage) string {
				var args struct {
					ReplaceAll bool `json:"replace_all"`
				}
				_ = json.Unmarshal(raw, &args)
				title := pathTitle(ws, "Edit")(raw)
				if args.ReplaceAll {
					title += " (replace all)"
				}
				return title
			},
			Done:   pathTitle(ws, "Edited"),
			Change: editChange,
			// Relies on edit's output shape: "replaced N occurrences in P".
			Summary: func(output string) string {
				n, _, _ := strings.Cut(strings.TrimPrefix(output, "replaced "), " in ")
				if n == "1 occurrence" {
					return ""
				}
				return n
			},
			Preview: editPreview,
			Action:  Action{Verb: "Edited", One: "file", Many: "files"},
			Outcome: ws.fileOutcome("Edited"),
			Subject: ws.fileSubject,
		},
	}
}

func decodeEditArgs(raw json.RawMessage) (editArgs, error) {
	args, err := decodeArgs[editArgs](raw)
	if err != nil {
		return args, err
	}
	switch {
	case args.FilePath == "":
		return args, fmt.Errorf("%w: file_path is required", ErrInvalidArgs)
	case args.OldString == "":
		return args, fmt.Errorf("%w: old_string must not be empty", ErrInvalidArgs)
	case args.NewString == nil:
		return args, fmt.Errorf("%w: new_string is required", ErrInvalidArgs)
	case *args.NewString == args.OldString:
		return args, fmt.Errorf("%w: old_string and new_string are identical", ErrInvalidArgs)
	}
	return args, nil
}

func edit(ctx context.Context, ws *Workspace, args editArgs, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	dest, err := ws.prepare(args.FilePath)
	if err != nil {
		return Result{}, err
	}
	defer func() { dest.parent.Close() }()
	if dest.output {
		return Result{}, fmt.Errorf("%w: %s", ErrProtected, args.FilePath)
	}
	f, err := dest.open()
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if err := ws.checkRead(dest, f, args.FilePath); err != nil {
		return Result{}, err
	}
	if _, err := admit(dest.target); err != nil {
		return Result{}, err
	}
	content, err := readForEdit(f, args.FilePath)
	if err != nil {
		return Result{}, err
	}
	dest.original, err = f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("failed to stat %s: %w", args.FilePath, err)
	}
	dest.content = []byte(content)
	n := strings.Count(content, args.OldString)
	switch {
	case n == 0:
		return Result{}, fmt.Errorf("%w in %s", ErrNoMatch, args.FilePath)
	case n > 1 && !args.ReplaceAll:
		return Result{}, fmt.Errorf("%w in %s (%d matches); add context to old_string or set replace_all", ErrAmbiguousMatch, args.FilePath, n)
	}
	limit := 1
	if args.ReplaceAll {
		limit = -1
	}
	updated := strings.Replace(content, args.OldString, *args.NewString, limit)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := dest.write(ws, []byte(updated)); err != nil {
		return Result{}, err
	}
	if n == 1 {
		return Result{Text: "replaced 1 occurrence in " + args.FilePath}, nil
	}
	return Result{Text: fmt.Sprintf("replaced %d occurrences in %s", n, args.FilePath)}, nil
}

func readForEdit(f *os.File, path string) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrIsDirectory, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEditBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	if len(data) > maxEditBytes {
		return "", fmt.Errorf("%w: %s is over %d MiB", ErrTooLarge, path, maxEditBytes>>20)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%w: %s", ErrBinary, path)
	}
	return string(data), nil
}

// editChange runs before the arguments are validated, so it decodes leniently.
func editChange(raw json.RawMessage) (string, []PreviewLine) {
	var args struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	_ = json.Unmarshal(raw, &args)
	return fmt.Sprintf("+%d -%d", countLines(args.NewString), countLines(args.OldString)),
		append(changeLines(Removed, args.OldString), changeLines(Added, args.NewString)...)
}

// editPreview runs before the arguments are validated, so it decodes leniently.
func editPreview(raw json.RawMessage) []PreviewLine {
	var args struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	_ = json.Unmarshal(raw, &args)
	return append(previewLines(Removed, args.OldString), previewLines(Added, args.NewString)...)
}
