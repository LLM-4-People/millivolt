package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt"
)

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
