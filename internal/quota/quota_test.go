package quota

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransport answers every request with status and body and keeps the requests.
type fakeTransport struct {
	status int
	body   string
	reqs   []*http.Request
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, req)
	return &http.Response{
		StatusCode: f.status,
		Status:     http.StatusText(f.status),
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     http.Header{},
	}, nil
}

func fakeClient(status int, body string) (*http.Client, *fakeTransport) {
	f := &fakeTransport{status: status, body: body}
	return &http.Client{Transport: f}, f
}

func TestLabel(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Hour, "5h"},
		{12 * time.Hour, "12h"},
		{day, "1d"},
		{3 * day, "3d"},
		{7 * day, "wk"},
		{14 * day, "14d"},
		{28 * day, "mo"},
		{30 * day, "mo"},
		{31 * day, "mo"},
		{32 * day, "32d"},
	} {
		assert.Equal(t, tc.want, label(tc.d), tc.d.String())
	}
}

func TestGetErrorNamesStatusOnly(t *testing.T) {
	client, _ := fakeClient(http.StatusUnauthorized, `{"error":"token secret-token rejected"}`)

	err := get(t.Context(), client, "https://example.test/usage", bearer("secret-token"), &struct{}{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unauthorized")
	assert.NotContains(t, err.Error(), "secret-token")
}

func TestGetLimitsTheBody(t *testing.T) {
	client, _ := fakeClient(http.StatusOK, `{"a":"`+strings.Repeat("x", maxBody)+`"}`)

	var out struct{ A string }
	err := get(t.Context(), client, "https://example.test/usage", bearer("tok"), &out)

	assert.Error(t, err)
}

func TestGetSendsOnlyTheGivenHeadersAndAccept(t *testing.T) {
	client, f := fakeClient(http.StatusOK, `{}`)

	require.NoError(t, get(t.Context(), client, "https://example.test/usage", bearer("tok"), &struct{}{}))

	require.Len(t, f.reqs, 1)
	assert.Equal(t, http.Header{"Authorization": {"Bearer tok"}, "Accept": {"application/json"}}, f.reqs[0].Header)
}
