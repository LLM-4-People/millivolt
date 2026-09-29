package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMCPRouteIsRegisteredOnTheOperatorMux pins the reserved namespace table
// operatorNamespaces declares and the installation newOperatorMux performs on
// it: the exact patterns and, through the real gate, that a valid Bearer POST
// to /mcp is answered by the MCP endpoint and never by the inference
// catch-all. The test drives newOperatorMux directly and never observes main,
// so main calling it (or keeping its installation) is not asserted here.
func TestMCPRouteIsRegisteredOnTheOperatorMux(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	mux, handler := newOperatorMux(gate)
	namespaces := operatorNamespaces(gate, handler)
	want := []string{"/admin", "/admin/", "/metrics", "/metrics/", "/mcp", "/mcp/"}
	if len(namespaces) != len(want) {
		t.Fatalf("reserved namespace count = %d, want %d", len(namespaces), len(want))
	}
	for i, namespace := range namespaces {
		if namespace.pattern != want[i] {
			t.Errorf("reserved namespace %d = %q, want %q", i, namespace.pattern, want[i])
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
	if upstreamHit {
		t.Fatal("POST /mcp reached the inference catch-all; the route is unregistered")
	}
}
