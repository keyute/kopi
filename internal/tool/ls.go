package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const (
	maxLSEntries = 500

	emptyDirNotice = "(empty directory)"
)

type lsArgs struct {
	Path string `json:"path"`
}

func NewLS(ws *Workspace, hidden Hidden) Tool {
	return Tool{
		Name:  "LS",
		Label: "LS",
		Description: "Lists one directory inside the workspace, sorted by name, directories ending in /. " +
			"path is absolute or relative to the workspace (default the workspace). " +
			"Includes hidden and .gitignored entries; symlinks are listed, not followed, except one whose destination cannot be resolved. " +
			"Returns up to 500 entries.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to list, absolute or relative to the workspace; defaults to the workspace.",
				},
			},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Target:     searchTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[lsArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if args.Path == "" {
				args.Path = "."
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			root, rel, info, err := ws.searchRoot(args.Path)
			if err != nil {
				return Result{}, err
			}
			if !info.IsDir() {
				return Result{}, fmt.Errorf("%w: %s", ErrNotDirectory, args.Path)
			}
			if _, err := admit(root); err != nil {
				return Result{}, err
			}
			entries, err := ws.readDir(rel, info)
			if err != nil {
				return Result{}, fmt.Errorf("failed to list %s: %w", args.Path, err)
			}
			stores, err := ws.statProtected()
			if err != nil {
				return Result{}, err
			}
			// It may have become a store while admit waited for approval.
			if isStore(stores, info) {
				return Result{}, fmt.Errorf("%w: %s", ErrProtected, args.Path)
			}
			var names []string
			for _, d := range entries {
				info, err := d.Info()
				if err != nil || isStore(stores, info) {
					continue
				}
				p := path.Join(rel, d.Name())
				_, t := under(root, rel, p)
				name := d.Name()
				switch {
				case info.Mode()&fs.ModeSymlink != 0:
					// Judged by where the link leads, so a rule on its
					// destination hides it; unresolvable links fail closed.
					dest, err := ws.Target(filepath.Join(ws.realDir, filepath.FromSlash(p)))
					if err != nil {
						continue
					}
					t.Real = dest.Real
				case info.IsDir():
					name += "/"
				}
				if hidden != nil && hidden("LS", root, t) {
					continue
				}
				names = append(names, name)
			}
			return Result{Text: lsOutput(names)}, nil
		},
		Display: Display{
			Title:   lsTitle,
			Summary: lsSummary,
			Action:  Action{Verb: "Listed", One: "directory", Many: "directories"},
		},
	}
}

func lsOutput(names []string) string {
	if len(names) == 0 {
		return emptyDirNotice
	}
	if len(names) <= maxLSEntries {
		return strings.Join(names, "\n")
	}
	return strings.Join(names[:maxLSEntries], "\n") +
		fmt.Sprintf("\n[Showing %d of %d entries.]", maxLSEntries, len(names))
}

// lsTitle runs before the arguments are validated, so it decodes leniently.
func lsTitle(raw json.RawMessage) string {
	var args lsArgs
	_ = json.Unmarshal(raw, &args)
	if args.Path == "" {
		return "LS"
	}
	return "LS " + args.Path
}

// lsSummary relies on lsOutput's shape: one entry per line, and a notice line
// only after a list cut to maxLSEntries. A name may itself start with "[", so
// the notice is recognised by the line count.
func lsSummary(output string) string {
	if output == emptyDirNotice {
		return "empty"
	}
	lines := strings.Split(output, "\n")
	n := len(lines)
	if n > maxLSEntries {
		n--
	}
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}
