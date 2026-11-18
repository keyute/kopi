package tool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routedFetcher connects every address to srv, whatever host the URL names.
func routedFetcher(srv *httptest.Server) *fetcher {
	return newFetcher("Kopi/test", func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	})
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url, reason string
	}{
		{url: "ftp://example.com/a", reason: "scheme must be http or https"},
		{url: "file:///etc/hosts", reason: "scheme must be http or https"},
		{url: "https://user:pw@example.com/", reason: "it must not contain credentials"},
		{url: "https:///path", reason: "it has no host"},
		{url: "http://localhost:8080/", reason: "host is not public"},
		{url: "http://LOCALHOST./", reason: "host is not public"},
		{url: "http://intranet/", reason: "host is not public"},
		{url: "http://a.localhost/", reason: "host is not public"},
		{url: "http://printer.local/", reason: "host is not public"},
		{url: "http://x.internal/", reason: "host is not public"},
		{url: "http://127.0.0.1/", reason: "host is not a public address"},
		{url: "http://[::1]/", reason: "host is not a public address"},
		{url: "http://10.1.2.3/", reason: "host is not a public address"},
		{url: "http://169.254.169.254/latest/meta-data", reason: "host is not a public address"},
		{url: "http://100.64.0.1/", reason: "host is not a public address"},
		{url: "http://[::ffff:127.0.0.1]/", reason: "host is not a public address"},
		{url: "https://example.com/a?b"},
		{url: "http://93.184.215.14:8080/"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			u, err := checkURL(tc.url)

			if tc.reason == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.url, u.String())
				return
			}
			require.ErrorIs(t, err, ErrURLNotAllowed)
			assert.EqualError(t, err, fmt.Sprintf("failed to fetch %q: URL is not allowed: %s", tc.url, tc.reason))
		})
	}
}

func TestPublicFetcherRefusesLoopbackAtDial(t *testing.T) {
	requested := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requested <- struct{}{} }))
	defer srv.Close()

	_, err := newPublicFetcher("Kopi/test").fetch(t.Context(), mustURL(t, srv.URL))

	require.ErrorIs(t, err, ErrURLNotAllowed)
	assert.Contains(t, err.Error(), "host resolves to a non-public address")
	assert.Empty(t, requested)
}

func TestFetcherUsesNoProxy(t *testing.T) {
	assert.Nil(t, newPublicFetcher("Kopi/test").transport.Proxy)
}

func TestFetchSendsHeaders(t *testing.T) {
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hi"))
	}))
	defer srv.Close()

	page, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/"))

	require.NoError(t, err)
	assert.Equal(t, "hi", string(page.body))
	assert.Equal(t, "text/plain", page.contentType)
	h := <-headers
	assert.Equal(t, "Kopi/test", h.Get("User-Agent"))
	assert.Equal(t, "text/markdown, text/html;q=0.9, text/plain;q=0.8, */*;q=0.1", h.Get("Accept"))
}

func TestFetchFollowsSameHostRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
	}{
		{name: "same host", from: "http://example.com/old", to: "http://example.com/new"},
		{name: "www", from: "http://www.example.com/old", to: "http://example.com/new"},
		{name: "to www", from: "http://example.com/old", to: "http://www.example.com/new"},
		{name: "http to https", from: "http://example.com/old", to: "https://example.com/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/old" {
					http.Redirect(w, r, tc.to, http.StatusMovedPermanently)
					return
				}
				_, _ = w.Write([]byte("new page"))
			})
			plain := httptest.NewServer(handler)
			defer plain.Close()
			secure := httptest.NewTLSServer(handler)
			defer secure.Close()
			f := newFetcher("Kopi/test", func(ctx context.Context, network, addr string) (net.Conn, error) {
				srv := plain
				if strings.HasSuffix(addr, ":443") {
					srv = secure
				}
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			})
			roots := x509.NewCertPool()
			roots.AddCert(secure.Certificate())
			f.transport.TLSClientConfig = &tls.Config{RootCAs: roots}

			page, err := f.fetch(t.Context(), mustURL(t, tc.from))

			require.NoError(t, err)
			assert.Nil(t, page.redirect)
			assert.Equal(t, tc.to, page.url.String())
			assert.Equal(t, "new page", string(page.body))
		})
	}
}

func TestFetchReturnsCrossHostRedirect(t *testing.T) {
	hosts := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		http.Redirect(w, r, "https://other.example.org/page", http.StatusFound)
	}))
	defer srv.Close()

	page, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/a"))

	require.NoError(t, err)
	assert.Equal(t, "https://other.example.org/page", page.redirect.String())
	assert.Equal(t, http.StatusFound, page.status)
	assert.Equal(t, "http://example.com/a", page.url.String())
	close(hosts)
	var requested []string
	for h := range hosts {
		requested = append(requested, h)
	}
	assert.Equal(t, []string{"example.com"}, requested)
}

