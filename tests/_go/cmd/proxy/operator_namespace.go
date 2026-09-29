package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestGatedPathReadsBackslashAsSeparator pins the namespace membership rule
// the gate's separator refusal depends on: a decoded backslash is read as a
// separator alongside a slash, because upstream proxies and caches have
// historically read it as one, so the reserved namespace cannot depend on the
// next hop's reading.
func TestGatedPathReadsBackslashAsSeparator(t *testing.T) {
	for _, path := range []string{`/mcp\xyz`, `\admin\pause`, `/metrics\`} {
		if !gatedPath(path) {
			t.Errorf("gatedPath(%q) = false, want true (backslash read as separator)", path)
		}
	}
	for _, path := range []string{`/v1\chat`, `/mcp-x`, `/adminx`} {
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
		"/admin%2fpause", "/metrics%2fprometheus", "/dash%2fapp.js",
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
	mux, handler := newOperatorMux(gate)
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
		"/metrics%2fprometheus", "/dash%2fapp.js",
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
