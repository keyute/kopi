package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/keyute/kopi/internal/config"
)

// theme maps semantic roles to colours; call sites use roles, never hex.
type theme struct {
	accent, success, error, warning, muted, dim, text, userMessageBg, userCommandBg color.Color
	// heading, link and code colour assistant markdown; codeTheme names the
	// Chroma style for fenced code.
	heading, link, code color.Color
	codeTheme           string
	// codeFormatter is the Chroma formatter for fenced code: terminal16m for
	// hex colours, terminal16 to follow the terminal's palette.
	codeFormatter string
	// frame colours a dialog's rules.
	frame color.Color
}

func mocha() theme {
	return theme{
		accent:        lipgloss.Color("#cba6f7"),
		success:       lipgloss.Color("#a6e3a1"),
		error:         lipgloss.Color("#f38ba8"),
		warning:       lipgloss.Color("#f9e2af"),
		muted:         lipgloss.Color("#a6adc8"),
		dim:           lipgloss.Color("#6c7086"),
		text:          lipgloss.Color("#cdd6f4"),
		userMessageBg: lipgloss.Color("#313244"),
		userCommandBg: lipgloss.Color("#402f42"),
		heading:       lipgloss.Color("#fab387"),
		link:          lipgloss.Color("#89b4fa"),
		code:          lipgloss.Color("#94e2d5"),
		codeTheme:     "catppuccin-mocha",
		codeFormatter: "terminal16m",
		frame:         lipgloss.Color("#89b4fa"),
	}
}

func latte() theme {
	return theme{
		accent:        lipgloss.Color("#8839ef"),
		success:       lipgloss.Color("#40a02b"),
		error:         lipgloss.Color("#d20f39"),
		warning:       lipgloss.Color("#df8e1d"),
		muted:         lipgloss.Color("#6c6f85"),
		dim:           lipgloss.Color("#9ca0b0"),
		text:          lipgloss.Color("#4c4f69"),
		userMessageBg: lipgloss.Color("#ccd0da"),
		userCommandBg: lipgloss.Color("#ecd6de"),
		heading:       lipgloss.Color("#fe640b"),
		link:          lipgloss.Color("#1e66f5"),
		code:          lipgloss.Color("#179299"),
		codeTheme:     "catppuccin-latte",
		codeFormatter: "terminal16m",
		frame:         lipgloss.Color("#1e66f5"),
	}
}

// hexTheme builds a hex theme from its Chroma style and its colours in config.ThemeRoles order.
func hexTheme(codeTheme, accent, success, errc, warning, muted, dim, text, userBg, userCmdBg, heading, link, code, frame string) theme {
	return theme{
		accent: lipgloss.Color(accent), success: lipgloss.Color(success), error: lipgloss.Color(errc),
		warning: lipgloss.Color(warning), muted: lipgloss.Color(muted), dim: lipgloss.Color(dim),
		text: lipgloss.Color(text), userMessageBg: lipgloss.Color(userBg), userCommandBg: lipgloss.Color(userCmdBg),
		heading: lipgloss.Color(heading), link: lipgloss.Color(link), code: lipgloss.Color(code),
		codeTheme: codeTheme, codeFormatter: "terminal16m", frame: lipgloss.Color(frame),
	}
}

// ansiTheme maps roles to the terminal's own palette, so the user's terminal
// colours decide the look; text is the terminal default.
func ansiTheme(codeTheme, muted, dim, userBg string) theme {
	return theme{
		accent: lipgloss.Color("5"), success: lipgloss.Color("2"), error: lipgloss.Color("1"),
		warning: lipgloss.Color("3"), muted: lipgloss.Color(muted), dim: lipgloss.Color(dim),
		text: lipgloss.NoColor{}, userMessageBg: lipgloss.Color(userBg), userCommandBg: lipgloss.Color(userBg),
		heading: lipgloss.Color("3"), link: lipgloss.Color("4"), code: lipgloss.Color("6"),
		codeTheme: codeTheme, codeFormatter: "terminal16", frame: lipgloss.Color("4"),
	}
}

