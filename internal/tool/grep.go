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
	"unicode/utf8"

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
			var (
				root  policy.Target
				rel   string
				info  fs.FileInfo
				saved *os.File
			)
			if _, ok := ws.outputs.holds(ws.absolute(args.Path)); ok {
				// A saved output file is opened the way Read opens it, and is the
				// whole search: the walk never sees Kopi's home.
				dest, err := ws.prepare(args.Path)
				if err != nil {
					return Result{}, err
				}
				defer func() { dest.parent.Close() }()
				if saved, err = dest.open(); err != nil {
					return Result{}, err
				}
				defer saved.Close()
				if err := ws.checkRead(dest, saved, args.Path); err != nil {
					return Result{}, err
				}
				if info, err = saved.Stat(); err != nil {
					return Result{}, fmt.Errorf("failed to stat %s: %w", args.Path, err)
				}
				if !info.Mode().IsRegular() {
					return Result{}, fmt.Errorf("%w: %s", ErrNotRegular, args.Path)
				}
				root, rel = dest.target, dest.target.Path
			} else if root, rel, info, err = ws.searchRoot(args.Path); err != nil {
				return Result{}, err
			}
			if _, err := admit(root); err != nil {
				return Result{}, err
			}
			return grep(ctx, ws, hidden, args, re, match, root, rel, info, saved)
		},
		Display: Display{
			Title:   searchTitle(ws, "Grep", false),
			Done:    searchTitle(ws, "Searched for", true),
			Summary: foundSummary,
			Body:    foundBody,
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
	n       int
	text    string
	cut     bool
	matched bool
}

// grepLineReader exposes one logical line to regexp, retaining only its display
// prefix. LF ends the line; CR remains part of both the pattern input and text.
type grepLineReader struct {
	ctx     context.Context
	r       *bufio.Reader
	display bool
	text    strings.Builder
	chars   int
	cut     bool
	got     bool
	done    bool
	err     error
}

func (l *grepLineReader) ReadRune() (rune, int, error) {
	if l.done {
		return 0, 0, io.EOF
	}
	if err := l.ctx.Err(); err != nil {
		l.done, l.err = true, err
		return 0, 0, err
	}
	var first byte
	if l.display && l.chars < maxLineChars {
		if head, _ := l.r.Peek(1); len(head) > 0 {
			first = head[0]
		}
	}
	r, size, err := l.r.ReadRune()
	if err != nil {
		l.done, l.err = true, err
		return 0, 0, err
	}
	l.got = true
	if r == '\n' {
		l.done = true
		return 0, 0, io.EOF
	}
	if l.display && l.chars < maxLineChars {
		if r == utf8.RuneError && size == 1 {
			// Matching decodes invalid UTF-8, but displayed text keeps its bytes.
			l.text.WriteByte(first)
		} else {
			l.text.WriteRune(r)
		}
		l.chars++
	} else {
		l.cut = true
	}
	return r, size, nil
}

func readGrepLine(ctx context.Context, r *bufio.Reader, re *regexp.Regexp, mode string) (grepLine, error) {
	l := grepLineReader{ctx: ctx, r: r, display: mode == grepContent}
	matched := re.MatchReader(&l)
	if matched && mode == grepFiles && l.got && l.err == nil {
		return grepLine{matched: true}, ctx.Err()
	}
	// Finish the display prefix before draining the tail in buffered chunks.
	for !l.done && l.display && l.chars < maxLineChars {
		_, _, _ = l.ReadRune()
	}
	for !l.done {
		if err := ctx.Err(); err != nil {
			return grepLine{}, err
		}
		tail, err := r.ReadSlice('\n')
		if len(tail) > 0 {
			l.got = true
			if len(tail) > 1 || tail[0] != '\n' {
				l.cut = true
			}
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			l.done, l.err = true, err
		}
	}
	if err := ctx.Err(); err != nil {
		return grepLine{}, err
	}
	if l.err != nil && !errors.Is(l.err, io.EOF) {
		return grepLine{}, l.err
	}
	if !l.got {
		return grepLine{}, io.EOF
	}
	return grepLine{text: l.text.String(), cut: l.cut, matched: matched}, nil
}

