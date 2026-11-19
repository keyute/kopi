package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/keyute/kopi/internal/policy"
)

const (
	maxReadLines   = 2000
	maxLineChars   = 2000
	maxReadBytes   = 100 << 10
	binarySniffLen = 8 << 10
	// noticeReserve keeps room under maxReadBytes for the final notice line.
	noticeReserve = 256
	lineCutMarker = " [line truncated]"

	emptyFileNotice = "(empty file)"
	pastEndNotice   = "(offset "
	imageNotice     = "(image/"

	// OmittedImageNote ends the text of a result whose image could not be processed.
	OmittedImageNote = "[Image omitted: it could not be processed.]"
)

const MaxImageBytes = 20 << 20

var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// ImageType returns data's sniffed MIME type and whether it is an image a model takes.
func ImageType(data []byte) (string, bool) {
	mime := http.DetectContentType(data)
	return mime, slices.Contains(imageTypes, mime)
}

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   *int   `json:"offset"`
	Limit    *int   `json:"limit"`
}

func NewRead(ws *Workspace) Tool {
	return Tool{
		Name:  "Read",
		Label: "Read",
		Description: "Reads a text file or an image (PNG, JPEG, GIF or WebP) inside the workspace. " +
			"file_path is absolute or relative to the workspace. " +
			"A text file returns numbered lines, up to 2000 lines by default; use offset and limit to read other ranges. " +
			"An image is returned whole; offset and limit do not apply.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path of the file to read, absolute or relative to the workspace.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "1-based line number to start reading from.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Maximum number of lines to read (at most 2000).",
				},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},
		Capability: policy.Read,
		Target:     pathTarget(ws),
		Run: func(ctx context.Context, raw json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (Result, error) {
			args, err := decodeReadArgs(raw)
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
			return read(ctx, f, args)
		},
		Display: Display{
			Title:   readTitle,
			Summary: readSummary,
			Action:  Action{Verb: "Read", One: "file", Many: "files"},
		},
	}
}

// readTitle runs before the arguments are validated, so it decodes leniently.
func readTitle(raw json.RawMessage) string {
	var args readArgs
	_ = json.Unmarshal(raw, &args)
	if args.FilePath == "" {
		return "Read"
	}
	return "Read " + args.FilePath
}

// readSummary relies on read's output shape: every file line and the
// continuation notice end in "\n", only the notice starts with "[", and
// notices that replace file lines start with "(".
func readSummary(output string) string {
	switch {
	case output == emptyFileNotice:
		return "empty file"
	case strings.HasPrefix(output, pastEndNotice):
		return "past end of file"
	case strings.HasPrefix(output, imageNotice):
		if strings.Contains(output, OmittedImageNote) {
			return "image omitted"
		}
		return "image"
	}
	n := strings.Count(output, "\n")
	lastLine := output[strings.LastIndex(strings.TrimSuffix(output, "\n"), "\n")+1:]
	if strings.HasPrefix(lastLine, "[") {
		n--
	}
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// decodeArgs decodes one JSON object strictly: unknown fields and trailing
// data are refused.
func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return args, fmt.Errorf("%w: %w", ErrInvalidArgs, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return args, fmt.Errorf("%w: trailing data after arguments", ErrInvalidArgs)
	}
	return args, nil
}

// pathTarget returns a Tool.Target for a tool whose file_path argument names
// the file it acts on. It runs before the arguments are validated, so it
// decodes leniently.
func pathTarget(ws *Workspace) func(json.RawMessage) (policy.Target, error) {
	return func(raw json.RawMessage) (policy.Target, error) {
		var args struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(raw, &args)
		if args.FilePath == "" {
			return policy.Target{}, fmt.Errorf("%w: file_path is required", ErrInvalidArgs)
		}
		return ws.Target(args.FilePath)
	}
}

