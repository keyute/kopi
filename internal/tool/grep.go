package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/keyute/kopi/internal/policy"
)

const (
	defaultGrepResults = 100
	maxGrepResults     = 1000
	maxGrepContext     = 10

	grepFiles   = "files_with_matches"
	grepContent = "content"
	grepCount   = "count"
)

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Context         *int   `json:"context"`
	Limit           *int   `json:"limit"`
}

func NewGrep(ws *Workspace, hidden Hidden) Tool {
	if hidden == nil {
		hidden = func(string, policy.Target, policy.Target) bool { return false }
	}
	return Tool{
		Name:  "Grep",
		Label: "Grep",
		Description: "Searches file contents inside the workspace with a regular expression (RE2 syntax). " +
			"path is a file or directory, absolute or relative to the workspace (default the workspace). " +
			"Skips .git directories below path, files .gitignore ignores, binary files and symlinks. " +
			"output_mode is files_with_matches (paths, most recently modified first; the default), " +
			"content (lines as path:line:text, with path-line-text for context) " +
			"or count (path:count, most recently modified first). " +
			"Returns at most limit results, 100 by default.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regular expression to search for (RE2 syntax).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "File or directory to search, absolute or relative to the workspace; defaults to the workspace.",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "Only search files whose path below path matches this glob, e.g. *.go or src/**/*.ts; a pattern without / matches file names at any depth.",
				},
				"output_mode": map[string]any{
					"type":        "string",
					"enum":        []string{grepFiles, grepContent, grepCount},
					"description": "What to return; default files_with_matches.",
				},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Match regardless of letter case.",
				},
				"context": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Lines to show before and after each match in content mode (at most 10).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Maximum number of results: files, or matching lines in content mode (default 100, at most 1000).",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Target:     searchTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeArgs[grepArgs](raw)
			if err != nil {
				return Result{}, err
			}
			re, match, err := compileGrep(args)
			if err != nil {
				return Result{}, err
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if args.Path == "" {
				args.Path = "."
			}
			root, rel, info, err := ws.searchRoot(args.Path)
			if err != nil {
				return Result{}, err
			}
			if _, err := admit(root); err != nil {
				return Result{}, err
			}
			return grep(ctx, ws, hidden, args, re, match, root, rel, info)
		},
		Display: Display{
			Title:   searchTitle("Grep"),
			Summary: foundSummary,
			Action:  Action{Verb: "Searched for", One: "pattern", Many: "patterns"},
		},
	}
}

// compileGrep validates args and compiles its pattern and glob.
func compileGrep(args grepArgs) (*regexp.Regexp, func(string) bool, error) {
	if args.Pattern == "" {
		return nil, nil, fmt.Errorf("%w: pattern is required", ErrInvalidArgs)
	}
	pattern := args.Pattern
	if args.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidArgs, err)
	}
	match, err := compileGlob(args.Glob)
	if err != nil {
		return nil, nil, err
	}
	switch args.OutputMode {
	case "", grepFiles, grepContent, grepCount:
	default:
		return nil, nil, fmt.Errorf("%w: unknown output_mode %q", ErrInvalidArgs, args.OutputMode)
	}
	if args.Context != nil && *args.Context < 0 {
		return nil, nil, fmt.Errorf("%w: context must be at least 0", ErrInvalidArgs)
	}
	if args.Limit != nil && *args.Limit < 1 {
		return nil, nil, fmt.Errorf("%w: limit must be at least 1", ErrInvalidArgs)
	}
	return re, match, nil
}

type grepLine struct {
	n    int
	text string
	cut  bool
}

