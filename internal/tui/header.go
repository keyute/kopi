package tui

// The art is the cup drawn in three parts so each takes its own role.
var (
	steamArt = []string{
		"        ▄ ▄",
		"       ▀ ▀",
	}
	cupArt = []string{
		"   ████████████▄▄",
		"   ████████████  █",
		"   ████████████▄▄▀",
		"    ██████████",
	}
	saucerArt = "  ▀▀▀▀▀▀▀▀▀▀▀▀▀▀"
)

// header is the transcript's opening lines: the art, a blank, then the name
// and version two in.
func header(version string, th theme) []line {
	var out []line
	for _, s := range steamArt {
		out = append(out, line{text: fg(th.dim).Render(s)})
	}
	for _, s := range cupArt {
		out = append(out, line{text: fg(th.accent).Render(s)})
	}
	out = append(out,
		line{text: fg(th.dim).Render(saucerArt)},
		line{},
		line{text: "  " + fg(th.accent).Bold(true).Render("kopi") + " " + fg(th.dim).Render(version)},
	)
	return out
}
