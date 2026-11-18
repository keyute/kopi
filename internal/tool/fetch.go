package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"code.dny.dev/ssrf"
	"codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maxPageBytes = 5 << 20
	// fetchTimeout bounds the whole HTTP phase of a fetch, redirects included.
	fetchTimeout = 30 * time.Second
	// maxPageChars is how many characters of a page the answering model reads.
	maxPageChars = 100_000
	maxRedirects = 5
	// maxRedirectURL bounds a redirect target, which the server chooses and
	// which reaches the model.
	maxRedirectURL = 2000
	// maxCauseRunes bounds an error cause whose text the server may choose.
	maxCauseRunes = 200
	fetchAccept   = "text/markdown, text/html;q=0.9, text/plain;q=0.8, */*;q=0.1"
)

// publicGuard refuses non-public addresses on any port, both for IP literals
// before a request and for every address a dial resolves to.
var publicGuard = ssrf.New(ssrf.WithAnyPort())

// checkURL refuses a URL that cannot name a public web page, before any
// request is made.
func checkURL(raw string) (*url.URL, error) {
	refuse := func(reason string) (*url.URL, error) {
		return nil, fmt.Errorf("failed to fetch %q: %w: %s", raw, ErrURLNotAllowed, reason)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return refuse("it is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return refuse("scheme must be http or https")
	case u.User != nil:
		return refuse("it must not contain credentials")
	case u.Hostname() == "":
		return refuse("it has no host")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if publicGuard.SafeAddr(addr) != nil {
			return refuse("host is not a public address")
		}
		return u, nil
	}
	if host == "localhost" || !strings.Contains(host, ".") ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return refuse("host is not public")
	}
	return u, nil
}

type fetcher struct {
	transport *http.Transport
	userAgent string
	timeout   time.Duration
}

// newPublicFetcher connects only to public addresses.
func newPublicFetcher(userAgent string) *fetcher {
	return newFetcher(userAgent, (&net.Dialer{Control: publicGuard.Safe}).DialContext)
}

