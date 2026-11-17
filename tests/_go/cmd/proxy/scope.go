package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
	"github.com/LLM-4-People/millivolt/internal/plugins"
)

const scopeTestFullToken = "full-operator-fixture-credential"

func scopeTestStore(t *testing.T, token string, scopes ...string) *plugins.Credentials {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := fmt.Sprintf(`{"credentials":[{"id":"analyzer","hash":%q,"scopes":[`, hex.EncodeToString(sum[:]))
	for i, scope := range scopes {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf("%q", scope)
	}
	body += `]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := plugins.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func scopeTestHandler(store *plugins.Credentials) http.Handler {
	gate := newOperatorGateWithScopes(scopeTestFullToken, store)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return protectOperatorRequests(next, gate)
}

func requestWithToken(method, path, token string, internal bool) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if internal {
		r = r.WithContext(markInternalMCP(r.Context()))
	}
	return r
}

func TestScopedCredentialMCPBoundary(t *testing.T) {
	const scoped = "plugin-scoped-fixture-credential"
	h := scopeTestHandler(scopeTestStore(t, scoped, "read:metrics"))

	cases := []struct {
		name     string
		method   string
		path     string
		token    string
		internal bool
		want     int
	}{
		{"scoped MCP admitted", http.MethodPost, "/mcp", scoped, false, http.StatusOK},
		{"scoped direct metrics denied", http.MethodGet, "/metrics/bootstrap", scoped, false, http.StatusForbidden},
		{"scoped internal metrics admitted", http.MethodGet, "/metrics/bootstrap", scoped, true, http.StatusOK},
		{"scoped internal mutation denied", http.MethodPost, "/admin/pause", scoped, true, http.StatusForbidden},
		{"full token direct metrics admitted", http.MethodGet, "/metrics/bootstrap", scopeTestFullToken, false, http.StatusOK},
		{"wrong token denied", http.MethodGet, "/metrics/bootstrap", "wrong-token", false, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, requestWithToken(tc.method, tc.path, tc.token, tc.internal))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestAuthorizeMCPScopes(t *testing.T) {
	const scoped = "plugin-scoped-fixture-credential"
	gate := newOperatorGateWithScopes(scopeTestFullToken, scopeTestStore(t, scoped, "read:metrics"))

	full, scopes, ok := gate.authorizeMCP(requestWithToken(http.MethodPost, "/mcp", scopeTestFullToken, false))
	if !ok || full != scopeTestFullToken || !scopes.Allows(mcp.CapabilityAdminHistory) {
		t.Fatalf("full token: ok=%v scopes=%v", ok, scopes.Strings())
	}
	token, scopes, ok := gate.authorizeMCP(requestWithToken(http.MethodPost, "/mcp", scoped, false))
	if !ok || token != scoped || !scopes.Allows(mcp.CapabilityMetrics) || scopes.Allows(mcp.CapabilityAdminHistory) {
		t.Fatalf("scoped token: ok=%v scopes=%v", ok, scopes.Strings())
	}
	if _, _, ok := gate.authorizeMCP(requestWithToken(http.MethodPost, "/mcp", "wrong-token", false)); ok {
		t.Fatal("wrong token must not authorize")
	}
}

func TestInternalRouteScopeMapping(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   mcp.Capability
	}{
		{http.MethodGet, "/metrics/bootstrap", mcp.CapabilityMetrics},
		{http.MethodGet, "/metrics/query", mcp.CapabilityRecords},
		{http.MethodGet, "/admin/config", mcp.CapabilityConfig},
		{http.MethodPost, "/admin/config", mcp.CapabilityAdminConfig},
		{http.MethodPost, "/admin/pause", mcp.CapabilityControl},
		{http.MethodGet, "/admin/debug", mcp.CapabilityAudit},
		{http.MethodPost, "/admin/purge", mcp.CapabilityAdminHistory},
	}
	for _, tc := range cases {
		got, ok := internalRouteScope(httptest.NewRequest(tc.method, tc.path, nil))
		if !ok || got != tc.want {
			t.Errorf("internalRouteScope(%s %s) = %q, %v; want %q", tc.method, tc.path, got, ok, tc.want)
		}
	}
	if _, ok := internalRouteScope(httptest.NewRequest(http.MethodGet, "/v1/models", nil)); ok {
		t.Error("unknown internal route must not be mapped")
	}
}

func TestSpoofedInternalHeaderIsIgnored(t *testing.T) {
	const scoped = "plugin-scoped-fixture-credential"
	h := scopeTestHandler(scopeTestStore(t, scoped, "read:metrics"))
	r := httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil)
	r.Header.Set("Authorization", "Bearer "+scoped)
	r.Header.Set("X-Millivolt-Internal-MCP", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed internal header status = %d, want 403", rec.Code)
	}
}