var builtinThemes = map[string]func() theme{
	"catppuccin-mocha": mocha,
	"catppuccin-latte": latte,
	// Catppuccin: mauve, green, red, yellow, subtext0, overlay0, text, surface0, peach, blue, teal.
	"catppuccin-frappe": func() theme {
		return hexTheme("catppuccin-frappe", "#ca9ee6", "#a6d189", "#e78284", "#e5c890", "#a5adce", "#737994", "#c6d0f5", "#414559", "#554c5e", "#ef9f76", "#8caaee", "#81c8be", "#8caaee")
	},
	"catppuccin-macchiato": func() theme {
		return hexTheme("catppuccin-macchiato", "#c6a0f6", "#a6da95", "#ed8796", "#eed49f", "#a5adcb", "#6e738d", "#cad3f5", "#363a4f", "#4c4358", "#f5a97f", "#8aadf4", "#8bd5ca", "#8aadf4")
	},
	// Gruvbox: bg1, fg, fg4/gray, red, green, yellow, purple, aqua, orange, blue.
	"gruvbox-dark": func() theme {
		return hexTheme("gruvbox", "#d3869b", "#b8bb26", "#fb4934", "#fabd2f", "#a89984", "#928374", "#ebdbb2", "#3c3836", "#533a36", "#fe8019", "#83a598", "#8ec07c", "#83a598")
	},
	"gruvbox-light": func() theme {
		return hexTheme("gruvbox-light", "#8f3f71", "#79740e", "#9d0006", "#b57614", "#7c6f64", "#928374", "#3c3836", "#ebdbb2", "#e2c19d", "#af3a03", "#076678", "#427b58", "#076678")
	},
	// Tokyo Night: purple, green, red, yellow, fg_dark, comment, fg, bg_highlight, orange, blue, cyan.
	"tokyonight-night": func() theme {
		return hexTheme("tokyonight-night", "#bb9af7", "#9ece6a", "#f7768e", "#e0af68", "#a9b1d6", "#565f89", "#c0caf5", "#292e42", "#42374b", "#ff9e64", "#7aa2f7", "#7dcfff", "#7aa2f7")
	},
	"tokyonight-day": func() theme {
		return hexTheme("tokyonight-day", "#9854f1", "#587539", "#f52a65", "#8c6c3e", "#6172b0", "#848cb5", "#3760bf", "#c4c8da", "#cab5cc", "#b15c00", "#2e7de9", "#007197", "#2e7de9")
	},
	// Rosé Pine: iris, foam/pine, love, gold, subtle, muted, text, overlay, rose.
	"rose-pine": func() theme {
		return hexTheme("rose-pine", "#c4a7e7", "#9ccfd8", "#eb6f92", "#f6c177", "#908caa", "#6e6a86", "#e0def4", "#26233a", "#3e2c45", "#ebbcba", "#9ccfd8", "#31748f", "#9ccfd8")
	},
	"rose-pine-dawn": func() theme {
		return hexTheme("rose-pine-dawn", "#907aa9", "#286983", "#b4637a", "#ea9d34", "#797593", "#9893a5", "#575279", "#f2e9e1", "#ebd9d5", "#d7827e", "#286983", "#56949f", "#286983")
	},
	// Solarized: violet, green, red, yellow, base1/base01, base01/base1, base0/base00, base02/base2, orange, blue, cyan.
	"solarized-dark": func() theme {
		return hexTheme("solarized-dark", "#6c71c4", "#859900", "#dc322f", "#b58900", "#93a1a1", "#586e75", "#839496", "#073642", "#213640", "#cb4b16", "#268bd2", "#2aa198", "#268bd2")
	},
	"solarized-light": func() theme {
		return hexTheme("solarized-light", "#6c71c4", "#859900", "#dc322f", "#b58900", "#586e75", "#93a1a1", "#657b83", "#eee8d5", "#ecd2c1", "#cb4b16", "#268bd2", "#2aa198", "#268bd2")
	},
	// Dracula has no subtext shade, so muted is a lightened comment colour.
	"dracula": func() theme {
		return hexTheme("dracula", "#bd93f9", "#50fa7b", "#ff5555", "#f1fa8c", "#9aa5ce", "#6272a4", "#f8f8f2", "#44475a", "#5a4959", "#ffb86c", "#8be9fd", "#ff79c6", "#8be9fd")
	},
	"ansi-dark":  func() theme { return ansiTheme("native", "7", "8", "0") },
	"ansi-light": func() theme { return ansiTheme("xcode", "0", "8", "7") },
}

// Themes are the dark and light themes a session draws with; build them with
// LoadThemes.
type Themes struct{ dark, light theme }

// themeFile is a custom theme: a built-in theme with some roles and the
// code style replaced.
type themeFile struct {
	Base      string            `json:"base"`
	Colors    map[string]string `json:"colors"`
	CodeTheme string            `json:"codeTheme"`
}

// LoadThemes resolves the dark and light theme names: a built-in name is the
// built-in, any other is dir/<name>.json.
func LoadThemes(dir, dark, light string) (Themes, error) {
	d, err := loadTheme(dir, dark)
	if err != nil {
		return Themes{}, fmt.Errorf("failed to load theme %q: %w", dark, err)
	}
	l, err := loadTheme(dir, light)
	if err != nil {
		return Themes{}, fmt.Errorf("failed to load theme %q: %w", light, err)
	}
	return Themes{dark: d, light: l}, nil
}

func loadTheme(dir, name string) (theme, error) {
	if b, ok := builtinThemes[name]; ok {
		return b(), nil
	}
	path := filepath.Join(dir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return theme{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var f themeFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return theme{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	build, ok := builtinThemes[f.Base]
	if !ok {
		return theme{}, fmt.Errorf("%s: unknown base %q", path, f.Base)
	}
	th := build()
	roles := th.roles()
	for role, value := range f.Colors {
		slot, ok := roles[role]
		if !ok {
			return theme{}, fmt.Errorf("%s: unknown colour role %q", path, role)
		}
		if err := config.ValidLiteralColor(value); err != nil {
			return theme{}, fmt.Errorf("%s: colour %q: %w", path, role, err)
		}
		*slot = lipgloss.Color(value)
	}
	if f.CodeTheme != "" {
		if _, ok := styles.Registry[f.CodeTheme]; !ok {
			return theme{}, fmt.Errorf("%s: unknown codeTheme %q", path, f.CodeTheme)
		}
		th.codeTheme = f.CodeTheme
	}
	return th, nil
}

// roles addresses each colour role by its config.ThemeRoles name.
func (t *theme) roles() map[string]*color.Color {
	return map[string]*color.Color{
		"accent": &t.accent, "success": &t.success, "error": &t.error, "warning": &t.warning,
		"muted": &t.muted, "dim": &t.dim, "text": &t.text, "userMessageBg": &t.userMessageBg, "userCommandBg": &t.userCommandBg,
		"heading": &t.heading, "link": &t.link, "code": &t.code, "frame": &t.frame,
	}
}

func fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
