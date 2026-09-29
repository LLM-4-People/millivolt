package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// mcpEndpointToken arms the gate of the endpoint under test. It is a fixture
// value for a throwaway in-process server, not a credential for anything else.
const mcpEndpointToken = "mcp-http-endpoint-fixture-credential"

// TestGatedPathOwnsTheMCPNamespace pins the /mcp namespace to the operator
// plane: both the root and every look-alike are gated, so neither the
// unauthenticated gate nor an authenticated request can forward one to the
// inference catch-all, exactly like /admin and /metrics.
func TestGatedPathOwnsTheMCPNamespace(t *testing.T) {
	for _, path := range []string{"/mcp", "/mcp/", "/mcp/tools", "/mcp/unregistered"} {
		if !gatedPath(path) {
			t.Errorf("gatedPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/mcpx", "/mcp-tools", "/other"} {
		if gatedPath(path) {
			t.Errorf("gatedPath(%q) = true, want false", path)
		}
	}
}

// dispatchRecord is one synthesized operator request as the fake proxy's own
// handler saw it.
type dispatchRecord struct {
	Method        string
	Path          string
	Host          string
	Authorization string
}

// recordingMCPDispatch is the stand-in for the proxy's guarded handler: it
// records every synthesized request and answers the Prometheus exposition.
func recordingMCPDispatch(mu *sync.Mutex, records *[]dispatchRecord) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*records = append(*records, dispatchRecord{
			Method: r.Method, Path: r.URL.Path, Host: r.Host,
			Authorization: r.Header.Get("Authorization"),
		})
		mu.Unlock()
		if r.URL.Path != "/metrics/prometheus" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "millivolt_fixture_metric 1")
	})
}

// TestMCPHTTPEndpointAuthAndInProcessDispatch is the endpoint contract: a
// valid Bearer serves the same tool surface as stdio, the caller's credential
// rides every in-process call, the nominal origin is never derived from the
// request Host, and a cookie-only or credential-less request can never create
// an internal call.
func TestMCPHTTPEndpointAuthAndInProcessDispatch(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	var mu sync.Mutex
	var records []dispatchRecord
	endpoint := httptest.NewServer(newMCPHandler(gate, recordingMCPDispatch(&mu, &records)))
	t.Cleanup(endpoint.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	post := func(t *testing.T, credential string, cookie *http.Cookie) (*http.Response, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/mcp", strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", endpoint.URL, err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, string(body)
	}

	t.Run("no credential is refused before the MCP layer", func(t *testing.T) {
		response, body := post(t, "", nil)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 without a Bearer", response.StatusCode)
		}
		if got := response.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		if !strings.Contains(body, "Bearer") {
			t.Fatalf("denial must name the accepted scheme: %q", body)
		}
	})

	t.Run("wrong bearer is refused and never echoed", func(t *testing.T) {
		const wrong = "wrong-endpoint-fixture-credential"
		response, body := post(t, wrong, nil)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a wrong Bearer", response.StatusCode)
		}
		if strings.Contains(body, wrong) || strings.Contains(body, mcpEndpointToken) {
			t.Fatalf("denial echoed a credential: %q", body)
		}
	})

	t.Run("a cookie session alone is refused", func(t *testing.T) {
		cookie, err := gate.mintSessionCookie()
		if err != nil {
			t.Fatal(err)
		}
		// A cookie minted for the same gate: the handler must still refuse.
		response, body := post(t, "", cookie)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cookie-only status = %d, want 403 (the caller credential, not the session cookie, is forwarded)", response.StatusCode)
		}
		if !strings.Contains(body, "session cookie is not accepted") {
			t.Fatalf("cookie-only denial must say why: %q", body)
		}
	})

	t.Run("a valid bearer initializes", func(t *testing.T) {
		response, body := post(t, mcpEndpointToken, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %q", response.StatusCode, body)
		}
		if !strings.Contains(body, "serverInfo") {
			t.Fatalf("initialize answer = %q, want the server identity", body)
		}
	})

	// The real streamable HTTP client against the same endpoint: the tool
	// surface and an executed tool prove the whole path.
	session := connectMCPHTTP(t, endpoint.URL+"/mcp", mcpEndpointToken)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint must serve the shared registry, whatever it holds: compare
	// the HTTP listing against the production NewServer registration rather
	// than repeating a tool count a tool addition would leave stale.
	if got, want := toolNameSet(listed), registeredToolNames(t); !maps.Equal(got, want) {
		t.Fatalf("tools/list over HTTP differs from the shared registry: got %v, want %v", got, want)
	}
	document := mcpStructured(t, mcpCall(t, session, "prometheus", map[string]any{}))
	if lines, ok := document["lines"].(float64); !ok || lines < 1 {
		t.Fatalf("prometheus result = %v, want at least one line", document)
	}

	// Every in-process call carried the caller's Bearer and the named
	// non-routable origin, never the Host the outer request presented.
	mu.Lock()
	defer mu.Unlock()
	if len(records) == 0 {
		t.Fatal("no synthesized request reached the proxy handler")
	}
	last := records[len(records)-1]
	if last.Method != http.MethodGet || last.Path != "/metrics/prometheus" {
		t.Fatalf("synthesized request = %s %s, want GET /metrics/prometheus", last.Method, last.Path)
	}
	if want := "Bearer " + mcpEndpointToken; last.Authorization != want {
		t.Fatalf("synthesized Authorization = %q, want the caller's %q", last.Authorization, want)
	}
	if last.Host != "millivolt.internal" {
		t.Fatalf("synthesized Host = %q, want the named non-routable origin, never the request Host", last.Host)
	}
}

