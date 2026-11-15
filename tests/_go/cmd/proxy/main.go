package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
// dashboard gate: only /healthz, /readyz, brand/PWA files and the inference
// catch-all pass without the credential, every dashboard/metrics/admin route
// needs it (Bearer or the session cookie the gate mints), the unauthenticated
// HTML entrypoint gets the login page, same-origin mutation checks stay armed
// behind the credential, and an unconfigured credential denies the whole plane.
func TestOperatorPlaneBoundary(t *testing.T) {
	const token = "operator-fixture-credential"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	t.Run("open routes never see the credential gate", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		requests := []*http.Request{
			httptest.NewRequest(http.MethodGet, "http://proxy.example/healthz", nil),
			httptest.NewRequest(http.MethodGet, "http://proxy.example/readyz", nil),
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
		if len(set) != 1 || set[0].Name != sessionCookieName || !set[0].HttpOnly || set[0].Secure || set[0].SameSite != http.SameSiteStrictMode || set[0].Path != "/" {
			t.Fatalf("bearer success did not mint a strict HttpOnly session cookie over plain HTTP: %+v", set)
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
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/session"`) {
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
			`name="theme-color" content="#100e0b"`,
			`navigator.serviceWorker.register('/sw.js'`,
			`action="/session"`,
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
		// The probe routes and the inference catch-all stay open while the
		// plane is disabled; the session handshake hands off to its own
		// handler, which denies an unarmed plane (asserted below).
		for _, target := range []string{"/healthz", "/readyz", "/favicon.ico", "/v1/chat/completions", "/session"} {
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
		h := gate.handleOperatorSession
		// An unarmed plane never mints cookies.
		w := httptest.NewRecorder()
		disabled := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
		disabled.Header.Set("Authorization", "Bearer "+token)
		newOperatorGate("").handleOperatorSession(w, disabled)
		if w.Code != http.StatusForbidden {
			t.Errorf("disabled session handshake: status=%d want=403", w.Code)
		}
		// Bearer handshake: 204 + cookie.
		w = httptest.NewRecorder()
		bearer := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
		bearer.Header.Set("Authorization", "Bearer "+token)
		h(w, bearer)
		if w.Code != http.StatusNoContent || len(w.Result().Cookies()) != 1 {
			t.Fatalf("bearer session: status=%d cookies=%d want 204 + cookie", w.Code, len(w.Result().Cookies()))
		}
		// Form handshake: 303 + cookie (the no-JS login flow).
		w = httptest.NewRecorder()
		form := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
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
		bad := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
			strings.NewReader("token=wrong-credential"))
		bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, bad)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/session"`) {
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
		h(w, httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("empty session post: status=%d want=401", w.Code)
		}
		w = httptest.NewRecorder()
		emptyForm := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
			strings.NewReader(""))
		emptyForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, emptyForm)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/session"`) ||
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
	// The sign-in form survives its own lockout: the JSON 429 hid the only
	// route back to a credential. GET / is served as the read-only page, with
	// the form intact, the same Retry-After, and the lockout named in its
	// notice slot. Every other operator route keeps the JSON body.
	page := httptest.NewRecorder()
	pr := httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil)
	pr.RemoteAddr = "192.0.2.10:4444"
	h.ServeHTTP(page, pr)
	if page.Code != http.StatusTooManyRequests ||
		!strings.Contains(page.Body.String(), `action="/session"`) ||
		!strings.Contains(page.Body.String(), "Too many rejected tokens") {
		t.Fatalf("locked sign-in page: status=%d body=%s", page.Code, page.Body.String())
	}
	if got := page.Header().Get("Retry-After"); got != strconv.Itoa(retryAfter) {
		t.Fatalf("locked sign-in page Retry-After = %q, want the same %d as the denial", got, retryAfter)
	}
	jsonDenial := asSource("192.0.2.10:4444", "")
	if jsonDenial.Code != http.StatusTooManyRequests ||
		!strings.Contains(jsonDenial.Body.String(), `"error"`) ||
		strings.Contains(jsonDenial.Body.String(), `action="/session"`) {
		t.Fatalf("locked non-page route: status=%d body=%s, want the flat JSON 429, not the page",
			jsonDenial.Code, jsonDenial.Body.String())
	}
	// The handshake itself is still reachable through the gate: POST /session
	// is exempt by path whatever the lockout says, and only its own handler
	// (which runs the same lockout check after checking the credential) may
	// refuse it.
	form := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", strings.NewReader("token="+token))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.RemoteAddr = "192.0.2.10:4444"
	signedIn := httptest.NewRecorder()
	h.ServeHTTP(signedIn, form)
	if signedIn.Code != http.StatusNoContent {
		t.Fatalf("locked session handshake: status=%d, want the gate to hand it to the session handler", signedIn.Code)
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
// the parsed source is inspected at the end: loadOperatorToken must assign the
// result of os.LookupEnv(mcp.ProxyTokenEnv) to a name and compare that value
// against the owner band inside a decision condition, and bearerToken must
// compare a presented length against the owner presentation cap in its if
// condition. The walk follows same-package helper calls, so extracting those
// reads into a helper stays green; comments are absent from the AST and
// formatting is irrelevant, so only real symbol use satisfies the guard. A
// discarded lookup or a discarded comparison is not a decision and fails. The
// owner's literal pins live in tests/_go/internal/mcp/drift.go.
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
		{"minimum in two-byte characters", strings.Repeat("é", mcp.OperatorTokenMinLen/2), true},
		{"below the minimum in two-byte characters", strings.Repeat("é", mcp.OperatorTokenMinLen/2-1), false},
		{"513 bytes in three-byte characters", strings.Repeat("€", mcp.OperatorTokenMaxLen/3+1), false},
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
	// parsed source: loadOperatorToken owns the variable name, the assigned
	// value and the boot band, bearerToken owns the presentation cap.
	// Replacing any of these reads or comparisons with a literal fails here
	// even when its value matches today, while comments, discarded
	// expressions and formatting cannot satisfy the guard.
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
		{"loadOperatorToken assigns the owner variable lookup", uses.envLookup, "the assigned result of os.LookupEnv(mcp.ProxyTokenEnv)"},
		{"loadOperatorToken's decision compares against the owner minimum", uses.bandMin, "mcp.OperatorTokenMinLen against the assigned value"},
		{"loadOperatorToken's decision compares against the owner maximum", uses.bandMax, "mcp.OperatorTokenMaxLen against the assigned value"},
		{"bearerToken's if condition compares a length against the owner presentation cap", uses.presentMax, "mcp.OperatorTokenMaxLen against a len() call"},
	} {
		if !read.ok {
			t.Errorf("%s: %s must appear in a real decision of the parsed source; a comment, a discarded lookup, a discarded comparison or a same-value literal does not carry the contract",
				read.name, read.want)
		}
	}
}

