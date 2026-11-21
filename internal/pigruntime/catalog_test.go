package pigruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	catalogProvider = "mistral"
	catalogModel    = catalogProvider + "/catalog-new-1"
	catalogKey      = "synthetic-key-8c1d"
)

// catalogServer serves one provider list and signals every request it gets.
type catalogServer struct {
	*httptest.Server
	requests chan struct{}
	bundled  *ai.Model

	mu   sync.Mutex
	body []byte
	// hold makes a request wait until its client gives up.
	hold bool
}

// serve switches the list to a copy of the bundled model under each of ids.
func (s *catalogServer) serve(t *testing.T, ids ...string) {
	t.Helper()
	records := []json.RawMessage{}
	for _, id := range ids {
		m := *s.bundled
		m.ID = id
		record, err := ai.EncodeStoredModel(&m)
		require.NoError(t, err)
		records = append(records, record)
	}
	body, err := json.Marshal(records)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

func (s *catalogServer) holdRequests(hold bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = hold
}

// newCatalogServer serves a list holding a copy of a bundled mistral model
// under each of ids, or status when it is not 200.
func newCatalogServer(t *testing.T, bundled *ai.Model, status int, ids ...string) *catalogServer {
	t.Helper()
	s := &catalogServer{requests: make(chan struct{}, 8), bundled: bundled}
	s.serve(t, ids...)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/models/providers/"+catalogProvider, r.URL.Path)
		assert.Empty(t, r.Header.Get("Authorization"))
		s.requests <- struct{}{}
		s.mu.Lock()
		body, hold := s.body, s.hold
		s.mu.Unlock()
		if hold {
			<-r.Context().Done()
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"catalog-1"`)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// newCatalogRuntime opens a runtime whose catalog host is url. PI_OFFLINE is
// cleared because PiG skips the fetch when it is set.
func newCatalogRuntime(t *testing.T, sb sandbox, url string) *Runtime {
	t.Helper()
	t.Setenv("PI_OFFLINE", "")
	require.NoError(t, os.Unsetenv("PI_OFFLINE"))
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws, catalogURL: url})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r
}

func bundledMistral(t *testing.T, r *Runtime) *ai.Model {
	t.Helper()
	models := r.svcs.ModelRuntime().GetAllModels(catalogProvider)
	require.NotEmpty(t, models)
	m, ok := models[0].(*ai.Model)
	require.True(t, ok)
	return m
}

func withMistralKey(t *testing.T, sb sandbox) {
	t.Helper()
	writeAgentFile(t, sb, "auth.json", `{"`+catalogProvider+`":{"type":"api_key","key":"`+catalogKey+`"}}`)
}

// catalogSetup returns a sandbox with a mistral credential and a server
// serving ids, plus a runtime on it.
func catalogSetup(t *testing.T, status int, ids ...string) (sandbox, *catalogServer, *Runtime) {
	t.Helper()
	sb := newSandbox(t)
	withMistralKey(t, sb)
	// A throwaway runtime reads a bundled model to build the list from.
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), status, ids...)
	return sb, s, newCatalogRuntime(t, sb, s.URL)
}

func TestUnknownModelIsFetchedOnceAndResolved(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)
}

func TestFetchedModelResolvesFromStoreWithoutRequest(t *testing.T) {
	sb, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	<-s.requests

	fresh := newCatalogRuntime(t, sb, s.URL)

	require.NoError(t, fresh.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Empty(t, s.requests)
}

func TestFetchIgnoresFreshStoredList(t *testing.T) {
	sb, s, r := catalogSetup(t, http.StatusOK, "catalog-other-1")
	require.ErrorIs(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}), ErrUnknownModel)
	<-s.requests
	s.serve(t, "catalog-new-1")

	fresh := newCatalogRuntime(t, sb, s.URL)

	require.NoError(t, fresh.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)
}

func TestModelAbsentFromFetchedListStaysUnknown(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-other-1")

	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})
	require.ErrorIs(t, err, ErrUnknownModel)
	assert.NotErrorIs(t, err, ErrModelListUnavailable)
	require.ErrorIs(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}), ErrUnknownModel)

	assert.Len(t, s.requests, 1)
}

func TestFailedFetchReportsModelListUnavailable(t *testing.T) {
	sb, s, r := catalogSetup(t, http.StatusServiceUnavailable, "catalog-new-1")
	want := "unknown model: the model list could not be refreshed; declare the model in " + filepath.Join(sb.agentDir, "models.json")

	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})

	require.ErrorIs(t, err, ErrUnknownModel)
	require.ErrorIs(t, err, ErrModelListUnavailable)
	assert.ErrorContains(t, err, want)
	// PiG retries a failing fetch, so the count is its own.
	requests := len(s.requests)
	assert.NotZero(t, requests)

	err = r.Check(t.Context(), OpenOptions{Model: catalogModel})

	require.ErrorIs(t, err, ErrUnknownModel)
	require.ErrorIs(t, err, ErrModelListUnavailable)
	assert.Len(t, s.requests, requests)
}

func TestCancelledFetchIsNotRemembered(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	s.holdRequests(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Check(ctx, OpenOptions{Model: catalogModel}) }()
	<-s.requests
	cancel()
	require.Error(t, <-done)
	s.holdRequests(false)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)
}

func TestUnknownModelWithoutCredentialMakesNoRequest(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	r := newCatalogRuntime(t, sb, s.URL)

	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})

	require.ErrorIs(t, err, ErrNoCredential)
	assert.ErrorIs(t, err, ErrUnknownModel)
	assert.Empty(t, s.requests)
}

func TestSignInAfterNoCredentialFetches(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	r := newCatalogRuntime(t, sb, s.URL)
	require.ErrorIs(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}), ErrNoCredential)

	_, err := r.Login(t.Context(), catalogProvider, script(catalogKey).ui())
	require.NoError(t, err)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)
}

func TestUnknownProviderMakesNoRequest(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")

	err := r.Check(t.Context(), OpenOptions{Model: "nosuch/catalog-new-1"})

	require.ErrorIs(t, err, ErrUnknownModel)
	assert.Empty(t, s.requests)
}

func TestModelsFileEntryInheritsProviderAndCredential(t *testing.T) {
	sb := newSandbox(t)
	withMistralKey(t, sb)
	writeAgentFile(t, sb, "models.json", `{"providers":{"`+catalogProvider+`":{"models":[{"id":"brand-new"}]}}}`)
	s := newCatalogServer(t, &ai.Model{}, http.StatusOK)
	r := newCatalogRuntime(t, sb, s.URL)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/brand-new"}))
	assert.Empty(t, s.requests)
	declared := r.svcs.ModelRuntime().GetModel(catalogProvider, "brand-new")
	require.NotNil(t, declared)
	bundled := bundledMistral(t, r)
	assert.Equal(t, bundled.ProviderMeta.API, declared.ProviderMeta.API)
	assert.Equal(t, bundled.ProviderMeta.BaseURL, declared.ProviderMeta.BaseURL)
	assert.NotEmpty(t, declared.ProviderMeta.BaseURL)
}