func grep(ctx context.Context, ws *Workspace, hidden Hidden, args grepArgs, re *regexp.Regexp, match func(string) bool, root policy.Target, rel string, info fs.FileInfo, saved *os.File) (Result, error) {
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
	// A binary or unreadable file yields no lines; a read failure is kept in
	// readErr for the caller that wants it.
	var readErr error
	scan := func(f *os.File, onLine func(l grepLine) bool) {
		defer f.Close()
		readErr = nil
		r.Reset(f)
		head, err := r.Peek(binarySniffLen)
		if err != nil && !errors.Is(err, io.EOF) {
			readErr = err
			return
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return
		}
		for n := 1; ctx.Err() == nil; n++ {
			line, err := readGrepLine(ctx, r, re, mode)
			line.n = n
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					readErr = err
				}
				return
			}
			if !onLine(line) {
				return
			}
		}
	}

	var (
		kept           []found
		matches, files int
		out            strings.Builder
		more, limited  bool
		capped         bool
		printed        bool
		contextTrimmed bool
	)
	// searchFile reports whether the search goes on past p.
	searchFile := func(p string, f *os.File, fi fs.FileInfo) bool {
		switch mode {
		case grepFiles:
			hit := false
			scan(f, func(l grepLine) bool {
				hit = l.matched
				return !hit
			})
			if hit {
				files++
				kept = keepNewest(kept, found{path: p, modTime: fi.ModTime()}, limit)
			}
		case grepCount:
			n := 0
			scan(f, func(l grepLine) bool {
				if l.matched {
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
					// A match that does not fit is a match not shown; a context
					// line that does not fit says nothing about later matches.
					capped = true
					more = more || sep == ":"
					return false
				}
				out.WriteString(s)
				printed, last = true, l.n
				return true
			}
			scan(f, func(l grepLine) bool {
				if l.matched {
					if matches == limit {
						more, limited = true, true
						return false
					}
					// Reserve the match and a possible group separator before context.
					needed := len(p) + 2 + len(fmt.Sprint(l.n)) + len(l.text) + 1 + 3
					if l.cut {
						needed += len(lineCutMarker)
					}
					for _, b := range before {
						needed += len(p) + 2 + len(fmt.Sprint(b.n)) + len(b.text) + 1
						if b.cut {
							needed += len(lineCutMarker)
						}
					}
					for len(before) > 0 && out.Len()+needed > maxReadBytes-noticeReserve {
						b := before[0]
						needed -= len(p) + 2 + len(fmt.Sprint(b.n)) + len(b.text) + 1
						if b.cut {
							needed -= len(lineCutMarker)
						}
						before = before[1:]
						contextTrimmed = true
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
		return !more && !capped
	}

	if info.Mode().IsRegular() {
		f := saved
		if f == nil {
			var err error
			if f, err = ws.openWalked(rel, info); err != nil {
				return Result{}, err
			}
		}
		searchFile(rel, f, info)
		if readErr != nil {
			return Result{}, fmt.Errorf("failed to read %s: %w", rel, readErr)
		}
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
		if contextTrimmed {
			res.WriteString("[Some preceding context omitted to fit output.]\n")
		}
		if more {
			raise := ""
			if limited && limit < maxGrepResults {
				raise = ", or a higher limit,"
			}
			fmt.Fprintf(&res, "[Showing %d matching lines; more match. A narrower path, glob or pattern%s lists them.]\n", matches, raise)
		} else if capped {
			fmt.Fprintf(&res, "[Showing %d matching lines; output cut at the size limit. A narrower path, glob or pattern shows the rest.]\n", matches)
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
