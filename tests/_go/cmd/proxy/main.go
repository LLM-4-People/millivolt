package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
	"github.com/LLM-4-People/millivolt/internal/web"
)

// TestOperatorPlaneBoundary is the deny-by-default regression for the whole
// dashboard gate: only /healthz, brand/PWA files and the inference catch-all
// pass without the credential, every dashboard/metrics/admin route needs it
// (Bearer or the session cookie the gate mints), the unauthenticated HTML
// entrypoint gets the login page, same-origin mutation checks stay armed
// behind the credential, and an unconfigured credential denies the whole plane.
func TestOperatorPlaneBoundary(t *testing.T) {
	const token = "operator-fixture-credential"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	t.Run("open routes never see the credential gate", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		requests := []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://proxy.example/healthz", nil),
			httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/v1/models", nil),
			httptest.NewRequest(http.MethodPost, "http://proxy.example/anything/else/for/upstream", nil),
		}
		for _, p := range web.BrandPaths() {
			if gatedPath(p) {
				t.Errorf("brand path %s is gated", p)
			}
			requests = append(requests, httptest.NewRequest(http.MethodGet, "http://proxy.example"+p, nil))
		}
		// A provider credential rides the same header name; the gate must
		// never consume or reject it outside the operator plane.
		requests[0].Header.Set("Authorization", "Bearer provider-key")
		for _, r := range requests {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("%s %s: status=%d want=204 (passthrough)", r.Method, r.URL.Path, w.Code)
			}
		}
	})

	t.Run("the whole dashboard needs the credential", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		requests := []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://proxy.example/index.html", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/dash/js/core.js", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/agg/log", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/live/stream", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/prometheus", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/admin/config", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/admin/pause", nil),
			httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/purge", nil),
			// Deny by default: an unclassified admin path or method is gated.
			httptest.NewRequest(http.MethodGet, "http://proxy.example/admin/unclassified", nil),
			httptest.NewRequest(http.MethodPut, "http://proxy.example/admin/restart", nil),
		}
		for _, r := range requests {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without credential: status=%d want=401", r.Method, r.URL.Path, w.Code)
			}
		}
	})

	t.Run("bearer and session cookie both admit the dashboard", func(t *testing.T) {
		gate := newOperatorGate(token)
		h := protectOperatorRequests(next, gate)
		authorized := []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/dash/js/core.js", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil),
			httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/purge", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/admin/unclassified", nil),
		}
		for _, r := range authorized {
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("%s %s with bearer: status=%d want=204", r.Method, r.URL.Path, w.Code)
			}
		}
		// A Bearer success mints the session cookie (EventSource cannot send
		// Authorization): HttpOnly, Strict, root path, and it authenticates.
		w := httptest.NewRecorder()
		bearer := httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil)
		bearer.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, bearer)
		set := w.Result().Cookies()
		if len(set) != 1 || set[0].Name != sessionCookieName || !set[0].HttpOnly || set[0].SameSite != http.SameSiteStrictMode || set[0].Path != "/" {
			t.Fatalf("bearer success did not mint a strict HttpOnly session cookie: %+v", set)
		}
		// The MCP namespace is Bearer-only, so an admitted /mcp request mints
		// no session cookie: a client's cookie jar must not gain a dashboard
		// credential for a response that never uses one.
		w = httptest.NewRecorder()
		mcpRequest := httptest.NewRequest(http.MethodPost, "http://proxy.example/mcp", nil)
		mcpRequest.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, mcpRequest)
		if w.Code != http.StatusNoContent {
			t.Errorf("admitted /mcp: status=%d want=204", w.Code)
		}
		if set := w.Result().Cookies(); len(set) != 0 {
			t.Fatalf("admitted /mcp minted %d cookies, want none: %+v", len(set), set)
		}
		session := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
		session.AddCookie(set[0])
		w = httptest.NewRecorder()
		h.ServeHTTP(w, session)
		if w.Code != http.StatusNoContent {
			t.Errorf("session cookie: status=%d want=204", w.Code)
		}
		// Forged and expired cookies are rejected.
		forged := *set[0]
		forged.Value += "tampered"
		w = httptest.NewRecorder()
		bad := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
		bad.AddCookie(&forged)
		h.ServeHTTP(w, bad)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("tampered cookie: status=%d want=401", w.Code)
		}
		// A new process with the same operator token must accept the cookie
		// (container restart). A rotated token must not.
		restart := protectOperatorRequests(next, newOperatorGate(token))
		w = httptest.NewRecorder()
		again := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
		again.AddCookie(set[0])
		restart.ServeHTTP(w, again)
		if w.Code != http.StatusNoContent {
			t.Errorf("restarted process rejected the session cookie: status=%d want=204", w.Code)
		}
		rotated := protectOperatorRequests(next, newOperatorGate("rotated-fixture-credential"))
		w = httptest.NewRecorder()
		stale := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
		stale.AddCookie(set[0])
		rotated.ServeHTTP(w, stale)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("rotated token accepted the old cookie: status=%d want=401", w.Code)
		}
	})

	t.Run("unauthenticated HTML entry gets the login page", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil))
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/admin/session"`) {
			t.Fatalf("GET / without credential: status=%d want=401 login page", w.Code)
		}
		if !strings.Contains(w.Body.String(), "box-sizing: border-box") {
			t.Error("login page must box-size the card so padding cannot overflow the viewport")
		}
		body := w.Body.String()
		for _, needle := range []string{
			`rel="manifest" href="/manifest.webmanifest"`,
			`rel="apple-touch-icon" href="/apple-touch-icon.png"`,
			`name="apple-mobile-web-app-capable" content="yes"`,
			`name="theme-color" content="#1b1826"`,
			`navigator.serviceWorker.register('/sw.js'`,
			`action="/admin/session"`,
		} {
			if !strings.Contains(body, needle) {
				t.Errorf("login page missing %q", needle)
			}
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("login page cache-control=%q want no-store", got)
		}
		// JSON denial for the non-HTML gated surface.
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil))
		if w.Code != http.StatusUnauthorized || strings.HasPrefix(w.Body.String(), "<") {
			t.Errorf("GET /metrics/bootstrap denial: status=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("same-origin checks stay armed behind the credential", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		for _, origin := range []string{"https://foreign.example", "null"} {
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/purge", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("foreign origin %q with valid credential: status=%d want=403", origin, w.Code)
			}
		}
	})

	t.Run("unconfigured credential denies the whole plane", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(""))
		requests := []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil),
			httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/purge", nil),
		}
		for _, r := range requests {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s while disabled: status=%d want=403", r.Method, r.URL.Path, w.Code)
			}
		}
		// The liveness probe and the inference catch-all stay open while the
		// plane is disabled; the session handshake hands off to its own
		// handler, which denies an unarmed plane (asserted below).
		for _, target := range []string{"/healthz", "/favicon.ico", "/v1/chat/completions", "/admin/session"} {
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example"+target, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("%s while disabled: status=%d want=204 (passthrough)", target, w.Code)
			}
		}
	})

	t.Run("session handshake mints cookies", func(t *testing.T) {
		gate := newOperatorGate(token)
		h := gate.handleAdminSession
		// An unarmed plane never mints cookies.
		w := httptest.NewRecorder()
		disabled := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
		disabled.Header.Set("Authorization", "Bearer "+token)
		newOperatorGate("").handleAdminSession(w, disabled)
		if w.Code != http.StatusForbidden {
			t.Errorf("disabled session handshake: status=%d want=403", w.Code)
		}
		// Bearer handshake: 204 + cookie.
		w = httptest.NewRecorder()
		bearer := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
		bearer.Header.Set("Authorization", "Bearer "+token)
		h(w, bearer)
		if w.Code != http.StatusNoContent || len(w.Result().Cookies()) != 1 {
			t.Fatalf("bearer session: status=%d cookies=%d want 204 + cookie", w.Code, len(w.Result().Cookies()))
		}
		// Form handshake: 303 + cookie (the no-JS login flow).
		w = httptest.NewRecorder()
		form := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token="+url.QueryEscape(token)))
		form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, form)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("form session: status=%d location=%q want 303 /", w.Code, w.Header().Get("Location"))
		}
		if len(w.Result().Cookies()) != 1 {
			t.Errorf("form session: cookies=%d want 1", len(w.Result().Cookies()))
		}
		// Wrong credential: form gets the page back with a visible rejection
		// notice (a silent re-serve reads as "nothing happened" and had the
		// operator sign in twice without ever seeing why), API gets JSON.
		w = httptest.NewRecorder()
		bad := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token=wrong-credential"))
		bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, bad)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/admin/session"`) {
			t.Errorf("wrong form credential: status=%d want=401 login page", w.Code)
		}
		if !strings.Contains(w.Body.String(), `role="alert"`) ||
			!strings.Contains(w.Body.String(), "That token was rejected") {
			t.Error("rejected form sign-in must show the rejection notice")
		}
		// A first paint (GET /) never carries the notice: the unauthenticated
		// page cannot reveal that any attempt happened.
		plain := protectOperatorRequests(next, gate)
		w = httptest.NewRecorder()
		plain.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil))
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "That token was rejected") {
			t.Errorf("clean login page must not carry the rejection notice: status=%d", w.Code)
		}
		// The login page itself is inert without a credential: 401. A form
		// post with no token field is a clean page too - the rejection
		// notice is about a presented token, and none was.
		w = httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("empty session post: status=%d want=401", w.Code)
		}
		w = httptest.NewRecorder()
		emptyForm := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader(""))
		emptyForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, emptyForm)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/admin/session"`) ||
			strings.Contains(w.Body.String(), "That token was rejected") {
			t.Errorf("empty form post: status=%d want=401 clean login page", w.Code)
		}
	})
}