// TestMCPHTTPEndpointRefusesRedirectsInProcess proves the injected transport
// did not weaken the client: the in-process answer is a redirect, and it is
// refused rather than replayed.
func TestMCPHTTPEndpointRefusesRedirectsInProcess(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://elsewhere.example/credential")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	endpoint := httptest.NewServer(newMCPHandler(gate, redirect))
	t.Cleanup(endpoint.Close)

	session := connectMCPHTTP(t, endpoint.URL+"/mcp", mcpEndpointToken)
	result := mcpCall(t, session, "prometheus", map[string]any{})
	if !result.IsError {
		t.Fatalf("a redirect answer must be a tool error, got: %s", mcpText(t, result))
	}
	if text := mcpText(t, result); !strings.Contains(text, "redirect") {
		t.Fatalf("the refusal must name the redirect: %q", text)
	}
}

// connectMCPHTTP connects a real streamable HTTP MCP client that presents the
// fixture credential as a Bearer header. The standalone SSE GET is disabled
// because a stateless server answers it 405 by design.
func connectMCPHTTP(t *testing.T, endpoint, token string) *sdk.ClientSession {
	t.Helper()
	transport := &sdk.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			base:  http.DefaultTransport,
			token: token,
		}},
		DisableStandaloneSSE: true,
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "mcp-http-endpoint-test", Version: "0.0.0"}, nil).
		Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// bearerRoundTripper presents the fixture credential on every client request.
type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (b bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// mcpCall invokes one tool, failing on a transport error.
func mcpCall(t *testing.T, session *sdk.ClientSession, name string, arguments map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

// mcpStructured decodes a non-error tool result's structured content.
func mcpStructured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool returned an error result: %s", mcpText(t, result))
	}
	switch content := result.StructuredContent.(type) {
	case map[string]any:
		return content
	case []byte:
		var document map[string]any
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatalf("structured content is not a JSON object: %v", err)
		}
		return document
	default:
		t.Fatalf("structured content has unexpected type %T", result.StructuredContent)
		return nil
	}
}

// mcpText joins the text content of a tool result.
func mcpText(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// toolNameSet reduces a tools/list answer to its name set.
func toolNameSet(listed *sdk.ListToolsResult) map[string]bool {
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	return names
}

// registeredToolNames lists what the shared NewServer constructor registers,
// so an endpoint test can compare against the production registry instead of
// repeating its count or its names.
func registeredToolNames(t *testing.T) map[string]bool {
	t.Helper()
	service, err := mcp.NewServiceWithTransport(mcp.InProcessOrigin, mcpEndpointToken, mcp.DefaultLimits(), http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	server := mcp.NewServer(service)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "registry-probe", Version: "0.0.0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return toolNameSet(listed)
}
