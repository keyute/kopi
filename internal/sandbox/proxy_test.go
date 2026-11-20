package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"code.dny.dev/ssrf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// dialTo maps every address to upstream, which the real guard would refuse
// when it is on loopback.
func dialTo(upstream string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream)
	}
}

func failDial(err error) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) { return nil, err }
}

type proxyRig struct {
	sess *session
	addr string
}

func startProxy(t *testing.T, allow, deny []string, dial func(context.Context, string, string) (net.Conn, error)) *proxyRig {
	t.Helper()
	p, err := newProxy(allow, deny, nil, dial)
	require.NoError(t, err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &proxyRig{sess: p.serve(l), addr: l.Addr().String()}
	t.Cleanup(func() { r.sess.close() })
	return r
}

func (r *proxyRig) client() *http.Client {
	proxyURL := &url.URL{Scheme: "http", Host: r.addr}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}}
}

func echoUpstream(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return l.Addr().String()
}

// reply is a response whose body has been read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, c *http.Client, target string) (reply, string) {
	t.Helper()
	resp, err := c.Get(target)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return reply{resp.StatusCode, resp.Header}, string(body)
}

func TestNewProxyRejectsInvalidEntries(t *testing.T) {
	_, err := NewProxy([]string{"a.b", "*.com"}, nil, nil)
	require.ErrorIs(t, err, ErrInvalidDomain)
	assert.Contains(t, err.Error(), "*.com")
	_, err = NewProxy(nil, []string{"1.2.3.4"}, nil)
	require.ErrorIs(t, err, ErrInvalidDomain)
}

func TestNewProxyKeepsHelper(t *testing.T) {
	p, err := NewProxy([]string{"a.b"}, nil, []string{"kopi", "sandbox-listen", "--"})
	require.NoError(t, err)
	assert.Equal(t, []string{"kopi", "sandbox-listen", "--"}, p.helper)
}

func TestNewProxyDialerRefusesLoopback(t *testing.T) {
	p, err := NewProxy([]string{"a.b"}, nil, nil)
	require.NoError(t, err)
	_, err = p.dial(t.Context(), "tcp", "127.0.0.1:1")
	require.ErrorIs(t, err, ssrf.ErrProhibitedIP)
}

func TestProxyPlainHTTPAllowed(t *testing.T) {
	seen := make(chan *http.Request, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		_, _ = io.WriteString(w, "hello")
	}))
	defer up.Close()
	rig := startProxy(t, []string{"a.test"}, nil, dialTo(up.Listener.Addr().String()))

	req, err := http.NewRequest(http.MethodGet, "http://A.test/path?q=1", nil)
	require.NoError(t, err)
	req.Header.Set("Proxy-Authorization", "Basic eHg6eHg=")
	req.Header.Set("Keep-Alive", "timeout=5")
	resp, err := rig.client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello", string(body))
	got := <-seen
	assert.Equal(t, "A.test", got.Host)
	assert.Equal(t, "/path?q=1", got.RequestURI)
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Keep-Alive"} {
		assert.Empty(t, got.Header.Values(h), h)
	}
	assert.Empty(t, rig.sess.close())
}

func TestProxyPlainHTTPRefused(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("must not dial")))

	resp, body := get(t, rig.client(), "http://b.test:8080/x")

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "blocked-by-allowlist", resp.Header.Get("X-Proxy-Error"))
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "Kopi's sandbox refused b.test:8080: it is not in sandbox.allowDomains.\n", body)
	assert.Equal(t, []string{"b.test:8080"}, rig.sess.close())
}

func TestProxyRefusesUpgradeRequests(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("must not dial")))
	conn := rawConn(t, rig)

	_, err := io.WriteString(conn, "GET http://a.test/ HTTP/1.1\r\nHost: a.test\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Empty(t, rig.sess.close())
}

func TestProxyDenyBeatsAllow(t *testing.T) {
	rig := startProxy(t, []string{"*.a.test"}, []string{"bad.a.test"}, failDial(errors.New("must not dial")))

	resp, body := get(t, rig.client(), "http://bad.a.test/")

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "blocked-by-denylist", resp.Header.Get("X-Proxy-Error"))
	assert.Equal(t, "Kopi's sandbox refused bad.a.test:80: it is in sandbox.denyDomains.\n", body)
}

func TestProxyIPLiteralIsNeverAllowed(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("must not dial")))

	resp, _ := get(t, rig.client(), "http://1.2.3.4/")

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, []string{"1.2.3.4:80"}, rig.sess.close())
}

func TestProxyPortScope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	rig := startProxy(t, []string{"a.test:8080"}, nil, dialTo(up.Listener.Addr().String()))

	resp, _ := get(t, rig.client(), "http://a.test:8080/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = get(t, rig.client(), "http://a.test/")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, []string{"a.test:80"}, rig.sess.close())
}

func TestProxyAddressRefusal(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(fmt.Errorf("dial: %w", ssrf.ErrProhibitedIP)))

	resp, body := get(t, rig.client(), "http://a.test/")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "blocked-by-address", resp.Header.Get("X-Proxy-Error"))
	assert.Equal(t, "Kopi's sandbox refused a.test:80: it resolves to a non-public address.\n", body)

	conn := rawConn(t, rig)
	_, err := io.WriteString(conn, "CONNECT a.test:443 HTTP/1.1\r\nHost: a.test:443\r\n\r\n")
	require.NoError(t, err)
	connectResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	connectResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, connectResp.StatusCode)
	assert.Equal(t, "blocked-by-address", connectResp.Header.Get("X-Proxy-Error"))
	assert.Equal(t, []string{"a.test:80", "a.test:443"}, rig.sess.close())
}

