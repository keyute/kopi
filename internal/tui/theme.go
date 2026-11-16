package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// theme maps semantic roles to colours; call sites use roles, never hex.
type theme struct {
	accent, success, error, warning, muted, dim, text, userMessageBg color.Color
	// heading, link and code colour assistant markdown; codeTheme names the
	// Chroma style for fenced code.
	heading, link, code color.Color
	codeTheme           string
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
		heading:       lipgloss.Color("#fab387"),
		link:          lipgloss.Color("#89b4fa"),
		code:          lipgloss.Color("#94e2d5"),
		codeTheme:     "catppuccin-mocha",
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
		heading:       lipgloss.Color("#fe640b"),
		link:          lipgloss.Color("#1e66f5"),
		code:          lipgloss.Color("#179299"),
		codeTheme:     "catppuccin-latte",
		frame:         lipgloss.Color("#1e66f5"),
	}
}

func fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
