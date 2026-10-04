package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestMCPRouteIsRegisteredOnTheOperatorMux pins the reserved namespace table
// operatorNamespaces declares from the boot config and the installation
// newOperatorMux performs on it: the exact patterns and, through the real
// gate, that a valid Bearer POST to /mcp is answered by the MCP endpoint and
// never by the inference catch-all. Both boots are pinned: the enabled boot
// (the default) serves the endpoint, and a boot with mcp_enabled: false keeps
// the root reserved for the 404 handler, so /mcp and every /mcp/ look-alike
// answer 404 and never reach inference. The test drives newOperatorMux
// directly and never observes main, so main calling it (or keeping its
// installation) is not asserted here.
func TestMCPRouteIsRegisteredOnTheOperatorMux(t *testing.T) {
	want := []string{"/admin", "/admin/", "/session", "/session/", "/metrics", "/metrics/", "/mcp", "/mcp/"}
	namespacePatterns := func(t *testing.T, gate *operatorGate, boot *config.Config) ([]string, *http.ServeMux, http.Handler) {
		t.Helper()
		mux, handler := newOperatorMux(gate, boot)
		namespaces := operatorNamespaces(gate, handler, boot)
		if len(namespaces) != len(want) {
			t.Fatalf("reserved namespace count = %d, want %d", len(namespaces), len(want))
		}
		patterns := make([]string, 0, len(namespaces))
		for _, namespace := range namespaces {
			patterns = append(patterns, namespace.pattern)
		}
		return patterns, mux, handler
	}

	// Enabled boot: the endpoint is registered and answers initialize.
	gate := newOperatorGate(mcpEndpointToken)
	patterns, mux, handler := namespacePatterns(t, gate, config.Default())
	for i, pattern := range patterns {
		if pattern != want[i] {
			t.Errorf("enabled boot: reserved namespace %d = %q, want %q", i, pattern, want[i])
		}
	}
	var upstreamHit bool
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	endpoint := httptest.NewServer(handler)
	t.Cleanup(endpoint.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/mcp", strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
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
		t.Fatalf("POST /mcp with a valid Bearer: status = %d, body = %q; want the MCP endpoint's initialize answer", response.StatusCode, body)
	}
	// The MCP endpoint admits only the Bearer and retains nothing: the shared
	// gate mints no session cookie here even though it does on /admin,
	// /metrics and /dash, so an MCP client's cookie jar never gains a
	// 12-hour dashboard credential.
	if set := response.Header.Values("Set-Cookie"); len(set) != 0 {
		t.Fatalf("POST /mcp with a valid Bearer carried Set-Cookie %q; the MCP endpoint must mint no dashboard session", set)
	}
	if upstreamHit {
		t.Fatal("POST /mcp reached the inference catch-all; the route is unregistered")
	}

	// Disabled boot: the exact /mcp root stays reserved for the 404 handler.
	// The gate passes an authenticated gated path to the mux, so an
	// unregistered root would fall through to the inference catch-all;
	// reserving it is the only disabled shape that can never reach inference.
	off := config.Default()
	off.MCPEnabled = false
	offGate := newOperatorGate(mcpEndpointToken)
	offPatterns, offMux, offHandler := namespacePatterns(t, offGate, off)
	for i, pattern := range offPatterns {
		if pattern != want[i] {
			t.Errorf("disabled boot: reserved namespace %d = %q, want %q (the reservation survives a disabled endpoint)", i, pattern, want[i])
		}
	}
	offMux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	offEndpoint := httptest.NewServer(offHandler)
	t.Cleanup(offEndpoint.Close)

	for _, target := range []string{"/mcp", "/mcp/tools", "/mcp/unregistered"} {
		request, err := http.NewRequest(http.MethodPost, offEndpoint.URL+target, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+mcpEndpointToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled boot: POST %s with a valid Bearer: status = %d, body = %q; want the reserved-namespace 404",
				target, response.StatusCode, body)
		}
	}
	if upstreamHit {
		t.Fatal("a disabled-boot /mcp request reached the inference catch-all; the namespace must stay reserved")
	}
}
