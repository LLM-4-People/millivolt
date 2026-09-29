package mcp

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCredentialServerCacheReusesAndIsolates pins the /mcp server cache: the
// same credential reuses one server, a changed credential gets a new one and
// discards the previous entry (so nothing is ever shared across credentials),
// and a failed build is not cached.
func TestCredentialServerCacheReusesAndIsolates(t *testing.T) {
	service := newTestService(t, newFakeProxy(t), Limits{})
	builds := 0
	build := func() *sdk.Server {
		builds++
		return NewServer(service)
	}
	var cache credentialServerCache

	first := cache.serverFor(testToken, build)
	second := cache.serverFor(testToken, build)
	if first == nil || first != second {
		t.Fatalf("repeated requests with one credential must reuse one server (first %p, second %p)", first, second)
	}
	if builds != 1 {
		t.Fatalf("one credential built %d servers, want exactly one", builds)
	}

	other := cache.serverFor("another-fixture-credential-value", build)
	if builds != 2 {
		t.Fatalf("a changed credential must build its own server once (builds %d, want 2)", builds)
	}
	if other == nil || other == first {
		t.Fatalf("a changed credential must never receive the previous server (old %p, new %p)", first, other)
	}

	// The cache holds one entry: returning to the first credential rebuilds
	// rather than resurrecting the discarded server.
	third := cache.serverFor(testToken, build)
	if builds != 3 || third == first || third == other {
		t.Fatalf("switching back must invalidate the previous entry (builds %d, third %p)", builds, third)
	}

	failed := 0
	fail := func() *sdk.Server { failed++; return nil }
	if got := cache.serverFor("third-fixture-credential-value", fail); got != nil {
		t.Fatalf("a nil build must return nil, got %p", got)
	}
	if got := cache.serverFor("third-fixture-credential-value", fail); got != nil || failed != 2 {
		t.Fatalf("a failed build must not be cached (got %p, attempts %d)", got, failed)
	}
}

// TestNewHTTPHandlerReusesTheServerAcrossRequests is the end-to-end pin for
// the same reuse through the real handler: the first POST pays the server
// construction (all 22 tools and their schemas), and every later POST with the
// same credential pays only the request. The allocation ratio is the
// observation; a handler that rebuilt the server per POST would allocate the
// construction cost every time.
func TestNewHTTPHandlerReusesTheServerAcrossRequests(t *testing.T) {
	service := newTestService(t, newFakeProxy(t), Limits{})
	endpoint := httptest.NewServer(NewHTTPHandler(func(*http.Request) *Service { return service }))
	t.Cleanup(endpoint.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	post := func() {
		request, err := http.NewRequest(http.MethodPost, endpoint.URL, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "serverInfo") {
			t.Fatalf("POST status %d body %q, want an initialize answer", response.StatusCode, body)
		}
	}
	allocated := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.TotalAlloc
	}

	before := allocated()
	post()
	first := allocated() - before
	before = allocated()
	post()
	second := allocated() - before
	if first <= second {
		t.Fatalf("the first POST must pay the server construction (first %d bytes, second %d)", first, second)
	}
	if second*4 > first {
		t.Fatalf("the second POST allocated %d bytes against the first's %d; the server must be reused", second, first)
	}
}

// TestNewServerIsTheSharedRegistry pins the registry NewServer builds: its
// tool count against the pinned surface and the shared release identity. It
// drives the in-process transport only, so an entrypoint that bypasses
// NewServer with a parallel constructor registering an equivalent surface is
// not distinguishable here.
func TestNewServerIsTheSharedRegistry(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != len(registeredToolNames) {
		t.Fatalf("NewServer registered %d tools, want the pinned surface's %d", len(listed.Tools), len(registeredToolNames))
	}
	info := session.InitializeResult().ServerInfo
	if info == nil || info.Name != "millivolt" || info.Version != millivolt.Version() {
		t.Fatalf("server identity = %+v, want millivolt at the shared release version %q", info, millivolt.Version())
	}
}

// handlerTransport dispatches a synthesized request to an http.Handler with no
// network hop, the shape the proxy's /mcp endpoint injects. It exists here so
// the injected-transport contract is pinned without a running proxy.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

// TestInProcessTransportCarriesTheCallerCredential pins the contract the HTTP
// endpoint relies on: with an injected transport, the synthesized request
// reaches the proxy handler with the caller's Bearer header and the named
// non-routable origin, and the service reports that origin rather than any
// request Host. A loopback default or a Host-derived origin would fail here.
func TestInProcessTransportCarriesTheCallerCredential(t *testing.T) {
	if _, err := NormalizeProxyURL(InProcessOrigin); err != nil {
		t.Fatalf("the in-process origin must be a valid client origin: %v", err)
	}
	var mu sync.Mutex
	type dispatched struct{ method, path, host, auth string }
	var seen []dispatched
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, dispatched{method: r.Method, path: r.URL.Path, host: r.Host, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "millivolt_fixture_metric 1")
	})
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, DefaultLimits(), handlerTransport{handler: dispatch})
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Origin(); got != InProcessOrigin {
		t.Fatalf("Origin() = %q, want the named in-process origin %q", got, InProcessOrigin)
	}
	document := structured(t, call(t, connect(t, service), "prometheus", map[string]any{}))
	if lines, ok := document["lines"].(float64); !ok || lines < 1 {
		t.Fatalf("the in-process dispatch did not answer the tool: %v", document)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("in-process dispatch saw %d requests, want exactly one", len(seen))
	}
	got := seen[0]
	if got.method != http.MethodGet || got.path != "/metrics/prometheus" {
		t.Fatalf("dispatched %s %s, want GET /metrics/prometheus", got.method, got.path)
	}
	if got.host != "millivolt.internal" {
		t.Fatalf("dispatched Host = %q, want the non-routable millivolt.internal", got.host)
	}
	if want := "Bearer " + testToken; got.auth != want {
		t.Fatalf("dispatched Authorization = %q, want the caller credential %q", got.auth, want)
	}
}
