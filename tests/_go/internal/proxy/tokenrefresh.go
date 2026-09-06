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
	"sync/atomic"
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

func withMockTokenEndpoint(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldEndpoint, oldClient := xaiTokenEndpoint, tokenHTTPClient
	xaiTokenEndpoint = srv.URL + "/oauth2/token"
	tokenHTTPClient = srv.Client()
	t.Cleanup(func() { xaiTokenEndpoint, tokenHTTPClient = oldEndpoint, oldClient })
}

var tokenRefreshMechanisms = []struct {
	provider, accessField, refreshField string
}{
	{"x.ai", "access_token", "refresh_token"},
	{"cursor.sh", "accessToken", "refreshToken"},
}

func tokenResponse(t *testing.T, accessField, refreshField, access, refresh string) string {
	t.Helper()
	fields := map[string]string{accessField: access}
	if refresh != "" {
		fields[refreshField] = refresh
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type capturedRefreshRequest struct {
	capturedRequest
	refreshToken string
}

// One loopback fixture owns token and inference endpoints. Even a failed
// handback regression cannot send a request to a real provider. Atomic captures
// let assertions inspect completed HTTP requests without handler data races.
type tokenRefreshFixture struct {
	proxy     *Server
	records   *metrics.Buffer
	baseURL   string
	exchanges atomic.Int64
	relays    atomic.Int64
	lastRelay atomic.Pointer[capturedRefreshRequest]
}

func newTokenRefreshFixture(t *testing.T, provider string, status int, response string) *tokenRefreshFixture {
	t.Helper()
	f := &tokenRefreshFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" || r.URL.Path == "/auth/exchange_user_api_key" {
			f.exchanges.Add(1)
			w.WriteHeader(status)
			w.Write([]byte(response))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fixture request: %v", err)
		}
		f.lastRelay.Store(&capturedRefreshRequest{
			capturedRequest: capturedRequest{path: r.URL.Path, authHeader: r.Header.Get("Authorization"), body: string(body)},
			refreshToken:    r.Header.Get("X-Proxy-Refresh-Token"),
		})
		f.relays.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"fixture response"}}]}`))
	}))
	t.Cleanup(upstream.Close)
	withMockTokenEndpoint(t, upstream)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ProviderAliases = map[string]string{providerFromURL(u): provider}
	f.records = metrics.NewBuffer(cfg.HistorySize)
	f.proxy = New(cfg, f.records)
	f.baseURL = upstream.URL
	return f
}

func (f *tokenRefreshFixture) request(method, path, body, key string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Proxy-Refresh-Token", "client-rt")
	r.Header.Set("X-Proxy-Base-URL", f.baseURL)
	return r
}

func (f *tokenRefreshFixture) assertNoAccounting(t *testing.T) {
	t.Helper()
	snap := f.records.SnapshotSince(0)
	if len(snap.Records) != 0 || len(snap.InFlightRecords) != 0 || snap.PendingRevision != 0 ||
		snap.Counters.TotalReq != 0 || snap.Counters.InFlight != 0 {
		t.Errorf("refresh-only request changed finalized or live accounting: %+v", snap)
	}
}

func TestRefreshDeviceFlowGrant(t *testing.T) {
	m := newMockTokenServer(t, 200, `{"access_token":"fresh-access","refresh_token":"fresh-rt","expires_in":3600}`)
	withMockTokenEndpoint(t, m.srv)
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
	for _, mechanism := range tokenRefreshMechanisms {
		for _, tc := range []struct {
			name, body string
			status     int
		}{
			{"rejected", `{"error":"fixture failure"}`, http.StatusUnauthorized},
			{"server failure", `{"error":"fixture failure"}`, http.StatusInternalServerError},
			{"malformed", `{"`, http.StatusOK},
			{"missing access", `{}`, http.StatusOK},
			{"wrong type", `{"` + mechanism.accessField + `":42}`, http.StatusOK},
		} {
			t.Run(mechanism.provider+"/"+tc.name, func(t *testing.T) {
				f := newTokenRefreshFixture(t, mechanism.provider, tc.status, tc.body)
				expired := mkJWT(t, time.Now().Add(-time.Hour).Unix())
				r := f.request(http.MethodPost, "/v1/chat/completions", `{"model":"fixture-model","messages":[]}`, expired)
				w := httptest.NewRecorder()
				f.proxy.ServeHTTP(w, r)
				if w.Code != http.StatusOK || f.exchanges.Load() != 1 || f.relays.Load() != 1 {
					t.Fatalf("status=%d exchanges=%d relays=%d, want 200/1/1", w.Code, f.exchanges.Load(), f.relays.Load())
				}
				if got := f.lastRelay.Load(); got.authHeader != "Bearer "+expired || got.refreshToken != "" {
					t.Error("failed exchange changed the original key or leaked the refresh token")
				}
				if w.Header().Get("X-Proxy-Access-Token") != "" || w.Header().Get("X-Proxy-Refresh-Token") != "" {
					t.Error("failed exchange returned refreshed credential headers")
				}
			})
		}
	}
}

