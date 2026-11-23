package pigruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// validators holds the If-None-Match of each request, in order.
	validators []string
	// gate, when set, makes a request wait until it is closed or its client
	// gives up, then answer normally.
	gate chan struct{}
	// inflight counts requests being served; maxInflight is its high mark.
	inflight, maxInflight int
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

// gateRequests makes requests wait until the returned release is called.
func (s *catalogServer) gateRequests() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = gate
	return sync.OnceFunc(func() { close(gate) })
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
		s.validators = append(s.validators, r.Header.Get("If-None-Match"))
		s.inflight++
		s.maxInflight = max(s.maxInflight, s.inflight)
		body, hold, gate := s.body, s.hold, s.gate
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inflight--
		}()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
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
	want := "unknown model: failed to refresh the model list of provider \"" + catalogProvider + "\": "

	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})

	require.ErrorIs(t, err, ErrUnknownModel)
	require.ErrorIs(t, err, ErrModelListUnavailable)
	assert.ErrorContains(t, err, want)
	assert.ErrorContains(t, err, "; declare the model in "+filepath.Join(sb.agentDir, "models.json"))
	var listErr *ModelListError
	require.ErrorAs(t, err, &listErr)
	assert.Equal(t, catalogProvider, listErr.Provider)
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
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrUnknownModel)
	s.holdRequests(false)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Len(t, s.requests, 1)
}

func TestConcurrentChecksShareOneFetch(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	release := s.gateRequests()
	defer release()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- r.Check(t.Context(), OpenOptions{Model: catalogModel}) }()
	}
	<-s.requests
	release()

	require.NoError(t, <-results)
	require.NoError(t, <-results)
	assert.Empty(t, s.requests)
}

func TestWaiterCancelledWhileFetchInFlight(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	release := s.gateRequests()
	defer release()
	fetcher := make(chan error, 1)
	go func() { fetcher <- r.Check(t.Context(), OpenOptions{Model: catalogModel}) }()
	<-s.requests
	ctx, cancel := context.WithCancel(t.Context())
	waiter := make(chan error, 1)
	go func() { waiter <- r.Check(ctx, OpenOptions{Model: catalogModel}) }()
	cancel()

	err := <-waiter
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrUnknownModel)
	release()

	require.NoError(t, <-fetcher)
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

// refreshUsed marks catalogProvider used by checking a bundled id of it, then
// runs the background refresh to its end.
func refreshUsed(t *testing.T, r *Runtime) {
	t.Helper()
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	r.StartModelListRefresh()
	r.refreshes.Wait()
}

func drainRequests(s *catalogServer) {
	for len(s.requests) > 0 {
		<-s.requests
	}
}

// ageStore marks every list in the saved store as checked long ago.
func ageStore(t *testing.T, sb sandbox) {
	t.Helper()
	path := filepath.Join(sb.agentDir, "models-store.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var store any
	require.NoError(t, json.Unmarshal(data, &store))
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if _, ok := v["checkedAt"]; ok {
				v["checkedAt"] = 1
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(store)
	data, err = json.Marshal(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestRefreshAsksOnlyUsedProviders(t *testing.T) {
	sb := newSandbox(t)
	writeAgentFile(t, sb, "auth.json", `{"`+catalogProvider+`":{"type":"api_key","key":"`+catalogKey+`"},"openai":{"type":"api_key","key":"`+catalogKey+`"}}`)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	r := newCatalogRuntime(t, sb, s.URL)

	refreshUsed(t, r)

	assert.Len(t, s.requests, 1)
	drainRequests(s)
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	assert.Empty(t, s.requests)
}

func TestRefreshSkipsFreshListAndRevalidatesStaleOne(t *testing.T) {
	sb, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	refreshUsed(t, r)
	drainRequests(s)

	refreshUsed(t, newCatalogRuntime(t, sb, s.URL))
	assert.Empty(t, s.requests)

	ageStore(t, sb)
	refreshUsed(t, newCatalogRuntime(t, sb, s.URL))
	assert.Len(t, s.requests, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, []string{"", `"catalog-1"`}, s.validators)
}

func TestRefreshNeedsOnlineAndCredentialAndUse(t *testing.T) {
	offline := func(key, value string) func(*testing.T, sandbox, string) *Runtime {
		return func(t *testing.T, sb sandbox, url string) *Runtime {
			t.Setenv(key, value)
			r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws, catalogURL: url})
			require.NoError(t, err)
			t.Cleanup(r.Close)
			return r
		}
	}
	tests := []struct {
		name string
		open func(*testing.T, sandbox, string) *Runtime
		key  bool
		use  bool
	}{
		{"PI_OFFLINE", offline("PI_OFFLINE", "1"), true, true},
		{"PIG_OFFLINE", offline("PIG_OFFLINE", "true"), true, true},
		{"no credential", newCatalogRuntime, false, true},
		{"nothing checked", newCatalogRuntime, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.key {
				clearEnv(t)
			}
			sb := newSandbox(t)
			if tt.key {
				withMistralKey(t, sb)
			}
			probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
			s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
			r := tt.open(t, sb, s.URL)
			switch {
			case tt.use && tt.key:
				refreshUsed(t, r)
			case tt.use:
				// A provider without a credential must not count as used.
				require.ErrorIs(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}), ErrNoCredential)
				r.StartModelListRefresh()
				r.refreshes.Wait()
			default:
				r.StartModelListRefresh()
				r.refreshes.Wait()
			}
			assert.Empty(t, s.requests)
			if !tt.key {
				// PiG itself makes no request without a credential, so the request
				// count cannot pin that the provider stayed out of used.
				r.refreshMu.Lock()
				defer r.refreshMu.Unlock()
				assert.Empty(t, r.used)
			}
		})
	}
}