// TestOperatorNamespaceOwned proves the /admin and /metrics namespaces never
// leak to the upstream catch-all: unauthenticated requests are gated 401 and
// even an authenticated request to an unregistered namespace path must hit
// the reserved 404 handler, never the inference proxy. (The middleware test
// stubs the mux, so the 404 ownership here is the next handler's contract:
// the gate must hand the request to the mux, which main.go reserves.)
func TestOperatorNamespaceOwned(t *testing.T) {
	var hit string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	h := protectOperatorRequests(next, newOperatorGate("operator-fixture-credential"))
	for _, target := range []string{"/admin/unregistered", "/metrics/unregistered"} {
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example"+target, nil)
		r.Header.Set("Authorization", "Bearer operator-fixture-credential")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent || hit != target {
			t.Errorf("authed %s: status=%d hit=%q want mux handoff to the reserved 404", target, w.Code, hit)
		}
	}
	// Unauthenticated look-alikes never reach the mux at all.
	hit = ""
	h2 := protectOperatorRequests(next, newOperatorGate("operator-fixture-credential"))
	for _, target := range []string{"/admin", "/metrics", "/dash"} {
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example"+target, nil))
		if w.Code != http.StatusUnauthorized || hit != "" {
			t.Errorf("unauthenticated namespace root %s: status=%d hit=%q want=401", target, w.Code, hit)
		}
		// The JSON 401 is a Bearer challenge: the realm header tells API
		// clients which credential realm to present, on the gate surface
		// itself (not just the session handshake).
		if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="millivolt-operator"` {
			t.Errorf("unauthenticated namespace root %s: WWW-Authenticate = %q, want the Bearer realm challenge", target, got)
		}
	}
}

// TestOperatorLockout is the brute-force regression: repeated rejected
// credentials lock the source IP out with an escalating window and a coarse
// Retry-After that never understates it; wrong guesses during a lockout stay
// throttled; a verified credential is admission (the lockout must not lock
// the operator out of their own dashboard) and clears the budget; other IPs
// are unaffected.
func TestOperatorLockout(t *testing.T) {
	const token = "operator-fixture-credential"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	gate := newOperatorGate(token)
	h := protectOperatorRequests(next, gate)

	// asSource rebuilds the request with a chosen RemoteAddr and credential.
	asSource := func(ip, credential string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/bootstrap", nil)
		r.RemoteAddr = ip
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < authFailureThreshold; i++ {
		if w := asSource("192.0.2.10:4444", "wrong-guess-"+strconv.Itoa(i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status=%d want=%d", i+1, w.Code, http.StatusUnauthorized)
		}
	}
	// The lockout throttles wrong guesses with a coarse Retry-After that
	// reflects (never understates) the remaining window.
	w := asSource("192.0.2.10:4444", "one-more-wrong-guess")
	retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if w.Code != http.StatusTooManyRequests || err != nil || retryAfter < 1 || retryAfter > int(authLockoutMax.Seconds()) {
		t.Fatalf("locked wrong guess: status=%d retry-after=%q", w.Code, w.Header().Get("Retry-After"))
	}
	// A verified credential during the lockout is admission, not a guess.
	w = asSource("192.0.2.10:4444", token)
	if w.Code != http.StatusNoContent {
		t.Fatalf("locked correct credential: status=%d want=204", w.Code)
	}
	// The success cleared the budget; wrong guesses start from zero again.
	for i := 0; i < authFailureThreshold-1; i++ {
		if w := asSource("192.0.2.10:4444", "wrong-again-"+strconv.Itoa(i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status=%d", i+1, w.Code)
		}
	}
	if w := asSource("192.0.2.10:4444", token); w.Code != http.StatusNoContent {
		t.Errorf("pre-threshold correct credential: status=%d want=204", w.Code)
	}
	// A different source IP is unaffected by the first IP's history.
	if w := asSource("198.51.100.7:5555", token); w.Code != http.StatusNoContent {
		t.Errorf("other ip: status=%d want=204", w.Code)
	}
}

// TestOperatorLockoutRetryAfterNeverUnderstates pins the ceiling shape of
// the lockout window: 750ms into a fresh 10s lockout leaves 9.25s, and
// nearest-second rounding would report 9s, letting a client honoring
// Retry-After retry 250ms before the lockout expires. The window must round
// up to the full 10s.
func TestOperatorLockoutRetryAfterNeverUnderstates(t *testing.T) {
	gate := newOperatorGate("operator-fixture-credential")
	now := time.Now()
	for i := 0; i < authFailureThreshold; i++ {
		gate.limiter.failure("192.0.2.10", now)
	}
	locked, retryIn := gate.limiter.locked("192.0.2.10", now.Add(750*time.Millisecond))
	if !locked || retryIn != authLockoutBase {
		t.Fatalf("locked=%v retryIn=%v want the full %s (rounded up, never nearest)", locked, retryIn, authLockoutBase)
	}
}

// TestOperatorLockoutHoldsAtCap proves a distributed flood cannot erase a
// live lockout: at the map cap, eviction sacrifices unlocked entries first
// and only then the oldest lockout, while tracked IPs never trigger
// eviction at all.
func TestOperatorLockoutHoldsAtCap(t *testing.T) {
	gate := newOperatorGate("operator-fixture-credential")
	now := time.Now()
	gate.limiter.failure("victim", now)
	gate.limiter.failure("victim", now)
	gate.limiter.failure("victim", now)
	gate.limiter.failure("victim", now)
	gate.limiter.failure("victim", now) // lockout armed
	// Fill the map to the cap with fresh unlocked entries whose lastSeen
	// values strictly increase, so "0" is deterministically the oldest.
	for i := 0; len(gate.limiter.entries) < authMaxIPs; i++ {
		gate.limiter.failure(strconv.Itoa(i), now.Add(time.Duration(i)*time.Second))
	}
	// Tracked failures never evict.
	gate.limiter.failure("victim", now)
	gate.limiter.mu.Lock()
	_, held := gate.limiter.entries["victim"]
	gate.limiter.mu.Unlock()
	if !held {
		t.Fatal("tracked IP was evicted by its own failure")
	}
	// A new source evicts the OLDEST UNLOCKED entry, never the live lockout.
	gate.limiter.failure("newcomer", now)
	gate.limiter.mu.Lock()
	_, victimHeld := gate.limiter.entries["victim"]
	_, firstBotHeld := gate.limiter.entries["0"]
	size := len(gate.limiter.entries)
	gate.limiter.mu.Unlock()
	if !victimHeld {
		t.Fatal("live lockout was evicted before unlocked entries")
	}
	if firstBotHeld {
		t.Fatal("eviction did not free a slot for the newcomer")
	}
	if size > authMaxIPs {
		t.Fatalf("limiter holds %d entries, want <= %d", size, authMaxIPs)
	}
}

// TestOperatorGateUsesThePublishedTokenContract pins the proxy's boot gate to
// internal/mcp, the one owner of the operator credential contract: the gate
// reads mcp.ProxyTokenEnv, and its boot band is exactly
// mcp.OperatorTokenMinLen..mcp.OperatorTokenMaxLen, inclusive. The boundary
// cases are derived from the owner, so a diverging local value fails here. A
// same-value local literal cannot be told apart by those behavior probes, so
// the parsed source is inspected at the end: loadOperatorToken must read the
// owner's variable name through os.LookupEnv and compare the value against the
// owner band, and bearerToken must compare against the owner presentation cap.
// The walk follows same-package helper calls, so extracting those reads into a
// helper stays green; comments are absent from the AST and formatting is
// irrelevant, so only real symbol use satisfies the guard. The owner's literal
// pins live in tests/_go/internal/mcp/drift.go.
func TestOperatorGateUsesThePublishedTokenContract(t *testing.T) {
	minToken := strings.Repeat("x", mcp.OperatorTokenMinLen)
	maxToken := strings.Repeat("x", mcp.OperatorTokenMaxLen)
	for _, tc := range []struct {
		name  string
		token string
		valid bool
	}{
		{"below the minimum", minToken[:len(minToken)-1], false},
		{"at the minimum", minToken, true},
		{"at the maximum", maxToken, true},
		{"above the maximum", maxToken + "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(mcp.ProxyTokenEnv, tc.token)
			value, err := loadOperatorToken()
			if tc.valid {
				if err != nil || value != tc.token {
					t.Fatalf("loadOperatorToken rejected the %d-character boundary token: value is %d characters, err=%v",
						len(tc.token), len(value), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("loadOperatorToken accepted the %d-character token, outside %d..%d",
					len(tc.token), mcp.OperatorTokenMinLen, mcp.OperatorTokenMaxLen)
			}
		})
	}

	// Setting only a look-alike name must leave the credential unset: the gate
	// reads the owner's variable, not a local spelling.
	t.Setenv(mcp.ProxyTokenEnv, "")
	t.Setenv(mcp.ProxyTokenEnv+"_LEGACY", minToken)
	if value, err := loadOperatorToken(); value != "" || err != nil {
		t.Fatalf("the gate must read exactly %s: value is %d characters, err=%v", mcp.ProxyTokenEnv, len(value), err)
	}

	// The behavior probes above cannot distinguish the owner symbols from
	// same-value local literals, so the two read sites are pinned in the
	// parsed source: loadOperatorToken owns the variable name and the boot
	// band, bearerToken owns the presentation cap. Replacing any of these
	// reads or comparisons with a literal fails here even when its value
	// matches today, while comments and formatting cannot satisfy the guard.
	file, err := parser.ParseFile(token.NewFileSet(), "operator.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/proxy/operator.go: %v", err)
	}
	uses := collectOperatorOwnerUses(t, file)
	for _, read := range []struct {
		name string
		ok   bool
		want string
	}{
		{"loadOperatorToken reads the owner variable name", uses.envLookup, "os.LookupEnv(mcp.ProxyTokenEnv)"},
		{"loadOperatorToken compares against the owner minimum", uses.bandMin, "mcp.OperatorTokenMinLen"},
		{"loadOperatorToken compares against the owner maximum", uses.bandMax, "mcp.OperatorTokenMaxLen"},
		{"bearerToken compares against the owner presentation cap", uses.presentMax, "mcp.OperatorTokenMaxLen"},
	} {
		if !read.ok {
			t.Errorf("%s: %s must appear in a real expression of the parsed source; a comment or literal does not carry the contract",
				read.name, read.want)
		}
	}
}

// operatorOwnerUses reports where the internal/mcp owner selectors appear in
// real expressions of loadOperatorToken and bearerToken: the parsed AST sees
// symbol use, never comments, and the walk follows same-package helper calls
// transitively so extracting a helper that still reads the owners stays green.
type operatorOwnerUses struct {
	envLookup  bool // os.LookupEnv(mcp.ProxyTokenEnv)
	bandMin    bool // a comparison with mcp.OperatorTokenMinLen as an operand
	bandMax    bool // a comparison with mcp.OperatorTokenMaxLen as an operand
	presentMax bool // bearerToken's comparison with mcp.OperatorTokenMaxLen
}

// collectOperatorOwnerUses walks the two credential read sites plus every
// same-package function they call and collects the owner-selector uses. A
// missing root function fails loudly rather than scanning nothing.
func collectOperatorOwnerUses(t *testing.T, file *ast.File) operatorOwnerUses {
	t.Helper()
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			funcs[fn.Name.Name] = fn
		}
	}
	reachable := func(root string) []*ast.FuncDecl {
		first, ok := funcs[root]
		if !ok {
			t.Fatalf("cmd/proxy/operator.go no longer declares %s; update the owner-symbol guard", root)
		}
		seen := map[string]bool{root: true}
		queue := []*ast.FuncDecl{first}
		out := []*ast.FuncDecl{first}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			ast.Inspect(current, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || seen[id.Name] {
					return true
				}
				if callee, ok := funcs[id.Name]; ok {
					seen[id.Name] = true
					queue = append(queue, callee)
					out = append(out, callee)
				}
				return true
			})
		}
		return out
	}

	var uses operatorOwnerUses
	for _, fn := range reachable("loadOperatorToken") {
		ast.Inspect(fn, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if selectorNamed(node.Fun, "os", "LookupEnv") {
					for _, arg := range node.Args {
						if selectorNamed(arg, "mcp", "ProxyTokenEnv") {
							uses.envLookup = true
						}
					}
				}
			case *ast.BinaryExpr:
				if !comparisonOperator(node.Op) {
					return true
				}
				if selectorNamed(node.X, "mcp", "OperatorTokenMinLen") || selectorNamed(node.Y, "mcp", "OperatorTokenMinLen") {
					uses.bandMin = true
				}
				if selectorNamed(node.X, "mcp", "OperatorTokenMaxLen") || selectorNamed(node.Y, "mcp", "OperatorTokenMaxLen") {
					uses.bandMax = true
				}
			}
			return true
		})
	}
	for _, fn := range reachable("bearerToken") {
		ast.Inspect(fn, func(n ast.Node) bool {
			node, ok := n.(*ast.BinaryExpr)
			if !ok || !comparisonOperator(node.Op) {
				return true
			}
			if selectorNamed(node.X, "mcp", "OperatorTokenMaxLen") || selectorNamed(node.Y, "mcp", "OperatorTokenMaxLen") {
				uses.presentMax = true
			}
			return true
		})
	}
	return uses
}

// selectorNamed reports whether expr is the qualified selector pkg.name.
func selectorNamed(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg && sel.Sel.Name == name
}

// comparisonOperator reports whether op is a relational comparison, the shape
// the boot band and the presentation cap are checked with.
func comparisonOperator(op token.Token) bool {
	switch op {
	case token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

// TestOperatorTokenEnv owns the boot-credential contract: unset denies the
// whole plane (armed, no credential); set but empty or too short fails the
// boot instead of silently degrading to a weaker policy.
func TestOperatorTokenEnv(t *testing.T) {
	if value, err := loadOperatorToken(); value != "" || err != nil {
		t.Errorf("unset token: value=%q err=%v, want \"\", nil", value, err)
	}
	t.Setenv(mcp.ProxyTokenEnv, "")
	if value, err := loadOperatorToken(); value != "" || err != nil {
		t.Errorf("empty token: value=%q err=%v, want \"\", nil (compose interpolations yield empty; treat as unset)", value, err)
	}
	t.Setenv(mcp.ProxyTokenEnv, "short")
	if _, err := loadOperatorToken(); err == nil {
		t.Error("short token: want boot failure")
	}
	t.Setenv(mcp.ProxyTokenEnv, strings.Repeat("x", mcp.OperatorTokenMaxLen+1))
	if _, err := loadOperatorToken(); err == nil {
		t.Error("oversized token: want boot failure (it could never be presented via Bearer)")
	}
	const credential = "operator-fixture-credential"
	t.Setenv(mcp.ProxyTokenEnv, credential)
	value, err := loadOperatorToken()
	if err != nil || value != credential {
		t.Errorf("valid token: value=%q err=%v, want %q, nil", value, err, credential)
	}
	maxTok := strings.Repeat("x", mcp.OperatorTokenMaxLen)
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
	req.Header.Set("Authorization", "Bearer "+maxTok)
	got, ok := bearerToken(req)
	if !ok || got != maxTok {
		t.Fatalf("max-length Bearer token: ok=%v len=%d, want accepted", ok, len(got))
	}
	// One byte over the owner maximum must be refused: a presentation cap
	// widened past the owner on either side would fail this leg.
	req = httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
	req.Header.Set("Authorization", "Bearer "+maxTok+"x")
	if got, ok := bearerToken(req); ok {
		t.Fatalf("one byte over the owner maximum: bearerToken accepted %d characters, want refused", len(got))
	}
}

// TestLoginPageNamesTheOwnedOperatorVariable pins the sign-in page's prose to
// mcp.ProxyTokenEnv, the one owner of the variable name: every MILLIVOLT_*
// spelling on both served variants must be that constant, and the page must
// name at least one. Comparing against the owner (rather than a pinned
// literal) is what makes an owner rename move the page: a stale hand-written
// copy then differs from the constant and fails here, while the page rendered
// from the owner follows the new name.
func TestLoginPageNamesTheOwnedOperatorVariable(t *testing.T) {
	pattern := regexp.MustCompile(`MILLIVOLT_[A-Z0-9_]+`)
	for _, page := range []struct {
		name string
		html string
	}{
		{"login page", loginPageHTML},
		{"rejected login page", loginPageRejectedHTML},
	} {
		names := pattern.FindAllString(page.html, -1)
		if len(names) == 0 {
			t.Fatalf("the %s no longer names the operator variable", page.name)
		}
		for _, got := range names {
			if got != mcp.ProxyTokenEnv {
				t.Fatalf("the %s names %s, the owner is %s", page.name, got, mcp.ProxyTokenEnv)
			}
		}
	}
}

// livePayload decodes the bootstrap/SSE payload wrapper for snapshot
// assertions (mirrors the internal/metrics feed-test fixture).
type livePayload struct {
	Records     []*metrics.Record `json:"records"`
	Seq         int64             `json:"seq"`
	Incremental bool              `json:"incremental"`
}

func decodeLivePayload(t *testing.T, raw []byte) livePayload {
	t.Helper()
	var p livePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v\n%s", err, raw)
	}
	return p
}

func fullSnapshot(t *testing.T, b *metrics.Buffer) livePayload {
	t.Helper()
	raw, err := json.Marshal(b.SnapshotSince(0))
	if err != nil {
		t.Fatal(err)
	}
	p := decodeLivePayload(t, raw)
	if p.Incremental {
		t.Fatalf("since=0 must be a FULL snapshot, got incremental")
	}
	return p
}

// TestApplySnapshotLimitGatedOnDurableStore is the regression for the
// reload-path bug: reloadConfig re-applied the full-snapshot cap gated only
// on the buffer (always non-nil), never on store presence - so an
// in-memory-only instance (db_path empty / -db-path none) got its full
// snapshots capped to 8×dash_log_rows on the FIRST reload while the ring
// held history_size records, and the /metrics/agg/log paging fallback is
// dead without a store: older rows became unreachable until restart. The
// gate must mirror boot: capped only when durable storage is on, unlimited
// otherwise (then the ring IS all history); a delta is never capped.
func TestApplySnapshotLimitGatedOnDurableStore(t *testing.T) {
	const rows = 1                         // smallest band value; 8×rows cap
	const capN = web.BootRingCapMul * rows // 8
	const total = capN + 2                 // strictly more than the cap

	b := metrics.NewBuffer(total)
	for i := 0; i < total; i++ {
		b.Record(&metrics.Record{
			ID: "r" + strconv.Itoa(i), Provider: "p", StatusCode: 200, Start: time.Now(),
		})
	}

	// No store: the ring IS all history - the full snapshot must carry
	// EVERY record (the buggy reload wiring capped it to capN).
	applySnapshotLimit(b, nil, rows)
	if p := fullSnapshot(t, b); len(p.Records) != total {
		t.Errorf("no store: full snapshot has %d records, want all %d (unlimited)",
			len(p.Records), total)
	}

	// A real store: full snapshots cap to the newest capN records, and the
	// delta path is unaffected (it may exceed the cap - capping a delta
	// would be a silent miss).
	d := config.Default()
	store, err := storage.Open(filepath.Join(t.TempDir(), "gate.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	applySnapshotLimit(b, store, rows)
	p := fullSnapshot(t, b)
	if len(p.Records) != capN {
		t.Errorf("with store: full snapshot has %d records, want capped %d", len(p.Records), capN)
	}
	wantFirst := "r" + strconv.Itoa(total-capN)
	if p.Records[0].ID != wantFirst || p.Records[len(p.Records)-1].ID != "r"+strconv.Itoa(total-1) {
		t.Errorf("with store: capped window = %s..%s, want newest %d (%s..)",
			p.Records[0].ID, p.Records[len(p.Records)-1].ID, capN, wantFirst)
	}
	raw, err := json.Marshal(b.SnapshotSince(1))
	if err != nil {
		t.Fatal(err)
	}
	dp := decodeLivePayload(t, raw)
	if !dp.Incremental || len(dp.Records) != total-1 {
		t.Errorf("delta after cap: inc=%v n=%d, want incremental %d (uncapped; exceeds the %d cap)",
			dp.Incremental, len(dp.Records), total-1, capN)
	}

	// The gate owns both states: back to no store → unlimited again.
	applySnapshotLimit(b, nil, rows)
	if p := fullSnapshot(t, b); len(p.Records) != total {
		t.Errorf("no store after store: full snapshot has %d records, want all %d restored",
			len(p.Records), total)
	}
}

// TestNewHTTPServerContract pins the real server constructor the main boot
// and the restart handoff's clone share. The round-seven mutation round
// proved NOTHING constructed newHTTPServer: dropping the BaseContext tie (the
// shared shutdown context that aborts in-flight SSE streams immediately and
// keeps Shutdown off the per-request timeouts) or the timeout set (boot
// config values, no WriteTimeout - it would cut off long-lived SSE streams)
// went unnoticed. The tie is asserted behaviorally: the context the server
// hands its listeners IS the caller's srvCtx, so cancelling it is observable
// by every connection the server accepted.
func TestNewHTTPServerContract(t *testing.T) {
	cfg := config.Default()
	cfg.ReadHeaderTimeout = 3 * time.Second
	cfg.IdleTimeout = 4 * time.Minute
	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newHTTPServer(http.NotFoundHandler(), cfg, srvCtx)
	if srv.ReadHeaderTimeout != cfg.ReadHeaderTimeout || srv.IdleTimeout != cfg.IdleTimeout {
		t.Fatalf("timeouts = (%v, %v), want the boot config's (%v, %v)",
			srv.ReadHeaderTimeout, srv.IdleTimeout, cfg.ReadHeaderTimeout, cfg.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 (it would cut off long-lived SSE streams)", srv.WriteTimeout)
	}
	if srv.BaseContext == nil {
		t.Fatal("BaseContext is nil; in-flight streams could never observe the shared shutdown context")
	}
	if got := srv.BaseContext(nil); got != srvCtx {
		t.Fatalf("BaseContext() = %v, want the caller's srvCtx (the shutdown tie)", got)
	}
	cancel()
	if err := srvCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("srvCtx.Err() = %v, want Canceled through the server's base context", err)
	}
}
