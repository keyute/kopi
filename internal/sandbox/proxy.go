package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.dny.dev/ssrf"
	"golang.org/x/net/http/httpguts"
)

const (
	dialTimeout = 30 * time.Second
	// maxRefused bounds how many distinct refused destinations a session keeps.
	maxRefused = 20
)

// publicGuard refuses non-public addresses on any port for every address a
// dial resolves to, as WebFetch does.
var publicGuard = ssrf.New(ssrf.WithAnyPort())

type sessionKey struct{}

// slogWriter resolves the default logger at write time: it is replaced after
// the proxy is built, and a captured handler would keep writing to stderr.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Warn(strings.TrimSpace(string(p)))
	return len(p), nil
}

// Proxy is the HTTP forward proxy that lets a sandboxed command reach only the
// hosts the user listed. A host is allowed when an allow entry covers it and no
// deny entry does, and the proxy connects only to public addresses. It holds no
// connections of its own: each command gets a session on its own listener.
type Proxy struct {
	allow, deny []domain
	// helper is the argv prefix that runs the in-sandbox listener helper with
	// the command after it. Only Linux uses it.
	helper  []string
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	forward *httputil.ReverseProxy
}

// NewProxy parses the allow and deny lists, each entry as ValidateDomain does.
func NewProxy(allow, deny, helper []string) (*Proxy, error) {
	return newProxy(allow, deny, helper, (&net.Dialer{Timeout: dialTimeout, Control: publicGuard.Safe}).DialContext)
}

func newProxy(allow, deny, helper []string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*Proxy, error) {
	p := &Proxy{helper: helper, dial: dial}
	for _, list := range []struct {
		out     *[]domain
		entries []string
	}{{&p.allow, allow}, {&p.deny, deny}} {
		for _, e := range list.entries {
			d, err := parseDomain(e)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidDomain, err)
			}
			*list.out = append(*list.out, d)
		}
	}
	// The clone keeps the default transport's settings; keep-alives are off so
	// no idle connection outlives its request.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = dial
	t.DisableKeepAlives = true
	p.forward = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The request goes to the URL's host, which is the one decided on.
			pr.Out.Host = pr.Out.URL.Host
			pr.Out.URL.User = nil
		},
		Transport:    t,
		ErrorHandler: p.forwardError,
		ErrorLog:     log.New(slogWriter{}, "", 0),
	}
	return p, nil
}

// session is one command's connection to the proxy: its server, its tunnels
// and the destinations it refused.
type session struct {
	p      *Proxy
	srv    *http.Server
	cancel context.CancelFunc
	served sync.WaitGroup // running Serve calls

	mu      sync.Mutex
	closed  bool
	conns   map[net.Conn]struct{}
	handles sync.WaitGroup // running handlers
	refused []string
}

// serve answers proxy requests on every listener until the session is closed.
func (p *Proxy) serve(listeners ...net.Listener) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{p: p, cancel: cancel, conns: map[net.Conn]struct{}{}}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(slogWriter{}, "", 0),
		BaseContext:       func(net.Listener) context.Context { return context.WithValue(ctx, sessionKey{}, s) },
	}
	for _, l := range listeners {
		s.served.Add(1)
		go func() {
			defer s.served.Done()
			_ = s.srv.Serve(l) // always returns an error; ErrServerClosed after close
		}()
	}
	return s
}

// close stops the session, ends every tunnel and returns the refused host:port
// values in the order they were first refused.
func (s *session) close() []string {
	_ = s.srv.Close()
	s.cancel()
	s.served.Wait()
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.handles.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refused...)
}

func (s *session) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.handles.Add(1)
	return true
}

// track registers c to be closed with the session, or closes it at once when
// the session is already closed.
func (s *session) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *session) untrack(conns ...net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range conns {
		delete(s.conns, c)
	}
}

func (s *session) record(hostPort string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refused) >= maxRefused || slices.Contains(s.refused, hostPort) {
		return
	}
	s.refused = append(s.refused, hostPort)
}

// refuse records the destination before it answers, so a caller that has seen
// the 403 finds it in the session.
func (s *session) refuse(w http.ResponseWriter, kind, hostPort string) {
	s.record(hostPort)
	var why string
	switch kind {
	case "blocked-by-allowlist":
		why = "it is not in sandbox.allowDomains"
	case "blocked-by-denylist":
		why = "it is in sandbox.denyDomains"
	default:
		why = "it resolves to a non-public address"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Proxy-Error", kind)
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, "Kopi's sandbox refused %s: %s.\n", hostPort, why)
}

