package tui

// The art is the mascot, a bag of iced kopi, drawn in parts so each takes its
// own role.
var (
	stringArt = []string{
		"        ▗▛▜▖",
		"        ▝▙▟▘",
		"        ▀██▀▀▀▀▄▄",
	}
	bagArt   = "       ▗▞▀▀▚▖"
	iceLeft  = "     ▗▞▘"
	iceArt   = "▄▖▗▖"
	iceRight = "▝▚▖"
	kopiArt  = []string{
		"   ▗▟██████████▙▖",
		"   ███▚▞████▚▞███",
		"   ▝▜████▙▟████▛▘",
	}
)

// header is the transcript's opening lines: the art, a blank, then the name
// and version two in.
func header(version string, th theme) []line {
	var out []line
	for _, s := range stringArt {
		out = append(out, line{text: fg(th.accent).Render(s)})
	}
	out = append(out,
		line{text: fg(th.dim).Render(bagArt)},
		line{text: fg(th.dim).Render(iceLeft) + fg(th.link).Render(iceArt) + fg(th.dim).Render(iceRight)},
	)
	for _, s := range kopiArt {
		out = append(out, line{text: fg(th.heading).Render(s)})
	}
	out = append(out,
		line{},
		line{text: "  " + fg(th.accent).Bold(true).Render("kopi") + " " + fg(th.dim).Render(version)},
	)
	return out
}
