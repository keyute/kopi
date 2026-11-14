package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestHeaderNamesVersion(t *testing.T) {
	for version, want := range map[string]string{"v1.2.3": "  kopi v1.2.3", "dev": "  kopi dev"} {
		ls := header(version, mocha())
		assert.Equal(t, want, ansi.Strip(ls[len(ls)-1].text))
		assert.Equal(t, 1, strings.Count(ansi.Strip(ls[len(ls)-1].text), "v"), version)
	}
}