func badGateway(w http.ResponseWriter, hostPort string, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprintf(w, "Kopi's sandbox could not reach %s: %v.\n", hostPort, err)
}

// decide returns the X-Proxy-Error kind that refuses the destination, or "" to
// allow it. No entry matches an IP literal.
func (p *Proxy) decide(host string, port int) string {
	for _, d := range p.deny {
		if d.matches(host, port) {
			return "blocked-by-denylist"
		}
	}
	for _, d := range p.allow {
		if d.matches(host, port) {
			return ""
		}
	}
	return "blocked-by-allowlist"
}

// validHost reports whether host can be a destination: an IP literal or a name
// of at most 253 bytes in [a-z0-9._-].
func validHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && len(host) <= 253 && strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789._-") == ""
}

// destination splits a host and port, lower-casing the host and dropping one
// trailing dot. ok is false for a destination that is not well formed.
func destination(hostname, port, defaultPort string) (host string, portNum int, ok bool) {
	if port == "" {
		port = defaultPort
	}
	host = strings.TrimSuffix(strings.ToLower(hostname), ".")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || !validHost(host) {
		return "", 0, false
	}
	return host, n, true
}

func (s *session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.begin() {
		http.Error(w, "Kopi's sandbox proxy is closed.", http.StatusServiceUnavailable)
		return
	}
	defer s.handles.Done()
	if r.Method == http.MethodConnect {
		s.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Scheme != "http" || r.URL.Hostname() == "" {
		http.Error(w, "Kopi's sandbox proxy takes CONNECT and absolute http:// requests only.", http.StatusBadRequest)
		return
	}
	// ReverseProxy takes over the client connection on a 101, which would leave
	// the session's tracking and keep close() waiting on an idle client.
	if httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") {
		http.Error(w, "Kopi's sandbox proxy does not upgrade plain HTTP connections; use CONNECT.", http.StatusBadRequest)
		return
	}
	host, port, ok := destination(r.URL.Hostname(), r.URL.Port(), "80")
	if !ok {
		http.Error(w, "Kopi's sandbox proxy got a malformed destination.", http.StatusBadRequest)
		return
	}
	if kind := s.p.decide(host, port); kind != "" {
		s.refuse(w, kind, net.JoinHostPort(host, strconv.Itoa(port)))
		return
	}
	s.p.forward.ServeHTTP(w, r)
}

// forwardError answers a plain request whose upstream round trip failed.
func (p *Proxy) forwardError(w http.ResponseWriter, r *http.Request, err error) {
	s := r.Context().Value(sessionKey{}).(*session)
	host, port, _ := destination(r.URL.Hostname(), r.URL.Port(), "80")
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	if errors.Is(err, ssrf.ErrProhibitedIP) {
		s.refuse(w, "blocked-by-address", hostPort)
		return
	}
	badGateway(w, hostPort, err)
}

func (s *session) connect(w http.ResponseWriter, r *http.Request) {
	hostname, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "Kopi's sandbox proxy got a malformed destination.", http.StatusBadRequest)
		return
	}
	host, portNum, ok := destination(hostname, port, "")
	if !ok {
		http.Error(w, "Kopi's sandbox proxy got a malformed destination.", http.StatusBadRequest)
		return
	}
	hostPort := net.JoinHostPort(host, strconv.Itoa(portNum))
	if kind := s.p.decide(host, portNum); kind != "" {
		s.refuse(w, kind, hostPort)
		return
	}
	upstream, err := s.p.dial(r.Context(), "tcp", hostPort)
	if err != nil {
		if errors.Is(err, ssrf.ErrProhibitedIP) {
			s.refuse(w, "blocked-by-address", hostPort)
			return
		}
		badGateway(w, hostPort, err)
		return
	}
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = upstream.Close()
		slog.Warn("failed to hijack a proxy connection", "error", err)
		return
	}
	defer client.Close()
	defer upstream.Close()
	defer s.untrack(client, upstream)
	if !s.track(client) || !s.track(upstream) {
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// The client may have sent bytes behind the CONNECT head already; they sit
	// in the hijacked reader, not on the connection.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(upstream, buffered.Reader)
		closeWrite(upstream)
	}()
	_, _ = io.Copy(client, upstream)
	closeWrite(client)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// proxyEnv returns the variables that point a command at the proxy on loopback
// port, each in upper and lower case.
func proxyEnv(port int) []string {
	url := "http://127.0.0.1:" + strconv.Itoa(port)
	var env []string
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "GRPC_PROXY"} {
		env = append(env, name+"="+url, strings.ToLower(name)+"="+url)
	}
	return append(env, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
}