func TestFetchReturnsPortChangeAndDowngradeRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
	}{
		{name: "port change", from: "http://example.com:8080/old", to: "http://example.com:8081/new"},
		{name: "https to http", from: "https://example.com/old", to: "http://example.com/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 4)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Host + r.URL.Path
				if r.URL.Path == "/old" {
					http.Redirect(w, r, tc.to, http.StatusFound)
					return
				}
				_, _ = w.Write([]byte("destination page"))
			})
			plain := httptest.NewServer(handler)
			defer plain.Close()
			secure := httptest.NewTLSServer(handler)
			defer secure.Close()
			f := newFetcher("Kopi/test", func(ctx context.Context, network, addr string) (net.Conn, error) {
				srv := plain
				if strings.HasSuffix(addr, ":443") {
					srv = secure
				}
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			})
			roots := x509.NewCertPool()
			roots.AddCert(secure.Certificate())
			f.transport.TLSClientConfig = &tls.Config{RootCAs: roots}

			page, err := f.fetch(t.Context(), mustURL(t, tc.from))

			require.NoError(t, err)
			close(requests)
			var requested []string
			for request := range requests {
				requested = append(requested, request)
			}
			from := mustURL(t, tc.from)
			assert.Equal(t, []string{from.Host + from.Path}, requested)
			require.NotNil(t, page.redirect)
			assert.Equal(t, tc.to, page.redirect.String())
			assert.Equal(t, http.StatusFound, page.status)
			assert.Equal(t, tc.from, page.url.String())
		})
	}
}

func TestFetchStopsAfterFiveRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
	}))
	defer srv.Close()

	_, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/0"))

	require.ErrorIs(t, err, ErrTooManyRedirects)
	assert.EqualError(t, err, `failed to fetch "http://example.com/0": too many redirects: over 5`)
}

func TestFetchReportsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/missing"))

	require.ErrorIs(t, err, ErrHTTPStatus)
	assert.EqualError(t, err, `failed to fetch "http://example.com/missing": server returned an error status: 404 Not Found`)
}

// rawServer answers every connection with response, whatever the request.
func rawServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if !assert.NoError(t, err) {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString(response)
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchKeepsServerTextOutOfErrors(t *testing.T) {
	junk := strings.Repeat("JUNK", 50_000)
	for _, tc := range []struct {
		name, response, want string
	}{
		{
			name:     "status reason",
			response: "HTTP/1.1 503 " + junk + "\r\nContent-Length: 0\r\n\r\n",
			want:     `failed to fetch "http://example.com/": server returned an error status: 503 Service Unavailable`,
		},
		{
			name:     "unknown status",
			response: "HTTP/1.1 599 " + junk + "\r\nContent-Length: 0\r\n\r\n",
			want:     `failed to fetch "http://example.com/": server returned an error status: 599`,
		},
		{name: "malformed first line", response: junk + "\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := routedFetcher(rawServer(t, tc.response)).fetch(t.Context(), mustURL(t, "http://example.com/"))

			require.Error(t, err)
			// A malformed response keeps only a bounded quote of the server's text.
			assert.NotContains(t, err.Error(), strings.Repeat("JUNK", 60))
			assert.Less(t, len(err.Error()), 300)
			if tc.want != "" {
				assert.EqualError(t, err, tc.want)
			}
		})
	}
}

func TestFetchRefusesLongRedirectTarget(t *testing.T) {
	long := "/" + strings.Repeat("a", maxRedirectURL)
	for _, tc := range []struct {
		name, target string
		status       int
	}{
		{name: "same host", target: "http://example.com" + long, status: http.StatusFound},
		{name: "other host", target: "https://other.example.org" + long, status: http.StatusFound},
		// Go does not follow a 300, so fetch checks it itself.
		{name: "not followed", target: "http://example.com" + long, status: http.StatusMultipleChoices},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				http.Redirect(w, r, tc.target, tc.status)
			}))
			defer srv.Close()

			_, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/a"))

			require.ErrorIs(t, err, ErrURLNotAllowed)
			assert.EqualError(t, err, `failed to fetch "http://example.com/a": URL is not allowed: redirect target is too long`)
			close(paths)
			var requested []string
			for p := range paths {
				requested = append(requested, p)
			}
			assert.Equal(t, []string{"/a"}, requested)
		})
	}
}

func TestFetchRefusesLargePageWhileReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Flushing first sends the body chunked, with no Content-Length.
		w.(http.Flusher).Flush()
		_, _ = w.Write(make([]byte, maxPageBytes+1))
	}))
	defer srv.Close()

	_, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/big"))

	require.ErrorIs(t, err, ErrPageTooLarge)
	assert.EqualError(t, err, `failed to fetch "http://example.com/big": page is too large: over 5 MiB`)
}

func TestFetchRefusesLargePageByContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxPageBytes+1))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Reading the body would hang until the client gives up.
		<-r.Context().Done()
	}))
	defer srv.Close()

	_, err := routedFetcher(srv).fetch(t.Context(), mustURL(t, "http://example.com/big"))

	require.ErrorIs(t, err, ErrPageTooLarge)
}

func TestFetchTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	f := routedFetcher(srv)
	f.timeout = 10 * time.Millisecond

	_, err := f.fetch(t.Context(), mustURL(t, "http://example.com/slow"))

	assert.EqualError(t, err, `failed to fetch "http://example.com/slow": timed out after 10ms`)
}

func TestFetchStopsWhenCancelled(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(arrived)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-arrived
		cancel()
	}()

	_, err := routedFetcher(srv).fetch(ctx, mustURL(t, "http://example.com/slow"))

	require.ErrorIs(t, err, context.Canceled)
}

const articleHTML = `<html><head><title>Release notes</title></head><body>
<nav><a href="/">Home</a> <a href="/about">Site navigation menu</a></nav>
<article><h1>Release notes</h1>
<p>Version 1.4 brings a faster parser, a smaller binary and many fixes that users asked for over the last months of work on the project.</p>
<p>The parser now reads large documents in a single pass, which halves the time it takes to load a typical page and keeps memory use flat.</p>
<p>See <a href="/docs/upgrade">the upgrade guide</a> for the steps to move from 1.3, including the two settings that were renamed in this release.</p>
<p>Thanks to everyone who reported issues, tested the release candidates and sent patches; this release would not exist without that help.</p>
</article>
<footer>Copyright footer text</footer>
</body></html>`

func TestPageText(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		contains, excludes      []string
		want                    string
		err                     error
	}{
		{
			name: "article", contentType: "text/html; charset=utf-8", body: articleHTML,
			contains: []string{"# Release notes", "faster parser", "[the upgrade guide](https://example.com/docs/upgrade)"},
			excludes: []string{"Site navigation menu", "Copyright footer text"},
		},
		{
			// Readability finds no article here; on a page with one it drops
			// asides and prefixes the title.
			name: "no article", contentType: "text/html",
			body: `<html><head><title>Side page</title></head><body><aside>Only <a href="/a">A</a> here</aside></body></html>`,
			want: "Only [A](https://example.com/a) here",
		},
		{
			name: "xhtml", contentType: "application/xhtml+xml", body: articleHTML,
			contains: []string{"faster parser"},
		},
		{name: "no header", body: "<html><body><p>Hello <b>there</b></p></body></html>", contains: []string{"Hello **there**"}},
		{name: "plain", contentType: "text/plain", body: "just <b>text</b>", want: "just <b>text</b>"},
		{name: "markdown", contentType: "text/markdown", body: "# Title\n", want: "# Title\n"},
		{name: "json", contentType: "application/json", body: `{"a":1}`, want: `{"a":1}`},
		{name: "ld+json", contentType: "application/ld+json", body: `{"@id":"x"}`, want: `{"@id":"x"}`},
		{name: "xml", contentType: "application/xml", body: "<a/>", want: "<a/>"},
		{name: "latin-1", contentType: "text/plain; charset=ISO-8859-1", body: "caf\xe9", want: "café"},
		{name: "unknown charset", contentType: "text/plain; charset=x-nonsense", body: "café", want: "café"},
		{name: "pdf", contentType: "application/pdf", body: "%PDF-1.7", err: ErrUnsupportedContent},
		{name: "blank", contentType: "text/plain", body: " \n\t", err: ErrEmptyPage},
		{name: "blank html", contentType: "text/html", body: "<html><body> </body></html>", err: ErrEmptyPage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := pageText(tc.contentType, []byte(tc.body), mustURL(t, "https://example.com/notes"))

			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			if tc.want != "" {
				assert.Equal(t, tc.want, text)
			}
			for _, s := range tc.contains {
				assert.Contains(t, text, s)
			}
			for _, s := range tc.excludes {
				assert.NotContains(t, text, s)
			}
		})
	}
}

func TestPageTextErrorMessages(t *testing.T) {
	u := mustURL(t, "https://example.com/doc")

	_, err := pageText("application/pdf", []byte("%PDF"), u)
	assert.EqualError(t, err, `failed to fetch "https://example.com/doc": unsupported content type: "application/pdf"; WebFetch reads HTML, text, JSON and XML`)

	junk := strings.Repeat("JUNK", 50_000)
	_, err = pageText("text/"+junk+"; x", []byte("text"), u)
	assert.EqualError(t, err, `failed to fetch "https://example.com/doc": unsupported content type: the Content-Type header is malformed`)

	_, err = pageText("application/"+junk, []byte("text"), u)
	require.ErrorIs(t, err, ErrUnsupportedContent)
	assert.NotContains(t, err.Error(), strings.Repeat("JUNK", 60))
	assert.Less(t, len(err.Error()), 300)

	_, err = pageText("text/html", []byte("<html></html>"), u)
	assert.EqualError(t, err, `failed to read "https://example.com/doc": page has no text; it may need JavaScript`)
}
