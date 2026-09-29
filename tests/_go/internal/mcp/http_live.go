package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLiveHTTPEndpointServesTheSameTools drives the proxy's own streamable
// HTTP MCP endpoint against a private dev instance: a real URL-capable client
// initializes, lists the same tool surface the stdio entrypoint serves, and
// runs representative reads that must agree with the stdio entrypoint byte for
// byte, because both come from the one NewServer implementation. The auth
// boundary is exercised directly: Bearer-only on top of the operator gate,
// cookie sessions refused, and unregistered /mcp/ look-alikes owned by the
// reserved namespace rather than forwarded to inference. It is opt-in through
// MILLIVOLT_MCP_INTEGRATION like the other live test; see integration.go.
func TestLiveHTTPEndpointServesTheSameTools(t *testing.T) {
	live := devInstance(t)

	session := connectLiveHTTP(t, live.origin+"/mcp", integrationToken)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != len(registeredToolNames) {
		t.Fatalf("tools/list over HTTP = %d tools, want the pinned surface's %d", len(listed.Tools), len(registeredToolNames))
	}

	// One implementation, two entrypoints: with no traffic between the calls,
	// describe and records must return identical documents.
	for _, tc := range []struct {
		tool      string
		arguments map[string]any
	}{
		{"describe", map[string]any{}},
		{"records", map[string]any{"limit": 10}},
	} {
		viaHTTP := canonicalDocument(t, structured(t, call(t, session, tc.tool, tc.arguments)))
		viaStdio := canonicalDocument(t, live.invoke(t, tc.tool, tc.arguments))
		if viaHTTP != viaStdio {
			t.Fatalf("%s differs between the HTTP and stdio entrypoints:\nHTTP:  %s\nstdio: %s",
				tc.tool, viaHTTP, viaStdio)
		}
	}

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}`
	post := func(credential, cookie, path string) (*http.Response, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, live.origin+path, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		if cookie != "" {
			request.Header.Set("Cookie", cookie)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, string(body)
	}

	t.Run("no credential is challenged by the operator gate", func(t *testing.T) {
		response, _ := post("", "", "/mcp")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.StatusCode)
		}
		if got := response.Header.Get("WWW-Authenticate"); got != `Bearer realm="millivolt-operator"` {
			t.Fatalf("WWW-Authenticate = %q, want the operator Bearer challenge", got)
		}
	})

	t.Run("a wrong bearer is denied by the operator gate", func(t *testing.T) {
		const wrong = "wrong-live-endpoint-credential"
		response, body := post(wrong, "", "/mcp")
		// The gate owns wrong-credential status on every gated path, so this
		// is the same 401 /admin and /metrics answer with (403 while the
		// plane is unarmed). Cookie-admitted requests that present a wrong
		// Bearer are refused by the MCP layer below.
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want the gate's 401", response.StatusCode)
		}
		if strings.Contains(body, wrong) || strings.Contains(body, integrationToken) {
			t.Fatalf("denial echoed a credential: %q", body)
		}
	})

	cookie := mintLiveSessionCookie(t, live.origin)

	t.Run("a cookie session alone is refused", func(t *testing.T) {
		response, body := post("", cookie, "/mcp")
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cookie-only status = %d, want 403 (no internal call may be created from a session cookie)", response.StatusCode)
		}
		if !strings.Contains(body, "session cookie is not accepted") {
			t.Fatalf("cookie-only denial = %q, want the Bearer-only reason", body)
		}
		if strings.Contains(body, integrationToken) {
			t.Fatalf("cookie-only denial leaked the credential: %q", body)
		}
	})

	t.Run("a wrong bearer beside a session cookie is refused by the MCP layer", func(t *testing.T) {
		response, _ := post("wrong-beside-cookie-credential", cookie, "/mcp")
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 from the MCP layer", response.StatusCode)
		}
	})

	t.Run("an unregistered look-alike stays in the reserved namespace", func(t *testing.T) {
		response, body := post(integrationToken, "", "/mcp/xyz")
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 from the reserved /mcp/ namespace, never inference", response.StatusCode)
		}
		if !strings.Contains(body, "404 page not found") {
			t.Fatalf("look-alike body = %q, want the reserved namespace's 404", body)
		}
		if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
			t.Fatalf("look-alike Content-Type = %q, want the reserved handler's text/plain, not an inference answer", contentType)
		}
		response, _ = post("", "", "/mcp/xyz")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("credential-less look-alike status = %d, want 401 (the namespace is gated)", response.StatusCode)
		}
	})
}

// connectLiveHTTP connects a real streamable HTTP MCP client that presents the
// dev instance's Bearer credential. The standalone SSE GET is disabled because
// the endpoint is stateless and answers it 405 by design.
func connectLiveHTTP(t *testing.T, endpoint, token string) *sdk.ClientSession {
	t.Helper()
	transport := &sdk.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: liveBearerRoundTripper{
			base:  http.DefaultTransport,
			token: token,
		}},
		DisableStandaloneSSE: true,
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "mcp-live-http-test", Version: "0.0.0"}, nil).
		Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// liveBearerRoundTripper presents the fixture credential on every client
// request.
type liveBearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (b liveBearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// mintLiveSessionCookie performs the operator handshake and returns the
// session cookie as a request header value.
func mintLiveSessionCookie(t *testing.T, origin string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, origin+"/admin/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("session handshake status = %d, want 204", response.StatusCode)
	}
	// The handshake mints exactly one cookie; its name is owned by the proxy
	// gate, so this test carries the value without restating the name.
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session handshake minted %d cookies, want 1", len(cookies))
	}
	return cookies[0].Name + "=" + cookies[0].Value
}

// canonicalDocument renders a tool document with sorted keys so two equal
// results compare equal regardless of map iteration order.
func canonicalDocument(t *testing.T, document map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
