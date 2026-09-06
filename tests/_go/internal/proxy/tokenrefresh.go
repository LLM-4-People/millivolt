package proxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// mkJWT builds a JWT-shaped token with the given exp. The header/signature
// segments are opaque: the proxy reads only the payload claims (the upstream
// validates the signature), so tests can pin expiry without real keys.
func mkJWT(t *testing.T, exp int64) string {
	t.Helper()
	seg := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return seg(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		seg(map[string]any{"sub": "user-1", "exp": exp}) + ".sig"
}

func TestJWTExpiring(t *testing.T) {
	now := time.Unix(1000000, 0)
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"already expired", mkJWT(t, now.Unix()-100), true},
		{"within margin", mkJWT(t, now.Unix()+30), true},
		{"past margin", mkJWT(t, now.Unix()+120), false},
		{"no exp claim", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c", false},
		{"exp zero", mkJWT(t, 0), false},
		{"not a jwt", "xai-plain-api-key", false},
		{"garbage payload", "a.!!!.c", false},
		{"two segments", "a.b", false},
	}
	for _, c := range cases {
		if got := jwtExpiring(c.token, now); got != c.want {
			t.Errorf("%s: jwtExpiring = %v, want %v", c.name, got, c.want)
		}
	}
}

// mockTokenServer stands in for a provider token endpoint and records the
// requests it received, replying with the configured status/body.
type mockTokenServer struct {
	srv     *httptest.Server
	calls   int
	grants  []string
	rTokens []string
	cIDs    []string
	bodies  []string
	status  int
	resp    string
}

