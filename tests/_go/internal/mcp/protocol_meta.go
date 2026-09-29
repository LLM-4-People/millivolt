package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The per-request protocol form (2026-07-28) is the SDK's, and its HTTP
// standard headers are not exported, so the test carries their names. The
// namespaced metadata keys ARE exported by the SDK, and the test reads them
// from there: the guide's prose is compared with the SDK constants below, so
// a guide that names the plain, un-namespaced keys - which the SDK does not
// read - fails here.
const (
	perRequestVersionHeader = "Mcp-Protocol-Version"
	perRequestMethodHeader  = "Mcp-Method"
	perRequestNameHeader    = "Mcp-Name"
	perRequestVersion       = "2026-07-28"
)

// TestGuideStatesThePerRequestProtocolForm pins the guide's sentence about the
// per-request form to the SDK's real keys and headers. The sentence used to
// name `_meta.protocolVersion` and `_meta.clientCapabilities`, which the SDK
// never reads: a client that followed it answered 400. The check is on both
// sides: the namespaced keys, the optional clientInfo and the Mcp-Name
// requirement must be stated, and the stale plain spellings must be gone.
func TestGuideStatesThePerRequestProtocolForm(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	const heading = "`2026-07-28` is served only through the SDK's per-request metadata form:"
	index := strings.Index(string(guide), heading)
	if index < 0 {
		t.Fatalf("docs/mcp.md must state %q", heading)
	}
	bullet := string(guide)[index:]
	if end := strings.Index(bullet, "\n- "); end >= 0 {
		bullet = bullet[:end]
	}
	for _, want := range []struct{ what, token string }{
		{"the SDK's namespaced protocol version key", sdk.MetaKeyProtocolVersion},
		{"the SDK's namespaced client capabilities key", sdk.MetaKeyClientCapabilities},
		{"the SDK's optional namespaced client info key", sdk.MetaKeyClientInfo},
		{"the Mcp-Protocol-Version header", perRequestVersionHeader},
		{"the Mcp-Method header", perRequestMethodHeader},
		{"the Mcp-Name header tools/call requires", perRequestNameHeader},
		{"the tools/call method that requires Mcp-Name", "tools/call"},
		{"the optional status of clientInfo", "optional"},
	} {
		if !strings.Contains(bullet, want.token) {
			t.Fatalf("docs/mcp.md must name %s (%q) in the per-request protocol form: %q", want.what, want.token, bullet)
		}
	}
	// The plain spellings are the exact drift this guard exists for: the SDK
	// reads only the namespaced keys, so documenting the plain ones is wrong
	// even though the words look right.
	for _, stale := range []string{"_meta.protocolVersion", "_meta.clientCapabilities"} {
		if strings.Contains(bullet, stale) {
			t.Fatalf("docs/mcp.md names the plain key %q, which the SDK does not read: %q", stale, bullet)
		}
	}
}

