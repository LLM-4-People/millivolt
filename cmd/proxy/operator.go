package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The operator plane is the whole embedded dashboard, standardized as the
// gated route set below: dashboard HTML/assets, every /metrics/* surface and
// every /admin/* action require the operator credential. Open surfaces are
// the /healthz liveness probe (Docker healthchecks and load balancers cannot
// carry the credential), /favicon.ico (browsers fetch it without
// Authorization, and it carries no dashboard data), and transparent
// inference (the mux catch-all; provider credentials ride the same header
// name and are never inspected). The gate is this file's middleware, the
// single lowest chokepoint every request passes through (main wraps the whole
// mux with it, including the restart-handoff clone).

// operatorTokenEnv holds the operator credential. It is read once at boot and
// never enters config.Config, YAML, /admin/config output, snapshots or logs.
// The restart handoff spawns the child with the parent's environment, so the
// credential survives rebuilds without extra wiring.
const operatorTokenEnv = "MILLIVOLT_OPERATOR_TOKEN"

// operatorTokenMinLen rejects trivially guessable credentials at boot. The
// gate is fail closed: an unusable credential is a configuration failure, the
// same class as an invalid config file. operatorTokenMaxLen matches the
// Bearer presentation cap in bearerToken: a longer configured credential
// could never be presented and would silently break header auth, so the boot
// rejects it instead.
const (
	operatorTokenMinLen = 16
	operatorTokenMaxLen = 512
)

// Session cookie bounds. The cookie exists because EventSource cannot send
// Authorization headers: it is HttpOnly (script never reads it), SameSite
// Strict (cross-site navigation never carries it), and its HMAC is derived
// from the operator token so a container restart with the same credential
// keeps the session. Rotating the token invalidates outstanding cookies.
const (
	sessionCookieName = "millivolt-operator"
	sessionCookieTTL  = 12 * time.Hour
	sessionNonceLen   = 16
	// Bounds the pre-auth login form's body phase: bytes are capped by
	// MaxBytesReader, this caps the time. Internal guardrail, not tunable.
	sessionBodyReadTimeout = 10 * time.Second
)

// Failed-authentication throttle (per source IP, RemoteAddr only: XFF-style
// headers are never trusted because no trusted-proxy model exists). The
// defaults follow the fail2ban/OWASP practice of a small failure budget with
// escalating lockout instead of permanent lockout, so a forgotten operator
// cannot brick the dashboard.
const (
	authFailureThreshold = 5                // consecutive failures before a lockout
	authLockoutBase      = 10 * time.Second // first lockout; doubles per further failure
	authLockoutMax       = 15 * time.Minute
	authEntryTTL         = 30 * time.Minute // outlives authLockoutMax so lockouts cannot be aged out
	authMaxIPs           = 4096             // hard memory bound against spoofed source floods
)

// loadOperatorToken resolves the boot credential. Unset or empty arms the
// gate with no credential (compose interpolations routinely yield empty
// strings, so empty is treated as unset with a warning): every gated request
// is denied while the liveness probe and inference keep working. A nonempty
// credential shorter than operatorTokenMinLen or longer than
// operatorTokenMaxLen fails the boot loudly rather than degrading to a
// weaker policy silently.
func loadOperatorToken() (string, error) {
	value, ok := os.LookupEnv(operatorTokenEnv)
	switch {
	case !ok:
		log.Printf("operator plane disabled: %s is not set, dashboard and admin endpoints deny all requests", operatorTokenEnv)
		return "", nil
	case value == "":
		log.Printf("operator plane disabled: %s is empty, dashboard and admin endpoints deny all requests", operatorTokenEnv)
		return "", nil
	case len(value) < operatorTokenMinLen:
		return "", errors.New(operatorTokenEnv + " must be at least " + strconv.Itoa(operatorTokenMinLen) + " characters")
	case len(value) > operatorTokenMaxLen:
		return "", errors.New(operatorTokenEnv + " must be at most " + strconv.Itoa(operatorTokenMaxLen) + " characters")
	}
	log.Printf("operator plane enabled (%s)", operatorTokenEnv)
	return value, nil
}