func TestRefreshKeyIfExpiredGates(t *testing.T) {
	for _, mechanism := range tokenRefreshMechanisms {
		for _, name := range []string{"missing refresh", "blank refresh", "valid access", "plain key", "missing key", "unknown provider", "disabled"} {
			t.Run(mechanism.provider+"/"+name, func(t *testing.T) {
				body := tokenResponse(t, mechanism.accessField, mechanism.refreshField, "fresh-access", "fresh-rt")
				f := newTokenRefreshFixture(t, mechanism.provider, http.StatusOK, body)
				key := mkJWT(t, time.Now().Add(-time.Hour).Unix())
				r := f.request(http.MethodPost, "/v1/chat/completions", "", key)
				tg := &target{provider: mechanism.provider, baseURL: f.baseURL}
				switch name {
				case "missing refresh":
					r.Header.Del("X-Proxy-Refresh-Token")
				case "blank refresh":
					r.Header.Set("X-Proxy-Refresh-Token", "  ")
				case "valid access":
					key = mkJWT(t, time.Now().Add(time.Hour).Unix())
				case "plain key":
					key = "fixture-api-key"
				case "missing key":
					key = ""
				case "unknown provider":
					tg.provider = "fixture.invalid"
				case "disabled":
					cfg := *f.proxy.cfg()
					cfg.AutoTokenRefresh = false
					f.proxy.Reload(&cfg)
				}
				w := httptest.NewRecorder()
				got, ok := f.proxy.resolveKey(w, r, tg, key)
				if !ok || got != key || f.exchanges.Load() != 0 || w.Body.Len() != 0 {
					t.Error("inapplicable refresh did not preserve the original key without an exchange or response")
				}
				if w.Header().Get("X-Proxy-Access-Token") != "" || w.Header().Get("X-Proxy-Refresh-Token") != "" {
					t.Error("inapplicable refresh returned refreshed credential headers")
				}
			})
		}
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

// Every successful inference refresh stops at the shared handback boundary.
func TestResolveKeyCursorHandback(t *testing.T) {
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
	if ok || key != "" {
		t.Errorf("ok=%v key=%q, want handback without an upstream key", ok, key)
	}
	if rec.Header().Get("X-Proxy-Access-Token") != "cur-fresh" ||
		rec.Header().Get("X-Proxy-Refresh-Token") != "cur-rt2" {
		t.Error("refreshed credential headers not set on handback")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 handback", rec.Code)
	}
}

// All inference routes stop before native translation, admission, live
// publication or final accounting. Only the provider exchange goes upstream.
func TestTokenHandbackReturnsFreshPairWithoutProxying(t *testing.T) {
	for _, mechanism := range tokenRefreshMechanisms {
		for _, refresh := range []string{"fresh-rt", ""} {
			for _, route := range []struct{ name, path, body, format string }{
				{"chat", "/v1/chat/completions", `{"model":"fixture-model","messages":[]}`, ""},
				{"stream", "/v1/chat/completions", `{"model":"fixture-model","messages":[],"stream":true}`, ""},
				{"responses", "/v1/responses", `{"model":"fixture-model","input":"hello"}`, ""},
				{"cursor bridge", "/v1/chat/completions", `{"model":"fixture-model","messages":[],"stream":true}`, "cursor"},
			} {
				t.Run(mechanism.provider+"/"+refresh+"/"+route.name, func(t *testing.T) {
					fresh := mkJWT(t, time.Now().Add(time.Hour).Unix())
					f := newTokenRefreshFixture(t, mechanism.provider, http.StatusOK,
						tokenResponse(t, mechanism.accessField, mechanism.refreshField, fresh, refresh))
					r := f.request(http.MethodPost, route.path, route.body, mkJWT(t, time.Now().Add(-time.Hour).Unix()))
					r.Header.Set("X-Proxy-Format", route.format)
					w := httptest.NewRecorder()
					f.proxy.ServeHTTP(w, r)
					if w.Code != http.StatusUnauthorized || f.exchanges.Load() != 1 || f.relays.Load() != 0 {
						t.Fatalf("status=%d exchanges=%d relays=%d, want 401/1/0", w.Code, f.exchanges.Load(), f.relays.Load())
					}
					if w.Header().Get("X-Proxy-Access-Token") != fresh || w.Header().Get("X-Proxy-Refresh-Token") != refresh {
						t.Error("handback did not return exactly the exchanged credentials")
					}
					var out struct {
						Error struct {
							Code string `json:"code"`
							Type string `json:"type"`
						} `json:"error"`
					}
					if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Error.Code != "token_expired" || out.Error.Type != "authentication_error" {
						t.Errorf("body = %s, want authentication_error/token_expired", w.Body.String())
					}
					f.assertNoAccounting(t)
					if route.name == "chat" {
						retry := f.request(http.MethodPost, route.path, route.body, w.Header().Get("X-Proxy-Access-Token"))
						if refresh != "" {
							retry.Header.Set("X-Proxy-Refresh-Token", refresh)
						}
						w = httptest.NewRecorder()
						f.proxy.ServeHTTP(w, retry)
						if w.Code != http.StatusOK || f.exchanges.Load() != 1 || f.relays.Load() != 1 {
							t.Fatalf("adopted-key retry: status=%d exchanges=%d relays=%d, want 200/1/1", w.Code, f.exchanges.Load(), f.relays.Load())
						}
						got := f.lastRelay.Load()
						if got.authHeader != "Bearer "+fresh || got.body != route.body || got.refreshToken != "" {
							t.Error("retry did not forward the adopted access key and original body without the refresh token")
						}
						if len(f.records.Snapshot()) != 1 || len(f.records.PendingRecords()) != 0 {
							t.Error("only the completed inference retry should be recorded")
						}
					}
				})
			}
		}
	}
}

// Both public discovery aliases share the transparent refresh policy, regardless
// of the mechanism. The OpenAI list format isolates that policy from adapters.
func TestModelsRefreshStaysTransparent(t *testing.T) {
	for _, mechanism := range tokenRefreshMechanisms {
		for _, path := range []string{"/models", "/v1/models"} {
			t.Run(mechanism.provider+path, func(t *testing.T) {
				f := newTokenRefreshFixture(t, mechanism.provider, http.StatusOK,
					tokenResponse(t, mechanism.accessField, mechanism.refreshField, "fresh-access", "fresh-rt"))
				r := f.request(http.MethodGet, path, "", mkJWT(t, time.Now().Add(-time.Hour).Unix()))
				w := httptest.NewRecorder()
				f.proxy.ServeHTTP(w, r)
				if w.Code != http.StatusOK || f.exchanges.Load() != 1 || f.relays.Load() != 1 {
					t.Fatalf("status=%d exchanges=%d relays=%d, want 200/1/1", w.Code, f.exchanges.Load(), f.relays.Load())
				}
				got := f.lastRelay.Load()
				if got.authHeader != "Bearer fresh-access" || got.path != "/v1/models" || got.refreshToken != "" {
					t.Error("discovery did not use the fresh access token on its constructed model request")
				}
				if w.Header().Get("X-Proxy-Access-Token") != "" || w.Header().Get("X-Proxy-Refresh-Token") != "" {
					t.Error("transparent discovery returned refreshed credential headers")
				}
				f.assertNoAccounting(t)
			})
		}
	}
}

func TestExchangeTokenResponseSizeBoundary(t *testing.T) {
	for _, mechanism := range tokenRefreshMechanisms {
		for _, extra := range []int{0, 1} {
			name := "at limit"
			if extra != 0 {
				name = "over limit"
			}
			t.Run(mechanism.provider+"/"+name, func(t *testing.T) {
				body := tokenResponse(t, mechanism.accessField, mechanism.refreshField, "fixture-access", "")
				body += strings.Repeat(" ", tokenRespMax-len(body)+extra)
				f := newTokenRefreshFixture(t, mechanism.provider, http.StatusOK, body)
				tg := &target{provider: mechanism.provider, baseURL: f.baseURL}
				fresh, err := refreshAccessToken(t.Context(), tg, "client-rt")
				if extra == 0 {
					if err != nil || fresh.access != "fixture-access" {
						t.Errorf("valid response at byte limit rejected: %v", err)
					}
				} else if err == nil || fresh.access != "" || fresh.refresh != "" {
					t.Error("oversized response with a valid JSON prefix was accepted")
				}
				if f.exchanges.Load() != 1 || f.relays.Load() != 0 {
					t.Error("size-boundary check must make exactly one token exchange and no inference")
				}
			})
		}
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
