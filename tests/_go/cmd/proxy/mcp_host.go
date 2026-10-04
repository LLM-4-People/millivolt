package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// TestMCPHTTPEndpointServesTheReverseProxiedHost pins the deliberate
// DisableLocalhostProtection in internal/mcp/http.go: the documented placement
// terminates TLS at an ingress that preserves the external Host while dialing
// the proxy over loopback, and the endpoint's own Bearer check is the stronger
// authentication. A valid Bearer request with a foreign Host must therefore
// serve; the SDK's DNS-rebinding check would answer 403 here, so flipping the
// option off must fail this test.
func TestMCPHTTPEndpointServesTheReverseProxiedHost(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	endpoint := httptest.NewServer(newMCPHandler(gate, http.NotFoundHandler(), mcp.LimitsFromConfig(config.Default())))
	t.Cleanup(endpoint.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/mcp", strings.NewReader(initialize))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "millivolt.example.test"
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("foreign Host with a valid Bearer: status = %d, body = %q; want 200 (the documented reverse-proxy placement)", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "serverInfo") {
		t.Fatalf("foreign Host answer = %q, want the server identity", body)
	}
}