// mustOperatorToken is the boot path: an invalid credential is fatal, the
// same class as an invalid config file.
func mustOperatorToken() string {
	token, err := loadOperatorToken()
	if err != nil {
		log.Fatalf("operator token: %v", err)
	}
	return token
}

// operatorGate owns the credential state: the token digest (hashed so the
// comparison cannot leak length), the cookie MAC key derived from that
// token, and the per-IP failure throttle.
type operatorGate struct {
	// armed distinguishes "no credential configured" from a configured
	// credential: an unset MILLIVOLT_OPERATOR_TOKEN still hashes to a real
	// SHA-256 digest, so the flag owns the deny-all state.
	armed      bool
	tokenHash  [sha256.Size]byte
	sessionKey [32]byte
	limiter    authLimiter
}

func newOperatorGate(token string) *operatorGate {
	g := &operatorGate{armed: token != ""}
	g.tokenHash = sha256.Sum256([]byte(token))
	if token != "" {
		g.sessionKey = sessionKeyFromToken(token)
	} else if _, err := rand.Read(g.sessionKey[:]); err != nil {
		log.Fatalf("operator gate: session key: %v", err)
	}
	return g
}

// sessionKeyFromToken derives the cookie MAC key from the operator token so
// a restarted process with the same credential accepts cookies it minted.
func sessionKeyFromToken(token string) [32]byte {
	mac := hmac.New(sha256.New, []byte("millivolt-operator-session-key-v1"))
	mac.Write([]byte(token))
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

// valid reports whether the presented credential matches, comparing fixed
// SHA-256 digests so token content and length both stay secret.
func (g *operatorGate) valid(presented string) bool {
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], g.tokenHash[:]) == 1
}

// gatedPath reports whether the request belongs to the dashboard/operator
// plane. The exact namespace roots are gated too: /admin, /metrics and /dash
// are millivolt-owned, so a look-alike path can never reach the inference
// catch-all. Everything else (registered /healthz, /favicon.ico and the
// catch-all) passes untouched.
func gatedPath(path string) bool {
	return path == "/" || path == "/index.html" ||
		path == "/admin" || path == "/metrics" || path == "/dash" ||
		strings.HasPrefix(path, "/dash/") ||
		strings.HasPrefix(path, "/metrics/") ||
		strings.HasPrefix(path, "/admin/")
}

// bearerToken extracts the presented credential verbatim. The scheme is
// case-insensitive per RFC 9110; the credential itself is not trimmed.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) || len(header) > 512 {
		return "", false
	}
	return header[len(prefix):], true
}

// sessionMAC signs the cookie's expiry instant and per-mint nonce. The key
// is derived from the operator token (stable across process restarts); the
// nonce means concurrent operators never share one value (a future denylist
// can target single cookies).
func (g *operatorGate) sessionMAC(expires int64, nonce []byte) []byte {
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(expires))
	mac := hmac.New(sha256.New, g.sessionKey[:])
	mac.Write([]byte("millivolt-operator-session"))
	mac.Write(stamp[:])
	mac.Write(nonce)
	return mac.Sum(nil)
}

func (g *operatorGate) mintSessionCookie() (*http.Cookie, error) {
	expires := time.Now().Add(sessionCookieTTL).Unix()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	value := "v1." + strconv.FormatInt(expires, 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce) + "." +
		base64.RawURLEncoding.EncodeToString(g.sessionMAC(expires, nonce))
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}, nil
}

// validSession verifies the cookie: known version, unexpired, signed.
func (g *operatorGate) validSession(c *http.Cookie) bool {
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(nonce) != sessionNonceLen {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(got) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(got, g.sessionMAC(expires, nonce)) == 1
}

// authLimiter bounds failed-authentication retries per source IP. Entries
// expire lazily and the map is hard-capped: a flood of spoofed source
// addresses can never grow it beyond authMaxIPs.
type authLimiter struct {
	mu      sync.Mutex
	entries map[string]*ipState
}

type ipState struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "invalid"
	}
	return host
}