// operatorOwnerUses reports how the internal/mcp owner selectors enter the
// credential decisions of loadOperatorToken and bearerToken: the parsed AST
// sees symbol use, never comments, and the walk follows same-package helper
// calls transitively so extracting a helper that still reads the owners stays
// green. The lookup flag requires the os.LookupEnv result to reach a named
// variable, and the band flags require that variable (also as a helper
// parameter bound at a call site) to be compared against the owner band inside
// a decision condition, so a discarded lookup or discarded comparison cannot
// satisfy the guard even when a same-value literal sits at the live site.
type operatorOwnerUses struct {
	envLookup  bool // os.LookupEnv(mcp.ProxyTokenEnv) assigned to a name
	bandMin    bool // a decision condition compares that value with mcp.OperatorTokenMinLen
	bandMax    bool // a decision condition compares that value with mcp.OperatorTokenMaxLen
	presentMax bool // bearerToken's if condition compares a length with mcp.OperatorTokenMaxLen
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
			for _, name := range calledFunctions(current, funcs) {
				if !seen[name] {
					seen[name] = true
					queue = append(queue, funcs[name])
					out = append(out, funcs[name])
				}
			}
		}
		return out
	}

	var uses operatorOwnerUses
	load := reachable("loadOperatorToken")
	tokenNames := lookedUpTokenNames(load, funcs)
	uses.envLookup = len(tokenNames) > 0
	for _, fn := range load {
		walkConditionComparisons(fn, func(cmp *ast.BinaryExpr) {
			if comparisonWithOwnerValue(cmp, tokenNames, "OperatorTokenMinLen") {
				uses.bandMin = true
			}
			if comparisonWithOwnerValue(cmp, tokenNames, "OperatorTokenMaxLen") {
				uses.bandMax = true
			}
		})
	}
	for _, fn := range reachable("bearerToken") {
		walkConditionComparisons(fn, func(cmp *ast.BinaryExpr) {
			if comparisonWithOwnerLength(cmp, "OperatorTokenMaxLen") {
				uses.presentMax = true
			}
		})
	}
	return uses
}

