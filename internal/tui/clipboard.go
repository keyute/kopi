package tui

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	atotto "github.com/atotto/clipboard"
)

// clipboardTimeout bounds each helper process reading a clipboard image.
const clipboardTimeout = 2 * time.Second

type clipboard interface {
	ReadText() (string, error)
	WriteText(string) error
	// ReadImage returns the clipboard's image as PNG, or nil when it holds
	// none.
	ReadImage(ctx context.Context) ([]byte, error)
}

type systemClipboard struct{}

func (systemClipboard) ReadText() (string, error) {
	text, err := atotto.ReadAll()
	if err != nil {
		return "", fmt.Errorf("failed to read the clipboard: %w", err)
	}
	return text, nil
}

func (systemClipboard) WriteText(text string) error {
	if err := atotto.WriteAll(text); err != nil {
		return fmt.Errorf("failed to write the clipboard: %w", err)
	}
	return nil
}

// ReadImage asks the platform's clipboard tool: osascript on macOS,
// wl-paste on Wayland, xclip on X11. Without one there is no image.
func (systemClipboard) ReadImage(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()
	switch {
	case runtime.GOOS == "darwin":
		out, err := clipboardTool(ctx, "osascript", "-e", "the clipboard as «class PNGf»")
		if err != nil || out == nil {
			return nil, err
		}
		return parsePNGf(out)
	case runtime.GOOS == "linux" && os.Getenv("WAYLAND_DISPLAY") != "":
		types, err := clipboardTool(ctx, "wl-paste", "--list-types")
		if err != nil || !listsPNG(types) {
			return nil, err
		}
		return clipboardTool(ctx, "wl-paste", "--type", "image/png")
	case runtime.GOOS == "linux" && os.Getenv("DISPLAY") != "":
		types, err := clipboardTool(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
		if err != nil || !listsPNG(types) {
			return nil, err
		}
		return clipboardTool(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	}
	return nil, nil
}

// clipboardTool runs a clipboard tool for its output. A tool that is missing
// or exits non-zero has nothing to give: nil output and no error.
func clipboardTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("failed to run %s: %w", name, ctx.Err())
	case errors.Is(err, exec.ErrNotFound), errors.As(err, &exit):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("failed to run %s: %w", name, err)
	}
	return out, nil
}

// parsePNGf decodes osascript's rendering of PNG data, «data PNGf<hex>».
// Empty output is no image.
func parsePNGf(out []byte) ([]byte, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	h, ok := strings.CutPrefix(s, "«data PNGf")
	if ok {
		h, ok = strings.CutSuffix(h, "»")
	}
	if !ok {
		return nil, fmt.Errorf("failed to parse the clipboard image: %w", ErrClipboardImage)
	}
	png, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the clipboard image: %w", err)
	}
	return png, nil
}

// listsPNG reports whether a tool's list of clipboard types, one per line,
// offers PNG.
func listsPNG(types []byte) bool {
	for l := range bytes.Lines(types) {
		if string(bytes.TrimSpace(l)) == "image/png" {
			return true
		}
	}
	return false
}

// readClipboard reads off the update loop, as the system clipboard runs
// helper processes. An image is saved to dir and arrives as an
// imagePastedMsg; otherwise the text arrives as a paste. An empty clipboard
// or a failed text read pastes nothing.
func readClipboard(ctx context.Context, cb clipboard, dir string) tea.Cmd {
	return func() tea.Msg {
		png, err := cb.ReadImage(ctx)
		if err == nil && png != nil {
			var path string
			if path, err = saveImage(dir, png); err == nil {
				return imagePastedMsg{path: path}
			}
		}
		if err != nil {
			slog.Warn("failed to paste an image from the clipboard", "err", err)
			return pasteFailedMsg{}
		}
		return clipboardText(cb)
	}
}

// readClipboardText is readClipboard for input that takes no images.
func readClipboardText(cb clipboard) tea.Cmd {
	return func() tea.Msg { return clipboardText(cb) }
}

func clipboardText(cb clipboard) tea.Msg {
	text, err := cb.ReadText()
	if err != nil {
		slog.Warn("failed to paste from the clipboard", "err", err)
		return nil
	}
	if text == "" {
		return nil
	}
	return tea.PasteMsg{Content: text}
}