// locked reports whether the IP is currently locked out and for how long
// (rounded up, so the coarse Retry-After never understates the window).
func (l *authLimiter) locked(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.entries[ip]
	if !ok || !now.Before(state.lockedUntil) {
		return false, 0
	}
	return true, state.lockedUntil.Sub(now).Round(time.Second)
}

// failure records one rejected credential and returns the new lockout, if
// any: none before authFailureThreshold, then authLockoutBase doubling per
// additional failure up to authLockoutMax. Tracked IPs never trigger
// eviction: only genuinely new sources pay the at-cap cost, and entries
// with a live lockout are evicted last.
func (l *authLimiter) failure(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*ipState, 64)
	}
	state, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= authMaxIPs {
			l.evictLocked(now)
		}
		if len(l.entries) >= authMaxIPs {
			return 0 // at capacity even after eviction: deny without tracking
		}
		state = &ipState{}
		l.entries[ip] = state
	}
	state.lastSeen = now
	state.failures++
	if state.failures < authFailureThreshold {
		return 0
	}
	lockout := authLockoutBase << min(state.failures-authFailureThreshold, 16)
	if lockout > authLockoutMax || lockout <= 0 {
		lockout = authLockoutMax
	}
	if until := now.Add(lockout); until.After(state.lockedUntil) {
		state.lockedUntil = until
	}
	return lockout
}

// evictLocked frees at least one slot: fully expired entries go first, then
// the oldest unlocked entry, and only when every entry is actively locked
// the oldest lockout itself. A live lockout is never the first casualty.
func (l *authLimiter) evictLocked(now time.Time) {
	var unlockedKey, lockedKey string
	var unlockedAt, lockedUntil time.Time
	for key, state := range l.entries {
		if now.Sub(state.lastSeen) >= authEntryTTL {
			delete(l.entries, key)
			return
		}
		if now.Before(state.lockedUntil) {
			if lockedKey == "" || state.lockedUntil.Before(lockedUntil) {
				lockedKey, lockedUntil = key, state.lockedUntil
			}
			continue
		}
		if unlockedKey == "" || state.lastSeen.Before(unlockedAt) {
			unlockedKey, unlockedAt = key, state.lastSeen
		}
	}
	if unlockedKey != "" {
		delete(l.entries, unlockedKey)
		return
	}
	if lockedKey != "" {
		delete(l.entries, lockedKey)
	}
}

// success clears the IP's history: an authenticated operator resets the
// budget, so the lockout only ever rates limits wrong guesses.
func (l *authLimiter) success(ip string) {
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}