func grep(ctx context.Context, ws *Workspace, hidden Hidden, args grepArgs, re *regexp.Regexp, match func(string) bool, root policy.Target, rel string, info fs.FileInfo) (Result, error) {
	mode := args.OutputMode
	if mode == "" {
		mode = grepFiles
	}
	limit, around := defaultGrepResults, 0
	if args.Limit != nil {
		limit = min(*args.Limit, maxGrepResults)
	}
	if args.Context != nil {
		around = min(*args.Context, maxGrepContext)
	}

	r := bufio.NewReaderSize(nil, 64<<10)
	// scan feeds each line of a text file to onLine until it returns false.
	// A binary or unreadable file yields no lines.
	scan := func(f *os.File, onLine func(l grepLine) bool) {
		defer f.Close()
		r.Reset(f)
		head, err := r.Peek(binarySniffLen)
		if err != nil && !errors.Is(err, io.EOF) || bytes.IndexByte(head, 0) >= 0 {
			return
		}
		for n := 1; ctx.Err() == nil; n++ {
			text, cut, err := readLine(ctx, r)
			if err != nil || !onLine(grepLine{n, text, cut}) {
				return
			}
		}
	}

	var (
		kept           []found
		matches, files int
		out            strings.Builder
		more, limited  bool
		printed        bool
	)
	// searchFile reports whether the search goes on past p.
	searchFile := func(p string, f *os.File, fi fs.FileInfo) bool {
		switch mode {
		case grepFiles:
			hit := false
			scan(f, func(l grepLine) bool {
				hit = re.MatchString(l.text)
				return !hit
			})
			if hit {
				files++
				kept = keepNewest(kept, found{path: p, modTime: fi.ModTime()}, limit)
			}
		case grepCount:
			n := 0
			scan(f, func(l grepLine) bool {
				if re.MatchString(l.text) {
					n++
				}
				return true
			})
			if n > 0 {
				matches += n
				files++
				kept = keepNewest(kept, found{path: p, modTime: fi.ModTime(), count: n}, limit)
			}
		case grepContent:
			var before []grepLine
			last, after, counted := 0, 0, false
			emit := func(l grepLine, sep string) bool {
				s := p + sep + fmt.Sprint(l.n) + sep + l.text
				if l.cut {
					s += lineCutMarker
				}
				s += "\n"
				if around > 0 && printed && (last == 0 || l.n != last+1) {
					s = "--\n" + s
				}
				if out.Len()+len(s) > maxReadBytes-noticeReserve {
					more = true
					return false
				}
				out.WriteString(s)
				printed, last = true, l.n
				return true
			}
			scan(f, func(l grepLine) bool {
				if re.MatchString(l.text) {
					if matches == limit {
						more, limited = true, true
						return false
					}
					for _, b := range before {
						if !emit(b, "-") {
							return false
						}
					}
					before = before[:0]
					if !emit(l, ":") {
						return false
					}
					matches++
					if !counted {
						counted = true
						files++
					}
					after = around
					return true
				}
				if after > 0 {
					after--
					return emit(l, "-")
				}
				if around > 0 {
					if len(before) == around {
						before = slices.Delete(before, 0, 1)
					}
					before = append(before, l)
				}
				return true
			})
		}
		return !more
	}

	if info.Mode().IsRegular() {
		f, err := ws.openWalked(rel, info)
		if err != nil {
			return Result{}, err
		}
		searchFile(rel, f, info)
	} else {
		err := ws.walk(ctx, rel, info, func(p string, entry fs.FileInfo) bool {
			if !entry.Mode().IsRegular() {
				return true
			}
			sub, t := under(root, rel, p)
			if !match(sub) || hidden("Grep", root, t) {
				return true
			}
			f, err := ws.openWalked(p, entry)
			if err != nil {
				return true
			}
			return searchFile(p, f, entry)
		})
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	if files == 0 {
		return Result{Text: "No matches found"}, nil
	}
	var res strings.Builder
	if mode == grepContent {
		fmt.Fprintf(&res, "Found %s in %s\n", grepNoun(matches, "match", "matches"), grepNoun(files, "file", "files"))
		res.WriteString(out.String())
		if more {
			raise := ""
			if limited && limit < maxGrepResults {
				raise = ", or a higher limit,"
			}
			fmt.Fprintf(&res, "[Showing %d matching lines; more match. A narrower path, glob or pattern%s lists them.]\n", matches, raise)
		}
		return Result{Text: res.String()}, nil
	}
	slices.SortFunc(kept, newestFirst)
	kept = kept[:min(len(kept), limit)]
	if mode == grepCount {
		fmt.Fprintf(&res, "Found %s in %s\n", grepNoun(matches, "match", "matches"), grepNoun(files, "file", "files"))
	} else {
		fmt.Fprintf(&res, "Found %s\n", grepNoun(files, "file", "files"))
	}
	for _, k := range kept {
		res.WriteString(k.path)
		if mode == grepCount {
			fmt.Fprintf(&res, ":%d", k.count)
		}
		res.WriteString("\n")
	}
	if files > len(kept) {
		fmt.Fprintf(&res, "[Showing %d of %d files, most recently modified first; a narrower path, glob or pattern lists the others.]\n", len(kept), files)
	}
	return Result{Text: res.String()}, nil
}

func grepNoun(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