func TestProxyOtherDialFailureIsBadGatewayAndNotRecorded(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("connection refused")))

	resp, body := get(t, rig.client(), "http://a.test/")
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, body, "connection refused")

	conn := rawConn(t, rig)
	_, err := io.WriteString(conn, "CONNECT a.test:443 HTTP/1.1\r\nHost: a.test:443\r\n\r\n")
	require.NoError(t, err)
	connectResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	connectResp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, connectResp.StatusCode)
	assert.Empty(t, rig.sess.close())
}

func TestProxyMalformedRequests(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("must not dial")))
	for name, head := range map[string]string{
		"origin form":  "GET /path HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"https scheme": "GET https://a.test/ HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"bad host":     "GET http://a%20b.test/ HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"bad port":     "GET http://a.test:99999/ HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"connect":      "CONNECT a_b$.test:443 HTTP/1.1\r\nHost: a.test\r\n\r\n",
		"connect port": "CONNECT a.test HTTP/1.1\r\nHost: a.test\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			conn := rawConn(t, rig)
			_, err := io.WriteString(conn, head)
			require.NoError(t, err)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	assert.Empty(t, rig.sess.close())
}

func rawConn(t *testing.T, rig *proxyRig) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", rig.addr)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProxyConnectRelaysWithHalfClose(t *testing.T) {
	rig := startProxy(t, []string{"a.test:443"}, nil, dialTo(echoUpstream(t)))
	conn := rawConn(t, rig)

	// The payload rides in the same write as the CONNECT head, so it reaches
	// the proxy's read buffer rather than the connection.
	_, err := io.WriteString(conn, "CONNECT a.test:443 HTTP/1.1\r\nHost: a.test:443\r\n\r\nping")
	require.NoError(t, err)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	echoed, err := io.ReadAll(r)
	require.NoError(t, err)

	assert.Equal(t, "ping", string(echoed))
	assert.Empty(t, rig.sess.close())
}

func TestProxyConnectRefused(t *testing.T) {
	rig := startProxy(t, []string{"a.test:443"}, nil, failDial(errors.New("must not dial")))
	conn := rawConn(t, rig)

	_, err := io.WriteString(conn, "CONNECT B.test:443 HTTP/1.1\r\nHost: b.test:443\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "blocked-by-allowlist", resp.Header.Get("X-Proxy-Error"))
	assert.Equal(t, "Kopi's sandbox refused b.test:443: it is not in sandbox.allowDomains.\n", string(body))
	assert.Equal(t, []string{"b.test:443"}, rig.sess.close())
}

func TestProxyRefusedAreDistinctCappedAndOrdered(t *testing.T) {
	rig := startProxy(t, []string{"a.test"}, nil, failDial(errors.New("must not dial")))
	var want []string
	for i := range maxRefused + 5 {
		host := fmt.Sprintf("h%d.test", i)
		if i < maxRefused {
			want = append(want, host+":80")
		}
		for range 2 {
			resp, _ := get(t, rig.client(), "http://"+host+"/")
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
		}
	}
	assert.Equal(t, want, rig.sess.close())
}

func TestProxyCloseEndsAnIdleTunnel(t *testing.T) {
	up := echoUpstream(t)
	ignore := goleak.IgnoreCurrent()
	p, err := newProxy([]string{"a.test"}, nil, nil, dialTo(up))
	require.NoError(t, err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	sess := p.serve(l)
	conn, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = io.WriteString(conn, "CONNECT a.test:80 HTTP/1.1\r\nHost: a.test:80\r\n\r\n")
	require.NoError(t, err)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Empty(t, sess.close())

	_, err = r.ReadByte()
	assert.ErrorIs(t, err, io.EOF)
	conn.Close()
	goleak.VerifyNone(t, ignore)
}

func TestProxyTrackAfterCloseClosesTheConnection(t *testing.T) {
	p, err := newProxy(nil, nil, nil, failDial(errors.New("unused")))
	require.NoError(t, err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	sess := p.serve(l)
	sess.close()

	a, b := net.Pipe()
	defer b.Close()
	assert.False(t, sess.track(a))
	_, err = a.Write([]byte("x"))
	assert.ErrorIs(t, err, io.ErrClosedPipe)
	assert.False(t, sess.begin())
}

func TestProxyEnv(t *testing.T) {
	assert.Equal(t, []string{
		"HTTP_PROXY=http://127.0.0.1:3128", "http_proxy=http://127.0.0.1:3128",
		"HTTPS_PROXY=http://127.0.0.1:3128", "https_proxy=http://127.0.0.1:3128",
		"ALL_PROXY=http://127.0.0.1:3128", "all_proxy=http://127.0.0.1:3128",
		"GRPC_PROXY=http://127.0.0.1:3128", "grpc_proxy=http://127.0.0.1:3128",
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
	}, proxyEnv(3128))
}
