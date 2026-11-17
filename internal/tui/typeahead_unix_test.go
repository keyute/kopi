//go:build darwin || linux

package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAskTrustDiscardsTypeahead(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	master, slave, err := pty.Open()
	require.NoError(t, err)
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 30, Cols: 80}); err != nil {
		_ = slave.Close()
		_ = master.Close()
		require.NoError(t, err)
	}
	drawn, drained := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(drained)
		var output strings.Builder
		buf := make([]byte, 4096)
		seen := false
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			if !seen && strings.Contains(ansi.Strip(output.String()), "Do you trust this folder?") {
				seen = true
				close(drawn)
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		// Close the slave first: closing a master with a blocked read can hang on Darwin.
		_ = slave.Close()
		select {
		case <-drained:
		case <-time.After(waitLimit):
			t.Error("timed out cleaning up terminal drain")
			return
		}
		_ = master.Close()
	})

	// Typed before the prompt exists; it would pick "Yes, trust this folder".
	_, err = io.WriteString(master, "1")
	require.NoError(t, err)
	type result struct {
		c   TrustChoice
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := AskTrust(t.Context(), slave, slave, testOptions(), trustReq)
		done <- result{c, err}
	}()
	select {
	case <-drawn:
	case r := <-done:
		t.Fatalf("AskTrust returned %v, %v before the prompt was drawn", r.c, r.err)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for the trust prompt")
	}
	_, err = io.WriteString(master, "3")
	require.NoError(t, err)

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, TrustDeny, r.c)
	case <-time.After(waitLimit):
		t.Fatal("AskTrust did not return")
	}
}