// calledFunctions returns the names of same-package functions called in fn.
func calledFunctions(fn *ast.FuncDecl, funcs map[string]*ast.FuncDecl) []string {
	var names []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := funcs[id.Name]; ok {
			names = append(names, id.Name)
		}
		return true
	})
	return names
}

// lookedUpTokenNames returns every name in the reachable graph that receives
// the result of os.LookupEnv(mcp.ProxyTokenEnv): directly from the assignment,
// or as a same-package helper parameter bound to such a name at a call site
// (propagated to a fixpoint). A blank-only assignment yields no names, so a
// discarded lookup cannot satisfy the guard.
func lookedUpTokenNames(funcs []*ast.FuncDecl, all map[string]*ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	for _, fn := range funcs {
		ast.Inspect(fn, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) || !lookupOfProxyTokenEnv(rhs) {
					continue
				}
				if id, ok := assign.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
					names[id.Name] = true
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				callee, ok := all[id.Name]
				if !ok || callee.Type.Params == nil {
					return true
				}
				index := 0
				for _, field := range callee.Type.Params.List {
					if _, variadic := field.Type.(*ast.Ellipsis); variadic {
						return true
					}
					for _, param := range field.Names {
						if index < len(call.Args) {
							if arg, ok := call.Args[index].(*ast.Ident); ok && names[arg.Name] && !names[param.Name] {
								names[param.Name] = true
								changed = true
							}
						}
						index++
					}
				}
				return true
			})
		}
	}
	return names
}

// lookupOfProxyTokenEnv reports the exact call shape os.LookupEnv(mcp.ProxyTokenEnv).
func lookupOfProxyTokenEnv(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || !selectorNamed(call.Fun, "os", "LookupEnv") {
		return false
	}
	for _, arg := range call.Args {
		if selectorNamed(arg, "mcp", "ProxyTokenEnv") {
			return true
		}
	}
	return false
}

// walkConditionComparisons visits every relational comparison inside a decision
// condition of fn: an if condition or a switch case expression. A comparison
// used as a discarded expression is not a decision and is never visited.
func walkConditionComparisons(fn *ast.FuncDecl, visit func(*ast.BinaryExpr)) {
	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			visitComparisons(node.Cond, visit)
		case *ast.CaseClause:
			for _, expr := range node.List {
				visitComparisons(expr, visit)
			}
		}
		return true
	})
}

// visitComparisons visits every relational comparison in one condition subtree.
func visitComparisons(cond ast.Expr, visit func(*ast.BinaryExpr)) {
	ast.Inspect(cond, func(n ast.Node) bool {
		if cmp, ok := n.(*ast.BinaryExpr); ok && comparisonOperator(cmp.Op) {
			visit(cmp)
		}
		return true
	})
}

// comparisonWithOwnerValue reports whether cmp compares the owner selector
// against the looked-up token value itself or its length.
func comparisonWithOwnerValue(cmp *ast.BinaryExpr, tokenNames map[string]bool, owner string) bool {
	if selectorNamed(cmp.X, "mcp", owner) {
		return tokenValueExpr(cmp.Y, tokenNames)
	}
	if selectorNamed(cmp.Y, "mcp", owner) {
		return tokenValueExpr(cmp.X, tokenNames)
	}
	return false
}