// TestPerRequestProtocolFormMatchesTheSDK drives the real /mcp handler with
// raw JSON-RPC requests and pins the per-request form end to end: the
// namespaced triple plus Mcp-Method is accepted, the plain keys are refused
// with -32602, tools/call additionally requires Mcp-Name (refused with -32020
// without it), and clientInfo is optional while clientCapabilities is not.
// This is the live behavior the guide sentence claims, so a dependency change
// or an endpoint regression fails here rather than in an operator's client.
func TestPerRequestProtocolFormMatchesTheSDK(t *testing.T) {
	if !slices.Contains(sdk.SupportedProtocolVersions(), perRequestVersion) {
		t.Fatalf("the bundled SDK no longer supports %s; the guide and this test must be re-checked (supported: %v)",
			perRequestVersion, sdk.SupportedProtocolVersions())
	}

	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, schemaPath, `[]`)
	service := newTestService(t, proxy, Limits{})
	endpoint := httptest.NewServer(NewHTTPHandler(func(*http.Request) *Service { return service }))
	t.Cleanup(endpoint.Close)

	// namespaced renders the triple's required pair plus any extra key, so a
	// case can omit clientCapabilities or add clientInfo deliberately.
	namespaced := func(extra string) string {
		meta := `"` + sdk.MetaKeyProtocolVersion + `":"` + perRequestVersion +
			`","` + sdk.MetaKeyClientCapabilities + `":{}`
		if extra != "" {
			meta += "," + extra
		}
		return meta
	}
	plain := func() string {
		return `"protocolVersion":"` + perRequestVersion + `","clientCapabilities":{}`
	}
	listParams := func(meta string) string { return `{"_meta":{` + meta + `}}` }
	callParams := func(meta string) string {
		return `{"name":"describe","arguments":{},"_meta":{` + meta + `}}`
	}
	headers := func(fields ...string) map[string]string {
		got := map[string]string{perRequestVersionHeader: perRequestVersion}
		for i := 0; i+1 < len(fields); i += 2 {
			got[fields[i]] = fields[i+1]
		}
		return got
	}

	for _, tc := range []struct {
		name    string
		method  string
		params  string
		headers map[string]string
		want    int
		wantErr int
	}{
		{
			name:    "the namespaced triple with Mcp-Method is accepted",
			method:  "tools/list",
			params:  listParams(namespaced("")),
			headers: headers(perRequestMethodHeader, "tools/list"),
			want:    http.StatusOK,
		},
		{
			name:    "the plain keys are refused",
			method:  "tools/list",
			params:  listParams(plain()),
			headers: headers(perRequestMethodHeader, "tools/list"),
			want:    http.StatusBadRequest,
			wantErr: -32602,
		},
		{
			name:    "clientCapabilities is required",
			method:  "tools/list",
			params:  listParams(`"` + sdk.MetaKeyProtocolVersion + `":"` + perRequestVersion + `"`),
			headers: headers(perRequestMethodHeader, "tools/list"),
			want:    http.StatusBadRequest,
			wantErr: -32602,
		},
		{
			name:    "Mcp-Method is required",
			method:  "tools/list",
			params:  listParams(namespaced("")),
			headers: headers(),
			want:    http.StatusBadRequest,
			wantErr: -32020,
		},
		{
			name:    "tools/call without Mcp-Name is refused",
			method:  "tools/call",
			params:  callParams(namespaced("")),
			headers: headers(perRequestMethodHeader, "tools/call"),
			want:    http.StatusBadRequest,
			wantErr: -32020,
		},
		{
			name:    "tools/call with the matching Mcp-Name is accepted and clientInfo is optional",
			method:  "tools/call",
			params:  callParams(namespaced("")),
			headers: headers(perRequestMethodHeader, "tools/call", perRequestNameHeader, "describe"),
			want:    http.StatusOK,
		},
		{
			name:    "tools/call carrying clientInfo is accepted",
			method:  "tools/call",
			params:  callParams(namespaced(`"` + sdk.MetaKeyClientInfo + `":{"name":"probe","version":"0"}`)),
			headers: headers(perRequestMethodHeader, "tools/call", perRequestNameHeader, "describe"),
			want:    http.StatusOK,
		},
		{
			name:    "a mismatched Mcp-Name is refused",
			method:  "tools/call",
			params:  callParams(namespaced("")),
			headers: headers(perRequestMethodHeader, "tools/call", perRequestNameHeader, "records"),
			want:    http.StatusBadRequest,
			wantErr: -32020,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"` + tc.method + `","params":` + tc.params + `}`
			request, err := http.NewRequest(http.MethodPost, endpoint.URL, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			for name, value := range tc.headers {
				request.Header.Set(name, value)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reply, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", response.StatusCode, tc.want, reply)
			}
			if tc.want == http.StatusOK {
				if !strings.Contains(string(reply), `"result"`) {
					t.Fatalf("accepted request must carry a JSON-RPC result: %q", reply)
				}
				return
			}
			var document struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(reply, &document); err != nil {
				t.Fatalf("refusal body is not a JSON-RPC error: %v (%q)", err, reply)
			}
			if document.Error.Code != tc.wantErr {
				t.Fatalf("error code = %d, want %d (body %q)", document.Error.Code, tc.wantErr, reply)
			}
		})
	}
}
