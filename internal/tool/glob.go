package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const maxGlobResults = 100

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func NewGlob(ws *Workspace, hidden Hidden) Tool {
	return Tool{
		Name:  "Glob",
		Label: "Glob",
		Description: "Finds files inside the workspace whose path matches a glob, such as **/*.go or src/**/*.ts; " +
			"a pattern without / matches file names at any depth. " +
			"Skips .git directories below path, files .gitignore ignores and symlinks. " +
			"Returns up to 100 paths, most recently modified first.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob matched against paths relative to path: * and ? within a name, ** across directories, {a,b} alternatives.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search, absolute or relative to the workspace; defaults to the workspace.",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Target:     searchTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[globArgs](raw)
			if err != nil {
				return Result{}, err
			}
			if args.Pattern == "" {
				return Result{}, fmt.Errorf("%w: pattern is required", ErrInvalidArgs)
			}
			match, err := compileGlob(args.Pattern)
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
			var kept []found
			total := 0
			err = ws.walk(ctx, rel, info, func(p string, info fs.FileInfo) bool {
				if !info.Mode().IsRegular() {
					return true
				}
				sub, t := under(root, rel, p)
				if !match(sub) || hidden != nil && hidden("Glob", root, t) {
					return true
				}
				total++
				kept = keepNewest(kept, found{path: p, modTime: info.ModTime()}, maxGlobResults)
				return true
			})
			if err != nil {
				return Result{}, err
			}
			return Result{Text: globOutput(kept, total)}, nil
		},
		Display: Display{
			Title:   searchTitle(ws, "Glob", false),
			Done:    searchTitle(ws, "Searched for", false),
			Summary: foundSummary,
			Body:    foundBody,
			Action:  Action{Verb: "Searched for", One: "pattern", Many: "patterns"},
		},
	}
}

func globOutput(kept []found, total int) string {
	if total == 0 {
		return "No files found"
	}
	slices.SortFunc(kept, newestFirst)
	kept = kept[:min(len(kept), maxGlobResults)]
	head := fmt.Sprintf("Found %d files", total)
	if total == 1 {
		head = "Found 1 file"
	}
	lines := []string{head}
	for _, f := range kept {
		lines = append(lines, f.path)
	}
	if total > len(kept) {
		lines = append(lines, fmt.Sprintf("[Showing %d of %d files, most recently modified first; a narrower path or pattern lists the others.]", len(kept), total))
	}
	return strings.Join(lines, "\n")
}