func TestFailedRefreshIsOnlyLogged(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusServiceUnavailable, "catalog-new-1")
	refreshUsed(t, r)
	assert.NotZero(t, len(s.requests))
	drainRequests(s)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})
	require.ErrorIs(t, err, ErrUnknownModel)
	require.ErrorIs(t, err, ErrModelListUnavailable)
	assert.NotZero(t, len(s.requests))
}

// The registry is checked rather than an open session: the refresh publishes
// to it, and a session reads its model from it.
func TestRefreshAppliesChangedContextWindow(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK)
	bundled := bundledMistral(t, r)
	changed := *bundled
	changed.Capabilities.ContextWindow = bundled.Capabilities.ContextWindow + 1000
	s.bundled = &changed
	s.serve(t, bundled.ID)

	refreshUsed(t, r)

	got := r.svcs.ModelRuntime().GetModel(catalogProvider, bundled.ID)
	require.NotNil(t, got)
	assert.Equal(t, changed.Capabilities.ContextWindow, got.Capabilities.ContextWindow)
}

func TestForcedFetchDuringBackgroundRequestResolves(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	release := s.gateRequests()
	defer release()
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	r.StartModelListRefresh()
	<-s.requests
	result := make(chan error, 1)
	go func() { result <- r.Check(t.Context(), OpenOptions{Model: catalogModel}) }()
	<-s.requests
	release()

	require.NoError(t, <-result)
	r.refreshes.Wait()
}

func TestForcedFetchAfterBackgroundRefreshIgnoresFreshList(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-other-1")
	refreshUsed(t, r)
	drainRequests(s)
	s.serve(t, "catalog-new-1")

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))

	assert.Len(t, s.requests, 1)
}

func TestRefreshAfterForcedFetchMakesNoRequest(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	require.NotEmpty(t, s.requests)
	drainRequests(s)

	r.StartModelListRefresh()
	r.refreshes.Wait()

	assert.Empty(t, s.requests)
}

func TestCloseStopsHeldRefresh(t *testing.T) {
	sb := newSandbox(t)
	withMistralKey(t, sb)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	s.holdRequests(true)
	// Built without newCatalogRuntime, whose cleanup would close it twice.
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws, catalogURL: s.URL})
	require.NoError(t, err)
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	r.StartModelListRefresh()
	<-s.requests

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		r.Close()
	}()
	select {
	case <-closed:
	case <-time.After(waitLimit):
		t.Fatal("Close did not return")
	}
}

func TestProviderFirstUsedAfterStartIsRefreshed(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	r.StartModelListRefresh()
	r.refreshes.Wait()
	require.Empty(t, s.requests)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	r.refreshes.Wait()

	assert.Len(t, s.requests, 1)
}

func TestForcedFetchDuringLateRefreshResolves(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	r.StartModelListRefresh()
	r.refreshes.Wait()
	release := s.gateRequests()
	defer release()
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	<-s.requests
	r.refreshMu.Lock()
	assert.Contains(t, r.fetching, catalogProvider)
	r.refreshMu.Unlock()
	result := make(chan error, 1)
	go func() { result <- r.Check(t.Context(), OpenOptions{Model: catalogModel}) }()
	waitForSlot(t)
	release()

	require.NoError(t, <-result)
	<-s.requests
	r.refreshes.Wait()
	assert.Empty(t, s.requests)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, 1, s.maxInflight, "the forced fetch must wait for the late refresh, not run beside it")
}

// waitForSlot returns once a goroutine is parked in refreshModels' wait for a
// provider's fetching slot; nothing else the runtime does shows that state.
func waitForSlot(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		trace := string(buf[:runtime.Stack(buf, true)])
		for _, g := range strings.Split(trace, "\n\n") {
			if strings.Contains(g, "[select") && strings.Contains(g, "(*Runtime).refreshModels(") && !strings.Contains(g, "fetchModels") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatal("the forced fetch never waited for the provider's slot")
}

func TestLateRefreshIsSkippedAfterForcedFetch(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")
	r.StartModelListRefresh()
	r.refreshes.Wait()
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogModel}))
	require.Len(t, s.requests, 1)
	drainRequests(s)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	r.refreshes.Wait()

	assert.Empty(t, s.requests)
}

func TestCloseStopsHeldLateRefresh(t *testing.T) {
	sb := newSandbox(t)
	withMistralKey(t, sb)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	s.holdRequests(true)
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws, catalogURL: s.URL})
	require.NoError(t, err)
	r.StartModelListRefresh()
	r.refreshes.Wait()
	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/" + bundledMistral(t, r).ID}))
	<-s.requests

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		r.Close()
	}()
	select {
	case <-closed:
	case <-time.After(waitLimit):
		t.Fatal("Close did not return")
	}

	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	assert.Empty(t, r.fetching)
}
