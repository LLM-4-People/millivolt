package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
	"github.com/LLM-4-People/millivolt/internal/web"
)

// TestOperatorPlaneBoundary is the deny-by-default regression for the whole
// dashboard gate: only /healthz and the inference catch-all pass without the
// credential, every dashboard/metrics/admin route needs it (Bearer or the
// session cookie the gate mints), the unauthenticated HTML entrypoint gets
// the login page, same-origin mutation checks stay armed behind the
// credential, and an unconfigured credential denies the whole plane.
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
		// A provider credential rides the same header name; the gate must
		// never consume or reject it outside the operator plane.
		requests[1].Header.Set("Authorization", "Bearer provider-key")
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
			httptest.NewRequest(http.MethodGet, "http://proxy.example/favicon.ico", nil),
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
	})

	t.Run("unauthenticated HTML entry gets the login page", func(t *testing.T) {
		h := protectOperatorRequests(next, newOperatorGate(token))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/", nil))
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/admin/session"`) {
			t.Fatalf("GET / without credential: status=%d want=401 login page", w.Code)
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
		for _, target := range []string{"/healthz", "/v1/chat/completions", "/admin/session"} {
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
		// Wrong credential: form gets the page back, API gets JSON.
		w = httptest.NewRecorder()
		bad := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token=wrong-credential"))
		bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(w, bad)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `action="/admin/session"`) {
			t.Errorf("wrong form credential: status=%d want=401 login page", w.Code)
		}
		// The login page itself is inert without a credential: 401.
		w = httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("empty session post: status=%d want=401", w.Code)
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

// TestOperatorTokenEnv owns the boot-credential contract: unset denies the
// whole plane (armed, no credential); set but empty or too short fails the
// boot instead of silently degrading to a weaker policy.
func TestOperatorTokenEnv(t *testing.T) {
	if value, err := loadOperatorToken(); value != "" || err != nil {
		t.Errorf("unset token: value=%q err=%v, want \"\", nil", value, err)
	}
	t.Setenv(operatorTokenEnv, "")
	if value, err := loadOperatorToken(); value != "" || err != nil {
		t.Errorf("empty token: value=%q err=%v, want \"\", nil (compose interpolations yield empty; treat as unset)", value, err)
	}
	t.Setenv(operatorTokenEnv, "short")
	if _, err := loadOperatorToken(); err == nil {
		t.Error("short token: want boot failure")
	}
	t.Setenv(operatorTokenEnv, strings.Repeat("x", operatorTokenMaxLen+1))
	if _, err := loadOperatorToken(); err == nil {
		t.Error("oversized token: want boot failure (it could never be presented via Bearer)")
	}
	const credential = "operator-fixture-credential"
	t.Setenv(operatorTokenEnv, credential)
	value, err := loadOperatorToken()
	if err != nil || value != credential {
		t.Errorf("valid token: value=%q err=%v, want %q, nil", value, err, credential)
	}
}

// livePayload decodes the bootstrap/SSE payload wrapper for snapshot
// assertions (mirrors the internal/metrics feed-test fixture).
type livePayload struct {
	Records     []*metrics.Record `json:"records"`
	Seq         int64             `json:"seq"`
	OldestSeq   int64             `json:"oldest_seq"`
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
	raw, err := b.SnapshotJSONSince(0)
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
	raw, err := b.SnapshotJSONSince(1)
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