// tokenValueExpr reports whether expr is the looked-up token variable or its
// len() call.
func tokenValueExpr(expr ast.Expr, tokenNames map[string]bool) bool {
	if id, ok := expr.(*ast.Ident); ok {
		return tokenNames[id.Name]
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || !isLenCall(call) {
		return false
	}
	id, ok := call.Args[0].(*ast.Ident)
	return ok && tokenNames[id.Name]
}

// comparisonWithOwnerLength reports whether cmp compares a len() call against
// the owner selector, the presentation-cap shape bearerToken owns.
func comparisonWithOwnerLength(cmp *ast.BinaryExpr, owner string) bool {
	if selectorNamed(cmp.X, "mcp", owner) {
		return lengthCall(cmp.Y)
	}
	if selectorNamed(cmp.Y, "mcp", owner) {
		return lengthCall(cmp.X)
	}
	return false
}

// lengthCall reports whether expr is a len() call on a named value.
func lengthCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || !isLenCall(call) {
		return false
	}
	_, ok = call.Args[0].(*ast.Ident)
	return ok
}

// isLenCall reports whether call is the builtin len applied to one argument.
func isLenCall(call *ast.CallExpr) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "len"
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
// snapshots capped to web.BootRingCapMul × dash_log_rows on the FIRST reload
// while the ring held history_size records, and the /metrics/agg/log paging
// fallback is dead without a store: older rows became unreachable until
// restart. The gate must mirror boot: capped only when durable storage is on,
// unlimited otherwise (then the ring IS all history); a delta is never capped.
func TestApplySnapshotLimitGatedOnDurableStore(t *testing.T) {
	const rows = 1                         // smallest band value; cap is BootRingCapMul * rows
	const capN = web.BootRingCapMul * rows // derived cap owned by internal/web
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
// by every connection the server accepted. The delivery wrapper is asserted
// through the constructor too: Handler must be the transport-header wrapper
// around the delivery wrapper (a nil policy still refuses through the deadline
// layer), and a request must still reach the wrapped handler.
func TestNewHTTPServerContract(t *testing.T) {
	cfg := config.Default()
	cfg.ReadHeaderTimeout = 3 * time.Second
	cfg.IdleTimeout = 4 * time.Minute
	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fixture-Handler", "reached")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := newHTTPServer(next, cfg.ReadHeaderTimeout, cfg.IdleTimeout, deliveryPolicyFor(cfg), srvCtx)
	outer, ok := srv.Handler.(http.HandlerFunc)
	if !ok {
		t.Fatalf("Handler = %T, want the transport-header wrapper", srv.Handler)
	}
	rr := httptest.NewRecorder()
	outer.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://proxy.example/", nil))
	if rr.Code != http.StatusNoContent || rr.Header().Get("X-Fixture-Handler") != "reached" {
		t.Fatalf("wrapped handler = status %d header %q, want the constructor's handler reached",
			rr.Code, rr.Header().Get("X-Fixture-Handler"))
	}
	if rr.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("the transport-header layer did not run on a secure presentation")
	}
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

// TestOperatorSessionCookieSecureFlag pins the transport-derived Secure
// attribute: the listener itself is plain HTTP, so loopback development keeps
// working without Secure, while a TLS request or an ingress that terminates
// TLS and sets X-Forwarded-Proto gets the flag. The header is a list and ANY
// secure token wins: a proxy that appends its own scheme after the client's
// "http" must not downgrade the cookie. wss counts as secure. The gate mints
// the cookie on every authenticated request, which is the path asserted here.
func TestOperatorSessionCookieSecureFlag(t *testing.T) {
	const token = "operator-fixture-credential"
	cases := []struct {
		name      string
		target    string
		forwarded string
		secure    bool
	}{
		{"plain loopback http", "http://127.0.0.1:8081/", "", false},
		{"tls request", "https://proxy.example/", "", true},
		{"forwarded https", "http://proxy.example/", "https", true},
		{"forwarded https folded", "http://proxy.example/", " HTTPS ", true},
		{"forwarded first token https", "http://proxy.example/", "https, http", true},
		{"forwarded plain http", "http://proxy.example/", "http", false},
		{"appended https is not downgraded", "http://proxy.example/", "http, https", true},
		{"forwarded wss", "http://proxy.example/", "wss", true},
		{"forwarded wss folded", "http://proxy.example/", "HTTP, WSS ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := newOperatorGate(token)
			h := protectOperatorRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), gate)
			r := httptest.NewRequest(http.MethodGet, tc.target+"metrics/bootstrap", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", w.Code)
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %d, want 1", len(cookies))
			}
			cookie := cookies[0]
			if cookie.Secure != tc.secure {
				t.Fatalf("Secure = %v, want %v", cookie.Secure, tc.secure)
			}
			if cookie.Name != sessionCookieName || !cookie.HttpOnly ||
				cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("session cookie lost a canonical attribute: %+v", cookie)
			}
		})
	}
}