func newFetcher(userAgent string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *fetcher {
	// The clone keeps the default transport's TLS settings and HTTP/2.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = dial
	return &fetcher{transport: t, userAgent: userAgent, timeout: fetchTimeout}
}

// fetched is either a page or, when redirect is set, a redirect to another
// host that was not followed.
type fetched struct {
	url         *url.URL
	contentType string
	body        []byte
	redirect    *url.URL
	status      int
}

func (f *fetcher) fetch(ctx context.Context, u *url.URL) (fetched, error) {
	reqCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	client := &http.Client{Transport: f.transport, CheckRedirect: followSameHost}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetched{}, fmt.Errorf("failed to fetch %q: %w", u, err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", fetchAccept)
	resp, err := client.Do(req)
	if err != nil {
		return fetched{}, f.fetchError(ctx, u, err)
	}
	defer resp.Body.Close()
	final := resp.Request.URL
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if target, err := resp.Location(); err == nil {
			if len(target.String()) > maxRedirectURL {
				return fetched{}, redirectTooLong(u)
			}
			return fetched{url: final, redirect: target, status: resp.StatusCode}, nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status code's standard text, never the server's own reason phrase.
		status := strings.TrimSpace(fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
		return fetched{}, fmt.Errorf("failed to fetch %q: %w: %s", u, ErrHTTPStatus, status)
	}
	tooLarge := fmt.Errorf("failed to fetch %q: %w: over %d MiB", u, ErrPageTooLarge, maxPageBytes>>20)
	if resp.ContentLength > maxPageBytes {
		return fetched{}, tooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return fetched{}, f.fetchError(ctx, u, err)
	}
	if len(body) > maxPageBytes {
		return fetched{}, tooLarge
	}
	return fetched{url: final, contentType: resp.Header.Get("Content-Type"), body: body}, nil
}

// fetchError words a failed request or body read; ctx is the caller's.
func (f *fetcher) fetchError(ctx context.Context, u *url.URL, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("failed to fetch %q: timed out after %s", u, f.timeout)
	case errors.Is(err, ssrf.ErrProhibitedIP):
		return fmt.Errorf("failed to fetch %q: %w: host resolves to a non-public address", u, ErrURLNotAllowed)
	case errors.Is(err, ErrTooManyRedirects):
		return fmt.Errorf("failed to fetch %q: %w: over %d", u, ErrTooManyRedirects, maxRedirects)
	case errors.Is(err, ErrURLNotAllowed):
		return redirectTooLong(u)
	}
	// %s, not %w: the cause can quote the server's response, so only its
	// bounded text is kept, and no sentinel is left to match.
	return fmt.Errorf("failed to fetch %q: %s", u, boundedCause(err))
}

func redirectTooLong(u *url.URL) error {
	return fmt.Errorf("failed to fetch %q: %w: redirect target is too long", u, ErrURLNotAllowed)
}

func boundedCause(err error) string {
	text := err.Error()
	if cut := cutRunes(text, maxCauseRunes); cut != text {
		return cut + " …"
	}
	return text
}

// followSameHost follows a redirect only to the same host and port, counting
// a leading "www." as the same host, and only to the same scheme or from http
// to https; any other redirect is returned to the caller. A target over
// maxRedirectURL is refused.
func followSameHost(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return ErrTooManyRedirects
	}
	if len(req.URL.String()) > maxRedirectURL {
		return ErrURLNotAllowed
	}
	from, to := via[len(via)-1].URL, req.URL
	sameHost := strings.TrimPrefix(strings.ToLower(from.Hostname()), "www.") == strings.TrimPrefix(strings.ToLower(to.Hostname()), "www.")
	sameScheme := from.Scheme == to.Scheme || from.Scheme == "http" && to.Scheme == "https"
	if !sameHost || from.Port() != to.Port() || !sameScheme {
		return http.ErrUseLastResponse
	}
	return nil
}

// pageText returns the text of a page fetched from u: HTML as markdown, other
// text as it is. The charset comes from contentType only, else UTF-8.
func pageText(contentType string, body []byte, u *url.URL) (string, error) {
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("failed to fetch %q: %w: the Content-Type header is malformed", u, ErrUnsupportedContent)
	}
	if enc, _ := charset.Lookup(params["charset"]); enc != nil {
		if body, err = enc.NewDecoder().Bytes(body); err != nil {
			return "", fmt.Errorf("failed to read %q: %w", u, err)
		}
	}
	text := strings.ToValidUTF8(string(body), "�")
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		if text, err = htmlMarkdown(text, u); err != nil {
			return "", fmt.Errorf("failed to read %q: %w", u, err)
		}
	case strings.HasPrefix(mediaType, "text/"), mediaType == "application/json", mediaType == "application/xml",
		strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
	default:
		return "", fmt.Errorf("failed to fetch %q: %w: %q; WebFetch reads HTML, text, JSON and XML", u, ErrUnsupportedContent, titleLine(mediaType))
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("failed to read %q: %w; it may need JavaScript", u, ErrEmptyPage)
	}
	return text, nil
}

// htmlMarkdown converts the page's main content, or the whole document when
// readability finds none.
func htmlMarkdown(text string, u *url.URL) (string, error) {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return "", err
	}
	domain := converter.WithDomain(u.String())
	parser := readability.NewParser()
	if article, err := parser.ParseDocument(doc, u); err == nil && article.Node != nil {
		md, err := htmltomarkdown.ConvertNode(article.Node, domain)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(md)) != "" {
			if title := strings.TrimSpace(article.Title()); title != "" {
				return "# " + title + "\n\n" + string(md), nil
			}
			return string(md), nil
		}
	}
	md, err := htmltomarkdown.ConvertNode(doc, domain)
	return string(md), err
}