// protectOperatorRequests is the operator-plane gate. Order is fixed: open
// paths pass first (so an unset credential never takes down /healthz or
// inference), a disabled plane denies everything gated, a verified Bearer or
// session cookie admits the request before the throttle (a correct
// credential is proof, not a guess: the lockout must not lock the operator
// out of their own dashboard), and only then does an active lockout deny
// wrong-guess traffic. Denied HTML entrypoints get the login page; every
// other denial is JSON, no-store, identical in shape.
func protectOperatorRequests(next http.Handler, gate *operatorGate) http.Handler {
	sameOrigin := http.NewCrossOriginProtection().Handler(next)
	deny := func(w http.ResponseWriter, code int, message string) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		http.Error(w, `{"error":"`+message+`"}`, code)
	}
	mint := func(w http.ResponseWriter) {
		if cookie, err := gate.mintSessionCookie(); err == nil {
			// The cookie rides on every authenticated response: force
			// no-store so no cache retains a Set-Cookie body.
			w.Header().Set("Cache-Control", "no-store")
			http.SetCookie(w, cookie)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The session handshake is the one /admin route that must stay
		// reachable without a credential: it is how a credential becomes a
		// cookie. It owns its throttling and runs behind its own
		// same-origin checks (registered in main). The exemption requires
		// the path to be canonical in both the decoded and escaped views so
		// an encoded-slash look-alike can never ride it.
		if r.URL.Path == "/admin/session" && r.URL.EscapedPath() == "/admin/session" {
			next.ServeHTTP(w, r)
			return
		}
		if !gatedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !gate.armed {
			deny(w, http.StatusForbidden, "operator plane is disabled: set "+operatorTokenEnv+" to enable it")
			return
		}
		ip := clientIP(r)
		// A verified credential is admission, not a guess: both the session
		// cookie and a correct Bearer pass before the throttle and clear the
		// budget, so a lockout can never lock the operator out of their own
		// dashboard while still throttling every wrong candidate.
		if cookie, err := r.Cookie(sessionCookieName); err == nil && gate.validSession(cookie) {
			sameOrigin.ServeHTTP(w, r)
			return
		}
		credential, hasBearer := bearerToken(r)
		if hasBearer && gate.valid(credential) {
			gate.limiter.success(ip)
			mint(w)
			sameOrigin.ServeHTTP(w, r)
			return
		}
		if locked, retryIn := gate.limiter.locked(ip, time.Now()); locked {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
			w.Header().Set("Retry-After", strconv.Itoa(int(max(retryIn/time.Second, 1))))
			http.Error(w, `{"error":"too many rejected credentials; try again later"}`, http.StatusTooManyRequests)
			return
		}
		if hasBearer {
			// Only presented-but-wrong credentials count as brute force.
			// Credential-less requests (an expired session cookie polling in
			// a background tab) are denied without burning the budget: they
			// carry no candidate to guess.
			gate.limiter.failure(ip, time.Now())
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(loginPageHTML))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="millivolt-operator"`)
		deny(w, http.StatusUnauthorized, "operator token required")
	})
}

// handleAdminSession is POST /admin/session: the one open operator-plane
// route, because it is the handshake that mints the session cookie. It
// accepts the credential as a Bearer header (API clients) or as the single
// "token" form field (the no-JS login page), throttles wrong candidates like
// every other gated request, and never reveals which of the two was wrong.
func (g *operatorGate) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	if !g.armed {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, `{"error":"operator plane is disabled: set `+operatorTokenEnv+` to enable it"}`, http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	form := false
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil {
		form = mediaType == "application/x-www-form-urlencoded"
	}
	credential := ""
	presented := false
	if value, ok := bearerToken(r); ok {
		credential, presented = value, true
	} else if form {
		// The handshake is pre-auth: bound the body in time as well as
		// bytes, so a dribbling client cannot hold a goroutine indefinitely.
		// Inference and SSE keep the server's no-body-timeout design. A
		// transport that cannot set deadlines proceeds byte-bounded only.
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(sessionBodyReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, `{"error":"unsupported connection"}`, http.StatusInternalServerError)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, `{"error":"invalid form body"}`, http.StatusBadRequest)
			return
		}
		credential = r.PostForm.Get("token")
		presented = credential != ""
	}
	// A verified credential is admission (and clears the throttle); a wrong
	// candidate is throttled; an empty request is a plain 401.
	if presented && g.valid(credential) {
		g.limiter.success(ip)
		w.Header().Set("Cache-Control", "no-store")
		if cookie, err := g.mintSessionCookie(); err == nil {
			http.SetCookie(w, cookie)
		}
		if form {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if presented {
		g.limiter.failure(ip, time.Now())
	}
	if locked, retryIn := g.limiter.locked(ip, time.Now()); locked {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", strconv.Itoa(int(max(retryIn/time.Second, 1))))
		http.Error(w, `{"error":"too many rejected credentials; try again later"}`, http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	if form {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginPageHTML))
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="millivolt-operator"`)
	http.Error(w, `{"error":"operator token required"}`, http.StatusUnauthorized)
}