func newMockTokenServer(t *testing.T, status int, resp string) *mockTokenServer {
	t.Helper()
	m := &mockTokenServer{status: status, resp: resp}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// FormValue must run before the body is drained by ReadAll.
		grant, rt, cid := r.FormValue("grant_type"), r.FormValue("refresh_token"), r.FormValue("client_id")
		body, _ := io.ReadAll(r.Body)
		m.calls++
		m.grants = append(m.grants, grant)
		m.rTokens = append(m.rTokens, rt)
		m.cIDs = append(m.cIDs, cid)
		m.bodies = append(m.bodies, string(body))
		w.WriteHeader(m.status)
		w.Write([]byte(m.resp))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func withMockXAI(t *testing.T, m *mockTokenServer) {
	t.Helper()
	oldEndpoint, oldClient := xaiTokenEndpoint, tokenHTTPClient
	xaiTokenEndpoint = m.srv.URL
	tokenHTTPClient = m.srv.Client()
	t.Cleanup(func() { xaiTokenEndpoint, tokenHTTPClient = oldEndpoint, oldClient })
}

func TestRefreshDeviceFlowGrant(t *testing.T) {
	m := newMockTokenServer(t, 200, `{"access_token":"fresh-access","refresh_token":"fresh-rt","expires_in":3600}`)
	withMockXAI(t, m)
	tg := &target{provider: "x.ai", baseURL: "https://api.x.ai/v1"}
	fresh, err := refreshAccessToken(t.Context(), tg, "rt-1")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.access != "fresh-access" || fresh.refresh != "fresh-rt" {
		t.Errorf("fresh = %+v, want access fresh-access / refresh fresh-rt", fresh)
	}
	if m.calls != 1 || m.grants[0] != "refresh_token" || m.rTokens[0] != "rt-1" {
		t.Errorf("token call = grants %v rTokens %v", m.grants, m.rTokens)
	}
	if m.cIDs[0] != xaiOAuthClientID {
		t.Errorf("client_id = %q, want the grok CLI OAuth client", m.cIDs[0])
	}
	if !fresh.handback {
		t.Error("x.ai refresh must be a handback mechanism (client adopts the fresh pair)")
	}
}

func TestRefreshExchangesOnTargetHost(t *testing.T) {
	calls := 0
	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/exchange_user_api_key" {
			body, _ := io.ReadAll(r.Body)
			calls++
			gotAuth = r.Header.Get("Authorization")
			gotBody = string(body)
			w.Write([]byte(`{"accessToken":"cur-fresh","refreshToken":"cur-rt2"}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	tg := &target{provider: "cursor.sh", baseURL: upstream.URL}
	fresh, err := refreshAccessToken(t.Context(), tg, "cursor-rt")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.access != "cur-fresh" || fresh.refresh != "cur-rt2" {
		t.Errorf("fresh = %+v, want cur-fresh/cur-rt2", fresh)
	}
	if fresh.handback {
		t.Error("cursor refresh must stay in place, not handback")
	}
	if calls != 1 || !strings.Contains(gotAuth, "Bearer cursor-rt") || strings.TrimSpace(gotBody) != "{}" {
		t.Errorf("exchange call: n=%d auth=%q body=%q", calls, gotAuth, gotBody)
	}
}

func TestRefreshUnknownProviderRejected(t *testing.T) {
	_, err := refreshAccessToken(t.Context(), &target{provider: "127.0.0.1:1", baseURL: "http://127.0.0.1:1"}, "rt")
	if err == nil || !strings.Contains(err.Error(), "no token refresh mechanism") {
		t.Errorf("err = %v, want provider-deny error", err)
	}
}

func TestRefreshKeyIfExpiredFailOpen(t *testing.T) {
	// 500 from the token endpoint → original key, no refresh signal.
	m := newMockTokenServer(t, 500, `{"error":"boom"}`)
	withMockXAI(t, m)
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Proxy-Refresh-Token", "rt-1")
	expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
	tg := &target{provider: "x.ai", baseURL: "https://api.x.ai/v1"}
	got, fresh, ok := s.refreshKeyIfExpired(r, tg, expired)
	if ok || fresh.access != "" {
		t.Errorf("ok=%v fresh=%+v, want fail-open passthrough", ok, fresh)
	}
	if got != expired {
		t.Error("original key not preserved on failed exchange")
	}
}

func TestRefreshKeyIfExpiredGates(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
	tg := &target{provider: "x.ai", baseURL: "https://api.x.ai/v1"}

	// No client refresh token → untouched.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if _, _, ok := s.refreshKeyIfExpired(r, tg, expired); ok {
		t.Error("refreshed without X-Proxy-Refresh-Token")
	}
	// Valid (non-expiring) token → untouched even with a refresh token.
	r.Header.Set("X-Proxy-Refresh-Token", "rt")
	valid := mkJWT(t, time.Now().Add(time.Hour).Unix())
	if _, _, ok := s.refreshKeyIfExpired(r, tg, valid); ok {
		t.Error("refreshed a still-valid token")
	}
	// Plain API key → never touched.
	if _, _, ok := s.refreshKeyIfExpired(r, tg, "xai-abc123"); ok {
		t.Error("refreshed a non-JWT key")
	}
	// Unknown provider → untouched (deny by default).
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set("X-Proxy-Refresh-Token", "rt")
	other := &target{provider: "10.0.0.5:8000", baseURL: "http://10.0.0.5:8000/v1"}
	if _, _, ok := s.refreshKeyIfExpired(r2, other, expired); ok {
		t.Error("refreshed for a provider with no known mechanism")
	}
	// Disabled by config → untouched.
	cfg := config.Default()
	cfg.AutoTokenRefresh = false
	sOff := New(cfg, metrics.Noop{})
	if _, _, ok := sOff.refreshKeyIfExpired(r, tg, expired); ok {
		t.Error("refreshed with auto_token_refresh off")
	}
}

// End-to-end wiring through ServeHTTP: the mock upstream's host maps to a
// provider label with no refresh mechanism, so this pins the PASSTHROUGH
// contract - an expired JWT + refresh token still proxies verbatim, and the
// client-supplied X-Proxy-Refresh-Token never rides upstream.
func TestRefreshTokenHeaderNeverUpstream(t *testing.T) {
	var seenRefresh string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRefresh = r.Header.Get("X-Proxy-Refresh-Token")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
	req.Header.Set("Authorization", "Bearer "+expired)
	req.Header.Set("X-Proxy-Refresh-Token", "client-rt")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seenRefresh != "" {
		t.Errorf("X-Proxy-Refresh-Token leaked upstream: %q", seenRefresh)
	}
	if resp.Header.Get("X-Proxy-Access-Token") != "" {
		t.Error("refresh headers set for a provider with no refresh mechanism")
	}
}

// resolveKey keeps a non-handback mechanism IN-PLACE: the fresh token is
// returned for upstream use and the pair rides the response headers - never
// the handback 401. (The handback decision is mechanism-owned; the caller
// stays label-blind.)
func TestResolveKeyCursorInPlace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/exchange_user_api_key" {
			w.Write([]byte(`{"accessToken":"cur-fresh","refreshToken":"cur-rt2"}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Proxy-Refresh-Token", "client-rt")
	expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
	tg := &target{provider: "cursor.sh", baseURL: upstream.URL}
	rec := httptest.NewRecorder()
	key, ok := s.resolveKey(rec, r, tg, expired)
	if !ok || key != "cur-fresh" {
		t.Errorf("ok=%v key=%q, want in-place fresh key", ok, key)
	}
	if rec.Header().Get("X-Proxy-Access-Token") != "cur-fresh" ||
		rec.Header().Get("X-Proxy-Refresh-Token") != "cur-rt2" {
		t.Error("fresh pair headers not set on in-place refresh")
	}
	if rec.Code == http.StatusUnauthorized {
		t.Error("handback response written for an in-place mechanism")
	}
}

// End-to-end handback through ServeHTTP: an expired JWT + the client's
// refresh token against xAI is answered DIRECTLY with 401 + the fresh pair in
// the response headers. The base URL is the real api.x.ai: any accidental
// proxying would leave the test process and fail the status assertion. When
// the provider did not rotate the refresh token, no X-Proxy-Refresh-Token
// header is set. (Model discovery is exempt - see
// TestModelsRefreshStaysTransparent.)
func TestTokenHandbackReturnsFreshPairWithoutProxying(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		rotated bool
	}{
		{"chat completions", http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[]}`, true},
		{"no rotated refresh token", http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			respBody := `{"access_token":"fresh-access","refresh_token":"fresh-rt"}`
			if !c.rotated {
				respBody = `{"access_token":"fresh-access"}`
			}
			m := newMockTokenServer(t, 200, respBody)
			withMockXAI(t, m)
			srv := proxyServer(t)
			defer srv.Close()

			var body io.Reader
			if c.body != "" {
				body = strings.NewReader(c.body)
			}
			req, _ := http.NewRequest(c.method, srv.URL+c.path, body)
			if c.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
			req.Header.Set("Authorization", "Bearer "+expired)
			req.Header.Set("X-Proxy-Refresh-Token", "client-rt")
			req.Header.Set("X-Proxy-Base-URL", "https://api.x.ai/v1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 handback", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Proxy-Access-Token"); got != "fresh-access" {
				t.Errorf("X-Proxy-Access-Token = %q, want fresh-access", got)
			}
			wantRT := ""
			if c.rotated {
				wantRT = "fresh-rt"
			}
			if got := resp.Header.Get("X-Proxy-Refresh-Token"); got != wantRT {
				t.Errorf("X-Proxy-Refresh-Token = %q, want %q", got, wantRT)
			}
			var out struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if json.Unmarshal(raw, &out) != nil || out.Error.Code != "token_expired" ||
				out.Error.Type != "authentication_error" {
				t.Errorf("body = %s, want an authentication_error/token_expired envelope", raw)
			}
			if m.calls != 1 {
				t.Errorf("token endpoint calls = %d, want exactly one exchange", m.calls)
			}
		})
	}
}

// Model discovery stays TRANSPARENT even for the handback mechanism: a models
// fetch refreshes in place (the fresh token is what goes upstream) and the
// response carries no handback status and no fresh-pair headers - the tooling
// that fetches models never implements the 401+headers contract. The mock
// upstream's IP host is aliased to the xAI label so the mechanism binds
// without a real network hop.
func TestModelsRefreshStaysTransparent(t *testing.T) {
	m := newMockTokenServer(t, 200, `{"access_token":"fresh-access","refresh_token":"fresh-rt"}`)
	withMockXAI(t, m)
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ProviderAliases = map[string]string{providerFromURL(u): "x.ai"}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
	req.Header.Set("Authorization", "Bearer "+expired)
	req.Header.Set("X-Proxy-Refresh-Token", "client-rt")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want transparent 200 (body %s)", resp.StatusCode, raw)
	}
	if gotAuth != "Bearer fresh-access" {
		t.Errorf("upstream Authorization = %q, want the fresh access token used in place", gotAuth)
	}
	if resp.Header.Get("X-Proxy-Access-Token") != "" || resp.Header.Get("X-Proxy-Refresh-Token") != "" {
		t.Error("fresh-pair headers returned on a transparent models refresh")
	}
	if m.calls != 1 {
		t.Errorf("token endpoint calls = %d, want exactly one exchange", m.calls)
	}
}

// The proxy's xAI client_id is the same OAuth registration scripts/grok-login.sh
// uses (one device-flow cohort per account): pin the two together so neither
// can drift.
func TestAutoRefreshClientIDMatchesLoginScript(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/grok-login.sh")
	if err != nil {
		t.Skipf("grok-login.sh not reachable from test cwd: %v", err)
	}
	if !strings.Contains(string(raw), xaiOAuthClientID) {
		t.Errorf("scripts/grok-login.sh no longer carries the proxy's client id %s", xaiOAuthClientID)
	}
}