func decodeReadArgs(raw json.RawMessage) (readArgs, error) {
	args, err := decodeArgs[readArgs](raw)
	if err != nil {
		return args, err
	}
	if args.FilePath == "" {
		return args, fmt.Errorf("%w: file_path is required", ErrInvalidArgs)
	}
	if args.Offset != nil && *args.Offset < 1 {
		return args, fmt.Errorf("%w: offset must be at least 1", ErrInvalidArgs)
	}
	if args.Limit != nil && *args.Limit < 1 {
		return args, fmt.Errorf("%w: limit must be at least 1", ErrInvalidArgs)
	}
	return args, nil
}

func read(ctx context.Context, f *os.File, args readArgs) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	offset, limit := 1, maxReadLines
	if args.Offset != nil {
		offset = *args.Offset
	}
	if args.Limit != nil {
		limit = min(*args.Limit, maxReadLines)
	}

	info, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("failed to stat %s: %w", args.FilePath, err)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("%w: %s", ErrIsDirectory, args.FilePath)
	}

	r := bufio.NewReaderSize(f, 64<<10)
	head, err := r.Peek(binarySniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("failed to read %s: %w", args.FilePath, err)
	}
	// An empty head sniffs as text, so an empty file still gets its notice.
	if mime, ok := ImageType(head); ok {
		tooLarge := fmt.Errorf("%w: %s is over %d MiB", ErrTooLarge, args.FilePath, MaxImageBytes>>20)
		if info.Size() > MaxImageBytes {
			return Result{}, tooLarge
		}
		// The limit also catches a file that grew since Stat.
		data, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
		if err != nil {
			return Result{}, fmt.Errorf("failed to read %s: %w", args.FilePath, err)
		}
		if len(data) > MaxImageBytes {
			return Result{}, tooLarge
		}
		return Result{
			Text:   fmt.Sprintf("(%s, %d bytes)", mime, len(data)),
			Images: []Image{{MimeType: mime, Data: data}},
		}, nil
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return Result{}, fmt.Errorf("%w: %s", ErrBinary, args.FilePath)
	}
	if len(head) == 0 {
		return Result{Text: emptyFileNotice}, nil
	}

	var out strings.Builder
	lineNo, last := 0, 0
	more := false
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		line, cut, err := readLine(ctx, r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, fmt.Errorf("failed to read %s: %w", args.FilePath, err)
		}
		lineNo++
		if lineNo < offset {
			continue
		}
		if last-offset+1 >= limit {
			more = true
			break
		}
		formatted := fmt.Sprintf("%6d\t%s", lineNo, line)
		if cut {
			formatted += lineCutMarker
		}
		formatted += "\n"
		if out.Len()+len(formatted) > maxReadBytes-noticeReserve {
			more = true
			break
		}
		out.WriteString(formatted)
		last = lineNo
	}

	if last == 0 {
		return Result{Text: fmt.Sprintf(pastEndNotice+"%d is past the end of the file, which has %d lines)", offset, lineNo)}, nil
	}
	if more {
		fmt.Fprintf(&out, "[Showing lines %d-%d. More lines remain; use offset=%d to continue.]\n", offset, last, last+1)
	}
	return Result{Text: out.String()}, nil
}

// readLine returns the next line without its newline, keeping at most
// maxLineChars characters and discarding the rest so an unbounded line never
// sits in memory. cut reports whether characters were dropped. io.EOF is
// returned only when no further line exists.
func readLine(ctx context.Context, r *bufio.Reader) (line string, cut bool, err error) {
	// A character is at most utf8.UTFMax bytes, so this many bytes always
	// hold maxLineChars characters plus one to detect the overflow.
	const keep = (maxLineChars + 1) * utf8.UTFMax
	var buf []byte
	got := false
	for {
		chunk, err := r.ReadSlice('\n')
		got = got || len(chunk) > 0
		room := max(keep-len(buf), 0)
		buf = append(buf, chunk[:min(len(chunk), room)]...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if err := ctx.Err(); err != nil {
				return "", false, err
			}
			continue
		}
		if err != nil && (!errors.Is(err, io.EOF) || !got) {
			return "", false, err
		}
		break
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	n, chars := 0, 0
	for n < len(buf) && chars < maxLineChars {
		_, size := utf8.DecodeRune(buf[n:])
		n += size
		chars++
	}
	return string(buf[:n]), n < len(buf), nil
}
