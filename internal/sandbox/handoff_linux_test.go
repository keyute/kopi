package sandbox

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestListenHelper stands in for Kopi in a filtered command's namespace: it
// becomes the command after the arguments.
func TestListenHelper(t *testing.T) {
	if os.Getenv(listenHelperEnv) == "" {
		return
	}
	err := ExecListening(flag.Args())
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// handoffLimit only guards against a hung test; no test relies on it elapsing.
const handoffLimit = 30 * time.Second

func newHandoff(t *testing.T, allow []string, upstream string) *handoff {
	t.Helper()
	p, err := newProxy(allow, nil, nil, dialTo(upstream))
	require.NoError(t, err)
	h, err := p.handoff()
	require.NoError(t, err)
	return h
}

func waitEnded(t *testing.T, h *handoff) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(handoffLimit):
		t.Fatal("timed out waiting for the handoff to end")
	}
}

// send passes fds to the host over the child end, as the helper does.
func send(t *testing.T, h *handoff, fds ...int) {
	t.Helper()
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	require.NoError(t, unix.Sendmsg(int(h.child.Fd()), []byte{0}, oob, nil, 0))
}

func TestHandoffServesTheReceivedListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "from upstream")
	}))
	t.Cleanup(upstream.Close)
	h := newHandoff(t, []string{"allowed.test"}, upstream.Listener.Addr().String())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	f, err := l.(*net.TCPListener).File()
	require.NoError(t, err)
	defer f.Close()

	send(t, h, int(f.Fd()))
	var ack [1]byte
	_, err = io.ReadFull(h.child, ack[:])
	require.NoError(t, err)

	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "http", Host: l.Addr().String()}),
		DisableKeepAlives: true,
	}}
	resp, err := client.Get("http://allowed.test/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "from upstream", string(body))
	resp, err = client.Get("http://blocked.test/")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	require.NoError(t, h.child.Close())
	assert.Equal(t, []string{"blocked.test:80"}, h.close())
}

func TestHandoffEndsWithoutAMessage(t *testing.T) {
	h := newHandoff(t, nil, "")

	require.NoError(t, h.child.Close())
	waitEnded(t, h)

	assert.Nil(t, h.session)
	assert.Nil(t, h.close())
}

func TestHandoffCloseReturnsWhenNothingWasSent(t *testing.T) {
	h := newHandoff(t, nil, "")
	require.NoError(t, h.child.Close())
	closed := make(chan []string)

	go func() { closed <- h.close() }()

	select {
	case refused := <-closed:
		assert.Nil(t, refused)
	case <-time.After(handoffLimit):
		t.Fatal("timed out waiting for close")
	}
}

func TestHandoffRejectsWhatIsNotOneTCPListener(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcp.Close() })
	tcpFile, err := tcp.(*net.TCPListener).File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcpFile.Close() })
	// Socket paths are short, which t.TempDir() may not be.
	dir, err := os.MkdirTemp("/tmp", "kopi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	uds, err := net.Listen("unix", filepath.Join(dir, "s"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = uds.Close() })
	udsFile, err := uds.(*net.UnixListener).File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = udsFile.Close() })

	for _, tc := range []struct {
		name string
		fds  []int
	}{
		{"no descriptor", nil},
		{"two descriptors", []int{int(tcpFile.Fd()), int(tcpFile.Fd())}},
		{"a Unix listener", []int{int(udsFile.Fd())}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandoff(t, nil, "")

			send(t, h, tc.fds...)
			waitEnded(t, h)

			assert.Nil(t, h.session)
			// The host closes its end without an acknowledgement, so the
			// helper's wait ends.
			assert.Nil(t, h.close())
			var b [1]byte
			_, err := h.child.Read(b[:])
			assert.ErrorIs(t, err, io.EOF)
			require.NoError(t, h.child.Close())
		})
	}
}
