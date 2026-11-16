package app

import (
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		line, name, args string
		isCommand        bool
	}{
		{line: "/plan", name: "plan", isCommand: true},
		{line: "/resume abc", name: "resume", args: "abc", isCommand: true},
		{line: "  /status  ", name: "status", isCommand: true},
		{line: "/frobnicate now", name: "frobnicate", args: "now", isCommand: true},
		{line: "//x"},
		{line: "explain this repo"},
		{line: ""},
	} {
		t.Run(tc.line, func(t *testing.T) {
			name, args, isCommand := parse(tc.line)
			assert.Equal(t, tc.name, name)
			assert.Equal(t, tc.args, args)
			assert.Equal(t, tc.isCommand, isCommand)
		})
	}
}

func TestArgumentsListsSessionsForResume(t *testing.T) {
	h, _ := openHarness(t, Config{})
	cs, err := h.c.Arguments("resume")
	require.NoError(t, err)
	assert.Empty(t, cs)

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})
	h.turn(t, "first line\nsecond line")
	cs, err = h.c.Arguments("resume")
	require.NoError(t, err)
	require.Len(t, cs, 1)
	assert.Equal(t, h.c.Status().SessionID, cs[0].Value)
	assert.Contains(t, cs[0].Description, "first line")
	assert.NotContains(t, cs[0].Description, "second line")

	for _, name := range []string{"plan", "nope"} {
		cs, err := h.c.Arguments(name)
		require.NoError(t, err)
		assert.Nil(t, cs, name)
	}
}
