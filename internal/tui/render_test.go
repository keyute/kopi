package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		41 * time.Second:                          "41s",
		1500 * time.Millisecond:                   "1s",
		72 * time.Second:                          "1m 12s",
		time.Hour + 2*time.Minute + 5*time.Second: "1h 2m",
	} {
		assert.Equal(t, want, formatDuration(d))
	}
}