// loginPageHTML is the whole pre-auth surface: self-contained (the gated
// /dash assets are unreachable by design), no script, and it only ever
// forwards the entered value to POST /admin/session.
const loginPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<title>millivolt operator sign-in</title>
<style>
* { box-sizing: border-box; }
:root { color-scheme: dark; }
html, body { margin: 0; min-height: 100dvh; overflow-x: hidden; }
body {
  display: grid; place-items: center;
  padding: max(24px, env(safe-area-inset-top)) max(20px, env(safe-area-inset-right))
    max(24px, env(safe-area-inset-bottom)) max(20px, env(safe-area-inset-left));
  background: #141210; color: #e8e4de;
  font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  -webkit-font-smoothing: antialiased;
}
form {
  width: min(22rem, 100%);
  border: 1px solid #3a342b; border-radius: 14px; background: #1a1714;
  padding: 22px; box-shadow: 0 18px 40px rgba(0,0,0,.35);
}
.brand { display: flex; align-items: center; gap: 11px; margin: 0 0 14px; }
.logo {
  width: 28px; height: 28px; border-radius: 9px; flex-shrink: 0;
  background: radial-gradient(130% 130% at 30% 22%, #2a2440 0%, #1b1826 70%);
  display: grid; place-items: center;
  box-shadow: 0 0 0 1px rgba(154,107,255,.28) inset, 0 4px 16px rgba(154,107,255,.28);
}
h1 { margin: 0; font: 650 15px/1.2 ui-monospace, "SFMono-Regular", Menlo, monospace; letter-spacing: -.01em; }
p { color: #97907e; margin: 0 0 16px; font-size: 13px; }
input {
  width: 100%; font: 13px/1.4 ui-monospace, "SFMono-Regular", Menlo, monospace;
  color: #e8e4de; background: #100e0b; border: 1px solid #2b2620;
  border-radius: 8px; padding: 10px 12px; min-height: 42px;
}
input:focus { border-color: #5b8cff; outline: none; box-shadow: 0 0 0 3px rgba(91,140,255,.16); }
button {
  font: 650 13px/1.2 inherit; margin-top: 12px; width: 100%; min-height: 42px;
  color: #141210; background: #5b8cff; border: 1px solid #5b8cff;
  border-radius: 8px; padding: 10px 12px; cursor: pointer;
}
button:hover { background: #7aa0ff; border-color: #7aa0ff; }
button:focus-visible { outline: 2px solid #5b8cff; outline-offset: 2px; }
</style>
</head>
<body>
<form method="post" action="/admin/session">
<div class="brand">
<div class="logo" aria-hidden="true">
<svg width="18" height="18" viewBox="0 0 18 18" fill="none">
<g stroke="#7a72a0" stroke-width="1" opacity=".55" stroke-linecap="round">
<path d="M2 13.5v1.6M5.5 13.5v1.6M9 13.5v1.6M12.5 13.5v1.6M16 13.5v1.6"/>
</g>
<path d="M1.5 12.8H16.5" stroke="#56507a" stroke-width="1" opacity=".5" stroke-linecap="round"/>
<path d="M1.5 9.5 4 9.5 5.6 4.6 8 13.2 10.4 6.8 12.4 9.5 16.5 9.5" stroke="#9a6bff" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>
</svg>
</div>
<h1>millivolt</h1>
</div>
<p>This dashboard is protected. Enter the MILLIVOLT_OPERATOR_TOKEN value.</p>
<input type="password" name="token" autocomplete="current-password" autofocus aria-label="Operator token" required>
<button type="submit">Sign in</button>
</form>
</body>
</html>
`

// handleHealthz is GET /healthz: the unauthenticated liveness probe for
// Docker HEALTHCHECK and load balancers. It reports only that the process is
// up and serving HTTP on this listener; storage depth lives on the
// /metrics/prometheus and dashboard surfaces.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, `{"error":"GET or HEAD only"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