// TestClientIPPrefersTheRightmostForwardedAddress pins the throttle key: the
// immediate hop is the only trusted one, so only the address that hop appended
// may count. A client can prepend anything to X-Forwarded-For, and using the
// first element would hand it a fresh bucket per guess.
func TestClientIPPrefersTheRightmostForwardedAddress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remote    string
		forwarded string
		want      string
	}{
		{"no header falls back to the peer", "192.0.2.9:5000", "", "192.0.2.9"},
		{"the appended element wins over a forged prefix", "127.0.0.1:5000", "10.9.9.9, 198.51.100.7", "198.51.100.7"},
		{"a forged suffix after whitespace still loses", "127.0.0.1:5000", "198.51.100.7 ,  10.0.0.1", "10.0.0.1"},
		{"a port is stripped", "127.0.0.1:5000", "198.51.100.7:44321", "198.51.100.7"},
		{"a bracketed IPv6 is normalized", "127.0.0.1:5000", "[2001:db8::7]:443", "2001:db8::7"},
		{"bare IPv6 is accepted", "127.0.0.1:5000", "2001:db8::7", "2001:db8::7"},
		{"unparseable elements are skipped", "127.0.0.1:5000", "junk, 198.51.100.7, not-an-ip", "198.51.100.7"},
		{"an all-junk header falls back to the peer", "192.0.2.9:5000", "junk", "192.0.2.9"},
		{"an unparseable peer is named", "not-an-address", "", "invalid"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
		r.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAuthLimiterLogsTheThresholdCrossingOnce pins the observable side of the
// throttle: crossing the threshold is logged exactly once per source, so an
// operator can see a brute-force run begin without a line per guess.
func TestAuthLimiterLogsTheThresholdCrossingOnce(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var l authLimiter
	now := time.Now()
	for range 3 * authFailureThreshold {
		l.failure("198.51.100.7", now)
	}
	if got := strings.Count(buf.String(), "crossed 5 rejected credentials"); got != 1 {
		t.Fatalf("threshold crossing logged %d times, want 1; log: %s", got, buf.String())
	}
	// A different source logs its own crossing.
	l.failure("198.51.100.8", now)
	l.failure("198.51.100.8", now)
	l.failure("198.51.100.8", now)
	l.failure("198.51.100.8", now)
	l.failure("198.51.100.8", now)
	if got := strings.Count(buf.String(), "198.51.100.8 crossed"); got != 1 {
		t.Fatalf("second source crossing logged %d times, want 1; log: %s", got, buf.String())
	}
}

// TestAuthLimiterLogsAFullTable pins the other silent bound: at the hard
// address cap every new source costs an eviction, so previously tracked
// sources start being forgotten. That must be visible, and only once per full
// episode because a spoofed flood would otherwise log per request.
func TestAuthLimiterLogsAFullTable(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var l authLimiter
	now := time.Now()
	// Every entry stays fresh and unlocked, so no idle expiry frees a slot.
	for i := range authMaxIPs {
		l.failure(strconv.Itoa(i), now)
	}
	// Attempts past the cap evict, and log only the first of them.
	for i := range 5 {
		l.failure("overflow-"+strconv.Itoa(i), now)
	}
	if got := strings.Count(buf.String(), "address cap"); got != 1 {
		t.Fatalf("full table logged %d times, want 1; log: %s", got, buf.String())
	}
	if len(l.entries) != authMaxIPs {
		t.Fatalf("limiter holds %d entries, want the cap %d", len(l.entries), authMaxIPs)
	}
	// A lull (len drops below the cap) re-arms the report for the next
	// episode instead of staying quiet forever.
	l.mu.Lock()
	for key := range l.entries {
		delete(l.entries, key)
		break
	}
	l.mu.Unlock()
	l.failure("after-lull", now)
	for i := range 5 {
		l.failure("again-"+strconv.Itoa(i), now)
	}
	if got := strings.Count(buf.String(), "address cap"); got != 2 {
		t.Fatalf("table refilled and logged %d times total, want 2; log: %s", got, buf.String())
	}
}

// TestCrossOriginCheckIsCaseInsensitive pins the host comparison: RFC 9110
// makes the host case-insensitive, but net/http compares the Origin host with
// r.Host byte for byte, so a case-preserving ingress or a mixed-case Host
// refused every mutation. Both directions are covered, and a genuinely foreign
// origin must still be refused.
func TestCrossOriginCheckIsCaseInsensitive(t *testing.T) {
	const token = "operator-fixture-credential"
	for _, tc := range []struct {
		name   string
		host   string
		origin string
		want   int
	}{
		{"same case", "proxy.example", "https://proxy.example", http.StatusNoContent},
		{"mixed-case host", "Proxy.Example", "https://proxy.example", http.StatusNoContent},
		{"mixed-case origin", "proxy.example", "https://PROXY.Example", http.StatusNoContent},
		{"mixed-case both with port", "Proxy.Example:8443", "https://proxy.EXAMPLE:8443", http.StatusNoContent},
		{"a different host still refuses", "proxy.example", "https://foreign.example", http.StatusForbidden},
		{"a different port still refuses", "proxy.example:8443", "https://proxy.example:9443", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newOperatorGate(token)
			h := protectOperatorRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), gate)
			r := httptest.NewRequest(http.MethodPost, "http://placeholder/metrics/bootstrap", nil)
			r.Host = tc.host
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestDoubleSlashSpellingsAreRefused pins the one gap in the non-canonical
// refusal: "//" passed the canonical test, so the gate admitted it and the mux
// then answered a 301, which is exactly the unauthenticated redirect the gate
// promises not to take. Every duplicated-separator spelling of an owned
// namespace must be refused 404 instead, while the single trailing separator
// that IS canonical for a subtree route stays admitted.
func TestNonCanonicalPathTable(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/", false},
		{"/index.html", false},
		{"/metrics/bootstrap", false},
		{"/metrics/", false},
		{"/metrics/pprof/", false},
		{"/admin/config", false},
		{"//metrics/pprof", true},
		{"//metrics/pprof/", true},
		{"//", true},
		{"//index.html", true},
		{"/metrics//pprof", true},
		{"/metrics//", true},
		{"/./metrics", true},
		{"/metrics/.", true},
		{"/metrics/..", true},
	} {
		if got := nonCanonicalPath(tc.path); got != tc.want {
			t.Errorf("nonCanonicalPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestDoubleSlashIsRefusedNotRedirected pins the same rule through the real
// gate: a doubled leading separator answers the reserved 404 with no Location
// header, so a caller never has to follow a redirect to learn the namespace
// exists.
func TestDoubleSlashIsRefusedNotRedirected(t *testing.T) {
	const token = "operator-fixture-credential"
	gate := newOperatorGate(token)
	_, handler := newOperatorMux(gate, config.Default())
	for _, tc := range []struct{ path, credential string }{
		{"//", ""},
		{"//metrics/bootstrap", token},
		{"//metrics/bootstrap", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.example"+tc.path, nil)
		if tc.credential != "" {
			r.Header.Set("Authorization", "Bearer "+tc.credential)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want the reserved 404", tc.path, w.Code)
		}
		if location := w.Header().Get("Location"); location != "" {
			t.Errorf("GET %s: Location = %q, want no redirect", tc.path, location)
		}
	}
}
