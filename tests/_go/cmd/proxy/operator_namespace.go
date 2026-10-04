package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestGatedPathReadsBackslashAsSeparator pins the namespace membership rule
// the gate's separator refusal depends on: a decoded backslash is read as a
// separator alongside a slash, because upstream proxies and caches have
// historically read it as one, so the reserved namespace cannot depend on the
// next hop's reading.
func TestGatedPathReadsBackslashAsSeparator(t *testing.T) {
	for _, path := range []string{`/mcp\xyz`, `\admin\pause`, `/session\xyz`, `/metrics\`} {
		if !gatedPath(path) {
			t.Errorf("gatedPath(%q) = false, want true (backslash read as separator)", path)
		}
	}
	for _, path := range []string{`/v1\chat`, `/mcp-x`, `/adminx`, `/sessionx`} {
		if gatedPath(path) {
			t.Errorf("gatedPath(%q) = true, want false", path)
		}
	}
}

// TestEncodedSeparatorPredicate pins the escaped-path test the gate uses. It
// must see both hex cases of %2f and %5c and must not be fooled by unrelated
// escapes or double encoding.
func TestEncodedSeparatorPredicate(t *testing.T) {
	for _, escaped := range []string{"/mcp%2fxyz", "/mcp%2Fxyz", "/admin%5cpause", "/metrics/a%5Cb", "/mcp/a%2fb"} {
		if !encodedSeparator(escaped) {
			t.Errorf("encodedSeparator(%q) = false, want true", escaped)
		}
	}
	for _, escaped := range []string{"/mcp/xyz", "/mcp%252fxyz", "/mcp%5bxyz", "/mcp%20xyz", "/mcp%2"} {
		if encodedSeparator(escaped) {
			t.Errorf("encodedSeparator(%q) = true, want false", escaped)
		}
	}
}

// TestOperatorGateRefusesEncodedNamespaceSeparators is the middleware-level
// regression: with a valid Bearer, an owned-namespace path written with an
// encoded separator must be refused 404 at the gate and never handed to the
// next handler, in both hex cases and for every gated namespace. A
// percent-encoded character outside the owned namespaces stays ordinary
// encoded inference traffic and is preserved.
func TestOperatorGateRefusesEncodedNamespaceSeparators(t *testing.T) {
	var followed []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = append(followed, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	h := protectOperatorRequests(next, newOperatorGate(mcpEndpointToken))
	for _, target := range []string{
		"/mcp%2fxyz", "/mcp%2Fxyz", "/mcp%5cxyz", "/mcp/a%5Cb",
		"/admin%2fpause", "/session%2fxyz", "/session%5cxyz",
		"/metrics%2fprometheus", "/metrics/pprof%2fheap",
		"/dash%2fapp.js",
	} {
		request := httptest.NewRequest(http.MethodPost, "http://proxy.example"+target, nil)
		request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s with a valid Bearer: status = %d, want 404 (reserved namespace look-alike)", target, recorder.Code)
		}
	}
	if len(followed) != 0 {
		t.Fatalf("look-alikes reached the next handler: %v", followed)
	}
	// The refusal is a denial shape, not a leaked mux answer, and it is
	// credential-independent: the reserved namespace is not even probed.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.example/mcp%2fxyz", nil)
	request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
	h.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("look-alike refusal Cache-Control = %q, want no-store", got)
	}
	for _, target := range []string{"/mcp%2fxyz", "/admin%2fpause"} {
		recorder = httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://proxy.example"+target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s without a credential: status = %d, want the reserved-namespace 404", target, recorder.Code)
		}
	}
	// Outside the namespaces an encoded separator is a normal encoded
	// character: it must be preserved and forwarded untouched.
	for _, target := range []string{"/v1/chat%2fcompletions", "/foo%5cbar"} {
		request = httptest.NewRequest(http.MethodPost, "http://proxy.example"+target, nil)
		request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
		recorder = httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Errorf("%s outside the namespaces: status = %d, want 204 (preserved)", target, recorder.Code)
		}
	}
}

// TestOperatorNamespaceLookAlikesNeverReachInference is the end-to-end
// regression for the escaped-separator gap: the real gate, the real reserved
// namespace table installed by newOperatorMux, and a recording inference
// catch-all. A valid Bearer on a percent-separator namespace look-alike must
// answer 404 and the catch-all must see nothing, while an encoded character
// outside the namespaces still reaches inference.
func TestOperatorNamespaceLookAlikesNeverReachInference(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	var mu sync.Mutex
	var upstreamHits []string
	mux, handler := newOperatorMux(gate, config.Default())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamHits = append(upstreamHits, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)

	post := func(target, credential string) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, endpoint.URL+target, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", target, err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}

	for _, target := range []string{
		"/mcp%2fxyz", "/mcp%2Fxyz", "/mcp%5cxyz", "/admin%2fpause",
		"/session%2fxyz", "/session%5cxyz",
		"/metrics%2fprometheus", "/metrics/pprof%2fheap", "/dash%2fapp.js",
	} {
		if status := post(target, mcpEndpointToken); status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (the gate's reserved-namespace refusal)", target, status)
		}
	}
	if status := post("/mcp%2fxyz", ""); status != http.StatusNotFound {
		t.Errorf("credential-less look-alike: status = %d, want the reserved-namespace 404", status)
	}
	if status := post("/v1/chat%2fcompletions", mcpEndpointToken); status != http.StatusNoContent {
		t.Errorf("non-namespace encoded path: status = %d, want the catch-all's 204 (preserved)", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(upstreamHits) != 1 || upstreamHits[0] != "POST /v1/chat%2fcompletions" {
		t.Fatalf("inference catch-all saw %v, want only the non-namespace encoded path", upstreamHits)
	}
}

// TestMetricsPprofRoutesServeOnlyThroughTheOperatorGate pins the profiling
// subtree's admission contract end to end through the real gate, the reserved
// namespace table and registerPprofRoutes, the same installation main
// performs: an unarmed plane denies the subtree, a wrong credential is
// challenged, an authenticated operator reaches the index and every
// registered profile, an unregistered subpath answers the reserved 404 (the
// stdlib index listing names cmdline, symbol and trace, none of which is
// mounted), a wrong method answers 405, and a single-encoded separator
// spelling never reaches the mounted handlers or the inference catch-all.
func TestMetricsPprofRoutesServeOnlyThroughTheOperatorGate(t *testing.T) {
	var mu sync.Mutex
	var upstreamHits []string
	server := func(gate *operatorGate) *httptest.Server {
		mux, handler := newOperatorMux(gate, config.Default())
		registerPprofRoutes(mux)
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			upstreamHits = append(upstreamHits, r.URL.EscapedPath())
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
		endpoint := httptest.NewServer(handler)
		t.Cleanup(endpoint.Close)
		return endpoint
	}
	get := func(t *testing.T, target, credential string) (int, http.Header, []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, body
	}
	// Every profile payload is a gzip stream (the runtime writes gzipped
	// protobuf), so the magic bytes prove a real profile came back without
	// parsing it.
	profilePayload := func(t *testing.T, name string, header http.Header, body []byte) {
		t.Helper()
		if ct := header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s: Content-Type = %q, want application/octet-stream", name, ct)
		}
		if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
			t.Errorf("%s: body does not start with the gzip magic bytes", name)
		}
	}

	t.Run("unarmed plane denies the subtree", func(t *testing.T) {
		endpoint := server(newOperatorGate(""))
		for _, target := range []string{"/metrics/pprof/", "/metrics/pprof/heap"} {
			for _, credential := range []string{"", "some-bearer-fixture-credential"} {
				status, header, _ := get(t, endpoint.URL+target, credential)
				if status != http.StatusForbidden {
					t.Errorf("%s (credential %q): status = %d, want 403", target, credential, status)
				}
				if cc := header.Get("Cache-Control"); cc != "no-store" {
					t.Errorf("%s: Cache-Control = %q, want no-store", target, cc)
				}
			}
		}
	})

	armed := server(newOperatorGate(mcpEndpointToken))

	t.Run("wrong credential is challenged", func(t *testing.T) {
		status, header, body := get(t, armed.URL+"/metrics/pprof/", "wrong-fixture-credential-value")
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if got := header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("WWW-Authenticate = %q, want the Bearer challenge", got)
		}
		if strings.Contains(string(body), mcpEndpointToken) {
			t.Fatalf("denial echoed the credential: %q", body)
		}
	})

	t.Run("authenticated operator reaches the index and profiles", func(t *testing.T) {
		status, header, body := get(t, armed.URL+"/metrics/pprof/", mcpEndpointToken)
		if status != http.StatusOK {
			t.Fatalf("index status = %d, want 200", status)
		}
		if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("index Content-Type = %q, want text/html", ct)
		}
		if !strings.Contains(string(body), "heap") {
			t.Fatalf("index body does not list the profiles: %q", body)
		}
		// The slash-less root is redirected to the index by the mux's
		// subtree-root redirect, the same convenience the stdlib's own
		// /debug/pprof registration gives, and never falls through to
		// inference.
		if status, _, body := get(t, armed.URL+"/metrics/pprof", mcpEndpointToken); status != http.StatusOK || !strings.Contains(string(body), "heap") {
			t.Fatalf("GET /metrics/pprof: status = %d, want the redirected index; body %q", status, body)
		}
		for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
			status, header, body := get(t, armed.URL+"/metrics/pprof/"+name, mcpEndpointToken)
			if status != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", name, status)
			}
			profilePayload(t, name, header, body)
		}
		// The CPU profile is the one mounted endpoint outside the registry
		// names; seconds=1 keeps the capture window bounded.
		status, header, body = get(t, armed.URL+"/metrics/pprof/profile?seconds=1", mcpEndpointToken)
		if status != http.StatusOK {
			t.Fatalf("profile: status = %d, want 200", status)
		}
		profilePayload(t, "profile", header, body)
	})

	t.Run("unregistered subpaths stay reserved", func(t *testing.T) {
		for _, name := range []string{"cmdline", "symbol", "trace", "unknown"} {
			if status, _, _ := get(t, armed.URL+"/metrics/pprof/"+name, mcpEndpointToken); status != http.StatusNotFound {
				t.Errorf("%s: status = %d, want the reserved namespace 404", name, status)
			}
		}
	})

	t.Run("wrong method answers 405 and never inference", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, armed.URL+"/metrics/pprof/heap", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST heap: status = %d, want 405", response.StatusCode)
		}
		if allow := response.Header.Get("Allow"); allow != http.MethodGet {
			t.Fatalf("POST heap: Allow = %q, want GET", allow)
		}
	})

	t.Run("encoded separator spelling never reaches the handlers", func(t *testing.T) {
		if status, _, _ := get(t, armed.URL+"/metrics/pprof%2fheap", mcpEndpointToken); status != http.StatusNotFound {
			t.Fatalf("look-alike: status = %d, want the reserved namespace 404", status)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(upstreamHits) != 0 {
		t.Fatalf("inference catch-all saw %v, want nothing", upstreamHits)
	}
}
