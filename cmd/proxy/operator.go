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
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	stdlibpath "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// The operator plane is the whole embedded dashboard, standardized as the
// gated route set below: dashboard HTML/assets, every /metrics/* surface and
// every /admin/* action require the operator credential. Open surfaces are
// the /healthz liveness probe (Docker healthchecks and load balancers cannot
// carry the credential), origin-root brand/PWA files (/favicon.ico,
// /favicon.svg, icons, /manifest.webmanifest, /sw.js; browsers fetch them
// without Authorization and they carry no dashboard data), and transparent
// inference (the mux catch-all; provider credentials ride the same header
// name and are never inspected). The gate is this file's middleware, the
// single lowest chokepoint every request passes through (main wraps the whole
// mux with it, including the restart-handoff clone).

// The operator credential's variable name and length band come from
// internal/mcp, the one owner the MCP server publishes and enforces:
// mcp.ProxyTokenEnv is the variable this gate reads, and a credential shorter
// than mcp.OperatorTokenMinLen or longer than mcp.OperatorTokenMaxLen (the
// Bearer presentation cap in bearerToken) fails the boot loudly rather than
// degrading to a weaker policy silently. It is read once at boot and never
// enters config.Config, YAML, /admin/config output, snapshots or logs. The
// restart handoff spawns the child with the parent's environment, so the
// credential survives rebuilds without extra wiring.

// Session cookie bounds. The cookie exists because EventSource cannot send
// Authorization headers: it is HttpOnly (script never reads it), SameSite
// Strict (cross-site navigation never carries it), and its HMAC is derived
// from the operator token so a container restart with the same credential
// keeps the session. Rotating the token invalidates outstanding cookies.
// Secure is transport-derived (TLS or an ingress-forwarded https scheme)
// because the built-in listener is plain HTTP.
const (
	sessionCookieName = "millivolt-operator"
	sessionCookieTTL  = 12 * time.Hour
	sessionNonceLen   = 16
	// Bounds the pre-auth login form's body phase: bytes are capped by
	// MaxBytesReader, this caps the time. Internal guardrail, not tunable.
	sessionBodyReadTimeout = 10 * time.Second
	// Byte cap for the pre-auth login form's body (the single "token"
	// field). Internal guardrail, not tunable.
	sessionFormBodyMax = 4096
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
// credential outside mcp.OperatorTokenMinLen..MaxLen fails the boot loudly
// rather than degrading to a weaker policy silently.
func loadOperatorToken() (string, error) {
	value, ok := os.LookupEnv(mcp.ProxyTokenEnv)
	switch {
	case !ok:
		log.Printf("operator plane disabled: %s is not set, dashboard and admin endpoints deny all requests", mcp.ProxyTokenEnv)
		return "", nil
	case value == "":
		log.Printf("operator plane disabled: %s is empty, dashboard and admin endpoints deny all requests", mcp.ProxyTokenEnv)
		return "", nil
	case len(value) < mcp.OperatorTokenMinLen:
		return "", errors.New(mcp.ProxyTokenEnv + " must be at least " + strconv.Itoa(mcp.OperatorTokenMinLen) + " bytes")
	case len(value) > mcp.OperatorTokenMaxLen:
		return "", errors.New(mcp.ProxyTokenEnv + " must be at most " + strconv.Itoa(mcp.OperatorTokenMaxLen) + " bytes")
	}
	log.Printf("operator plane enabled (%s)", mcp.ProxyTokenEnv)
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
	// credential: an unset mcp.ProxyTokenEnv still hashes to a real
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
// plane. The exact namespace roots are gated too: /admin, /session, /metrics,
// /dash and /mcp are millivolt-owned, so a look-alike path can never reach the
// inference catch-all. A decoded backslash is read as a separator alongside a
// slash: no Go mux pattern does, but upstream proxies and caches historically
// have, so the namespace test cannot depend on the next hop's reading.
// Everything else (registered /healthz, brand/PWA files and the catch-all)
// passes untouched.
func gatedPath(path string) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	return path == "/" || path == "/index.html" ||
		path == "/admin" || path == "/session" || path == "/metrics" || path == "/dash" ||
		strings.HasPrefix(path, "/dash/") ||
		strings.HasPrefix(path, "/metrics/") ||
		strings.HasPrefix(path, "/admin/") ||
		strings.HasPrefix(path, "/session/") ||
		mcpNamespace(path)
}

// mcpNamespace reports the millivolt-owned /mcp namespace, normalized the same
// way gatedPath normalizes it. The gate uses it to keep the session cookie off
// every MCP response: /mcp is Bearer-only, so a cookie minted there would sit
// unused in an MCP client's cookie jar as a full dashboard credential.
func mcpNamespace(path string) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	return path == "/mcp" || strings.HasPrefix(path, "/mcp/")
}

// encodedSeparator reports whether an escaped path contains a percent-encoded
// path separator (%2f or %5c, any hex case). ServeMux segments an escaped path
// without decoding separators, so such a path cannot match the reserved
// namespace subtree and would fall through to the inference catch-all.
func encodedSeparator(escapedPath string) bool {
	lower := strings.ToLower(escapedPath)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// bearerToken extracts the presented credential verbatim. The scheme is
// case-insensitive per RFC 9110; the credential itself is not trimmed.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := header[len(prefix):]
	if token == "" || len(token) > mcp.OperatorTokenMaxLen {
		return "", false
	}
	return token, true
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

// sessionCookieSecure derives the external scheme from the request. A direct
// TLS connection is authoritative. Behind an ingress that terminates TLS the
// forwarding header carries the client scheme; it is a comma-separated list,
// so ANY token naming a secure scheme wins. A single trusted hop that
// overwrites the header (the shipped NGINX example does) leaves exactly one
// token; a hop that appends its own scheme leaves the client's first, which is
// why a first-token test could downgrade a secure presentation and force
// Secure off. wss counts as secure: a WebSocket presentation is TLS too. Plain
// HTTP (the default listener, including loopback development) keeps Secure
// false so the browser still stores the cookie.
func sessionCookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	for _, proto := range strings.Split(r.Header.Get("X-Forwarded-Proto"), ",") {
		switch strings.ToLower(strings.TrimSpace(proto)) {
		case "https", "wss":
			return true
		}
	}
	return false
}

// sessionCookie is the one owner of every session-cookie attribute, so the
// minting paths and their tests cannot drift. Secure follows the request
// transport; the remaining attributes are fixed policy.
func sessionCookie(r *http.Request, value string) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   sessionCookieSecure(r),
		SameSite: http.SameSiteStrictMode,
	}
}

func (g *operatorGate) mintSessionCookie(r *http.Request) (*http.Cookie, error) {
	expires := time.Now().Add(sessionCookieTTL).Unix()
	nonce := make([]byte, sessionNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	value := "v1." + strconv.FormatInt(expires, 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce) + "." +
		base64.RawURLEncoding.EncodeToString(g.sessionMAC(expires, nonce))
	return sessionCookie(r, value), nil
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
	// fullLogged makes the at-capacity report one line per full episode
	// instead of one per evicted request; it clears as soon as the table has
	// room again.
	fullLogged bool
}

type ipState struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// clientIP is the throttle key for one request: the source address the client
// was seen from. The immediate hop is the only trusted one, so the RIGHTMOST
// X-Forwarded-For element wins - that is the address the last proxy appended,
// which a client cannot forge by prepending to the header. NGINX's default
// $proxy_add_x_forwarded_for appends rather than overwrites, and the shipped
// example resolves its own peer first (real_ip), so the rightmost element is
// the real client. A parser-friendly address is required; an element that is
// not an IP (a hostname, junk) is skipped rather than used as a fresh,
// attacker-chosen bucket. A directly exposed instance still lets a client pick
// its own key by setting the header: the throttle is a rate limit, never the
// credential decision.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if addr, ok := forwardedAddress(parts[i]); ok {
				return addr
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "invalid"
	}
	return host
}

// forwardedAddress normalizes one X-Forwarded-For element. Proxies send a bare
// address; some append a port or bracket an IPv6 literal, so both shapes are
// accepted through the same split.
func forwardedAddress(element string) (string, bool) {
	element = strings.TrimSpace(element)
	if host, _, err := net.SplitHostPort(element); err == nil {
		element = host
	}
	element = strings.Trim(element, "[]")
	ip := net.ParseIP(element)
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}

// locked reports whether the IP is currently locked out and for how long
// (rounded up to whole seconds, so the coarse Retry-After never
// understates the window).
func (l *authLimiter) locked(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.entries[ip]
	if !ok || !now.Before(state.lockedUntil) {
		return false, 0
	}
	remaining := state.lockedUntil.Sub(now)
	if r := remaining % time.Second; r != 0 {
		remaining += time.Second - r
	}
	return true, remaining
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
			if len(l.entries) >= authMaxIPs {
				// Nothing could be freed: the source is denied without being
				// tracked, so its failures never escalate to a lockout.
				if !l.fullLogged {
					log.Printf("operator gate: failure table holds its %d address cap and cannot free a slot; new sources are denied without tracking", authMaxIPs)
					l.fullLogged = true
				}
				return 0
			}
			// The cap is a hard memory bound, so a genuinely new source costs
			// an eviction and a previously tracked source is forgotten from
			// now on. Logged once per full episode, not per request: under a
			// spoofed flood every request would otherwise add a line.
			if !l.fullLogged {
				log.Printf("operator gate: failure table at its %d address cap; the oldest entry was evicted for a new source", authMaxIPs)
				l.fullLogged = true
			}
		} else {
			l.fullLogged = false
		}
		state = &ipState{}
		l.entries[ip] = state
	}
	state.lastSeen = now
	state.failures++
	if state.failures < authFailureThreshold {
		return 0
	}
	if state.failures == authFailureThreshold {
		// The crossing is logged once per source, not on every later attempt:
		// the dashboard would otherwise gain a line per guess.
		log.Printf("operator gate: %s crossed %d rejected credentials; lockouts start now", ip, authFailureThreshold)
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
// inference), a percent-encoded-separator look-alike of an owned namespace is
// refused 404 before the plane state or any credential is consulted (so no
// lookup form of the namespace can ever be forwarded), a disabled plane denies
// everything else gated, a verified Bearer or session cookie admits the
// request before the throttle (a correct credential is proof, not a guess: the
// lockout must not lock the operator out of their own dashboard), and only
// then does an active lockout deny wrong-guess traffic. Denied HTML
// entrypoints get the login page; every other denial is JSON, no-store,
// identical in shape.
// nonCanonicalPath reports whether a decoded path uses a spelling the mux
// would clean or redirect before dispatching: a duplicated separator or a dot
// segment. A single trailing separator is canonical for a subtree route
// (/metrics/pprof/), so it is preserved rather than stripped.
func nonCanonicalPath(path string) bool {
	// A doubled LEADING separator is never canonical: the mux cleans it and
	// answers a redirect, which is the one extra hop the gate promises never to
	// hand an unauthenticated caller.
	if strings.HasPrefix(path, "//") {
		return true
	}
	clean := strings.TrimSuffix(path, "/")
	if clean == "" {
		clean = "/"
	}
	cleaned := strings.TrimSuffix(stdlibpath.Clean(clean), "/")
	if cleaned == "" {
		cleaned = "/"
	}
	if path == clean {
		return stdlibpath.Clean(clean) != clean
	}
	return cleaned+"/" != path || stdlibpath.Clean(clean) != clean
}

// sameOriginGuard is the one owner of the cross-origin control and its denial
// shape. The stdlib Handler answers a refused request with a bare http.Error,
// which was the single denial in the operator plane without no-store and
// frame-ancestors, and its handler API carries no error for a deny writer to
// report. Driving the exported Check here keeps the control and sends the real
// reason through the shared denial writer instead.
//
// The request is normalized first: net/http compares the parsed Origin host
// with r.Host byte for byte, so a case-preserving ingress, or a browser on a
// mixed-case Host, refused every mutation although RFC 9110 makes the host
// case-insensitive.
func sameOriginGuard(next http.Handler) http.Handler {
	protection := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		normalizeOriginCase(r)
		if err := protection.Check(r); err != nil {
			denyOperator(w, http.StatusForbidden, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeOriginCase lowercases both sides of the origin comparison. The
// Origin header is rewritten only from its scheme separator onward, so a
// malformed value is left as sent and still fails closed. r.Host is a copy on
// this request and host names are case-insensitive everywhere it is read.
func normalizeOriginCase(r *http.Request) {
	r.Host = strings.ToLower(r.Host)
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if i := strings.Index(origin, "://"); i >= 0 {
		r.Header.Set("Origin", origin[:i+3]+strings.ToLower(origin[i+3:]))
	}
}

func protectOperatorRequests(next http.Handler, gate *operatorGate) http.Handler {
	sameOrigin := sameOriginGuard(next)
	mint := func(w http.ResponseWriter, r *http.Request) {
		cookie, err := gate.mintSessionCookie(r)
		if err != nil {
			// Log-only: the cookie is a convenience re-mint beside an
			// already-authenticated response; a side-effect failure must
			// not convert a working request into a 500.
			log.Printf("operator gate: mint session cookie: %v", err)
			return
		}
		// The gate already forced no-store for this admitted response, so a
		// cache can never retain the Set-Cookie body.
		http.SetCookie(w, cookie)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The session handshake is the one operator route that must stay
		// reachable without a credential: it is how a credential becomes a
		// cookie. It owns its throttling and runs behind its own
		// same-origin checks (registered in the reserved-namespace table).
		// The exemption requires the path to be canonical in both the
		// decoded and escaped views so an encoded-slash look-alike can
		// never ride it.
		if r.URL.Path == "/session" && r.URL.EscapedPath() == "/session" {
			next.ServeHTTP(w, r)
			return
		}
		// Gate on the canonical form of the path, not the literal spelling.
		// The mux cleans duplicate slashes and dot segments itself and answers
		// a non-canonical operator path with a 307 to the clean one before any
		// handler runs, which told an unauthenticated caller the operator route
		// table and skipped the gate and its same-origin check for that hop.
		// Resolve the spelling first, then refuse every non-canonical spelling
		// of an owned namespace outright.
		decoded := strings.ReplaceAll(r.URL.Path, `\`, "/")
		if !gatedPath(decoded) && !gatedPath(stdlibpath.Clean(decoded)) {
			next.ServeHTTP(w, r)
			return
		}
		// An encoded path separator makes a namespace look-alike the mux
		// cannot route to its reserved subtree, so it would be forwarded to
		// inference. The owned namespace is reserved absolutely: refuse it
		// here, at the single gate, before the plane state or any credential
		// is consulted. A raw backslash is caught too, because EscapedPath
		// re-encodes it to %5C. A duplicated separator (leading or interior)
		// or dot segment is refused for the same reason: it is a
		// non-canonical spelling of the same namespace, and the mux would
		// redirect instead of denying - the redirect an unauthenticated caller
		// then follows is exactly what the gate exists to remove.
		if encodedSeparator(r.URL.EscapedPath()) || nonCanonicalPath(decoded) {
			denyOperator(w, http.StatusNotFound, "reserved operator namespace")
			return
		}
		if !gate.armed {
			denyOperator(w, http.StatusForbidden, operatorPlaneDisabledMsg)
			return
		}
		ip := clientIP(r)
		// Every admitted operator response is unretainable, whichever
		// credential admitted it: the gate is the one owner so a handler cannot
		// forget it, and a cookie-authenticated /metrics/query or pprof response
		// is no more cacheable than its Bearer twin.
		w.Header().Set("Cache-Control", "no-store")
		// A verified credential is admission, not a guess: both the session
		// cookie and a correct Bearer pass before the throttle and clear the
		// budget, so a lockout can never lock the operator out of their own
		// dashboard while still throttling every wrong candidate.
		if cookie, err := r.Cookie(sessionCookieName); err == nil && gate.validSession(cookie) {
			// A verified cookie is proof exactly like a Bearer: clear the
			// budget, or the comment above ("both ... clear the budget") is
			// false and an operator holding a valid session keeps counting
			// down a lockout armed by someone else on the same address.
			gate.limiter.success(ip)
			sameOrigin.ServeHTTP(w, r)
			return
		}
		credential, hasBearer := bearerToken(r)
		if hasBearer && gate.valid(credential) {
			gate.limiter.success(ip)
			if !mcpNamespace(r.URL.Path) {
				// The MCP endpoint admits only the Bearer and retains
				// nothing, so minting the 12-hour dashboard session cookie
				// there would leave a full dashboard credential in an MCP
				// client's cookie jar for a response that can never use it.
				// The endpoint's own transport headers are left untouched.
				mint(w, r)
			}
			sameOrigin.ServeHTTP(w, r)
			return
		}
		// The sign-in form survives its own lockout. GET / is the one route
		// that leads back to a credential, and POST /session keeps working
		// through the lockout, so a JSON 429 here hid the only way forward.
		// The page carries the same Retry-After as the JSON denial and says
		// the source is locked, while every other operator route keeps the
		// JSON shape.
		retry, locked := gate.lockoutRetry(ip)
		if locked && !wantsLoginPage(r) {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			denyOperator(w, http.StatusTooManyRequests, lockoutMessage)
			return
		}
		if hasBearer && !locked {
			// Only presented-but-wrong credentials count as brute force.
			// Credential-less requests (an expired session cookie polling in
			// a background tab) are denied without burning the budget: they
			// carry no candidate to guess. A locked-out page read counts
			// nothing either, exactly like the JSON denial it replaces.
			gate.limiter.failure(ip, time.Now())
		}
		if locked {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			serveLoginPage(w, http.StatusTooManyRequests, loginPageLockedNotice(retry))
			return
		}
		if wantsLoginPage(r) {
			serveLoginPage(w, http.StatusUnauthorized, loginPageProtectedNotice)
			return
		}
		gate.denyBearer(w)
	})
}

// wantsLoginPage is the one owner of "this response carries the sign-in form"
// rather than a JSON denial: a GET of the dashboard entry, in its canonical
// spellings.
func wantsLoginPage(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html")
}

// lockoutMessage is the shared JSON lockout denial body.
const lockoutMessage = "too many rejected credentials; try again later"

// operatorPlaneDisabledMsg is the unarmed-plane denial message shared by the
// request gate and the session handshake: both answer 403 with it when no
// operator credential is configured.
const operatorPlaneDisabledMsg = "operator plane is disabled: set " + mcp.ProxyTokenEnv + " to enable it"

// denyOperator is the single denial writer for the operator plane: it owns
// the Cache-Control: no-store and frame-ancestors CSP denial headers and the
// flat JSON error body (adminjson.WriteError), so every denial - method
// gate, unarmed plane, malformed handshake, lockout, missing credential -
// carries the same no-store, unframeable shape. Extra headers stay at the
// call sites that own them: Allow at the method gate, Retry-After inside
// denyLockedOut, WWW-Authenticate inside denyBearer.
func denyOperator(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	adminjson.WriteError(w, status, msg)
}

// denyLockedOut answers a locked-out source IP with the shared lockout
// denial: Retry-After rounded up to whole seconds (never understating the
// remaining window) plus the flat 429 body. ok=false when ip is not locked
// out, so the caller keeps its flow control; both the request gate and the
// session handshake must run this check before counting a failure.
func (g *operatorGate) denyLockedOut(w http.ResponseWriter, ip string) bool {
	retry, locked := g.lockoutRetry(ip)
	if !locked {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	denyOperator(w, http.StatusTooManyRequests, lockoutMessage)
	return true
}

// lockoutRetry is the one owner of the lockout's Retry-After value: whole
// seconds rounded up (never understating the remaining window). It serves both
// denial shapes, the JSON one and the sign-in page notice.
func (g *operatorGate) lockoutRetry(ip string) (int, bool) {
	locked, retryIn := g.limiter.locked(ip, time.Now())
	if !locked {
		return 0, false
	}
	return int(max(retryIn/time.Second, 1)), true
}

// denyBearer answers a non-form request without a valid credential with the
// challenge shape: the Bearer realm header plus the flat 401 body.
func (g *operatorGate) denyBearer(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="millivolt-operator"`)
	denyOperator(w, http.StatusUnauthorized, "operator token required")
}

// cappedBody re-arms one absolute read deadline before every read. The delivery
// layer arms the generic idle bound per read, so a route that owns a tighter
// pre-auth bound must arm its own on every read as well; a single arm before
// the handler would survive only until the first read.
type cappedBody struct {
	body     io.ReadCloser
	rc       *http.ResponseController
	deadline time.Time
}

func (c cappedBody) Read(p []byte) (int, error) {
	// A transport without deadline support leaves the read unbounded here; the
	// handler's initial arm already reported that case.
	_ = c.rc.SetReadDeadline(c.deadline)
	return c.body.Read(p)
}

func (c cappedBody) Close() error { return c.body.Close() }

// handleOperatorSession is POST /session: the one open operator-plane
// route, because it is the handshake that mints the session cookie. It
// accepts the credential as a Bearer header (API clients) or as the single
// "token" form field (the login page works without JavaScript), throttles
// wrong candidates like every other gated request, and never reveals which
// of the two was wrong.
func (g *operatorGate) handleOperatorSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		denyOperator(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !g.armed {
		denyOperator(w, http.StatusForbidden, operatorPlaneDisabledMsg)
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
		deadline := time.Now().Add(sessionBodyReadTimeout)
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("operator session: read deadline: %v", err)
			denyOperator(w, http.StatusInternalServerError, "unsupported connection")
			return
		}
		// The shared body wrapper re-arms the generic idle bound before every
		// read, which would overwrite this one on the first read. Re-arm it per
		// read too, so the handshake's own tighter bound is the one that
		// applies to this pre-auth body instead of a reloadable route-wide one.
		r.Body = http.MaxBytesReader(w, cappedBody{body: r.Body, rc: rc, deadline: deadline}, sessionFormBodyMax)
		if err := r.ParseForm(); err != nil {
			denyOperator(w, http.StatusBadRequest, "invalid form body")
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
		cookie, err := g.mintSessionCookie(r)
		if err != nil {
			// The handshake's whole purpose is the cookie: without it the
			// 303/204 would loop the operator back to the login page with
			// no explanation. The only source is crypto/rand, which cannot
			// fail on this toolchain; this is contract hygiene.
			log.Printf("operator session: mint cookie: %v", err)
			denyOperator(w, http.StatusInternalServerError, "session unavailable")
			return
		}
		http.SetCookie(w, cookie)
		if form {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The lockout check precedes failure counting, matching the gate: the
	// request that crosses the threshold still gets its own denial shape,
	// and only the NEXT one is locked out.
	if g.denyLockedOut(w, ip) {
		return
	}
	if presented {
		g.limiter.failure(ip, time.Now())
	}
	if form {
		// The HTML login-page rejection keeps its own body (the page, not a
		// JSON error) but the same denial headers as denyOperator.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.WriteHeader(http.StatusUnauthorized)
		if presented {
			_, _ = w.Write([]byte(loginPageRejectedHTML))
		} else {
			// Nothing was presented: the rejection notice would be about a
			// token that never existed. The clean page just asks again.
			_, _ = w.Write([]byte(loginPageHTML))
		}
		return
	}
	g.denyBearer(w)
}

// loginPageTemplate is the whole pre-auth surface: self-contained (the gated
// /dash assets are unreachable by design). The form works without JavaScript
// and only forwards the entered value to POST /session. A tiny optional
// script registers the ungated service worker so the sign-in page is
// installable before a session cookie exists. The credential prose is not part
// of the template: loginPageProtectedNotice/loginPageRejectedNotice fill the
// loginPageMarker slot and interpolate mcp.ProxyTokenEnv, the one owner of the
// variable name, so an owner rename moves the page instead of leaving the form
// naming a stale variable.
const loginPageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<meta name="theme-color" content="#100e0b">
<meta name="mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-title" content="millivolt">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<link rel="icon" type="image/svg+xml" href="/favicon.svg">
<link rel="icon" href="/favicon.ico" sizes="any">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<link rel="manifest" href="/manifest.webmanifest">
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
  background: #2a2440; border: 1px solid #3a342b;
  display: grid; place-items: center;
}
h1 { margin: 0; font: 650 15px/1.2 ui-monospace, "SFMono-Regular", Menlo, monospace; letter-spacing: -.01em; }
p { color: #97907e; margin: 0 0 16px; font-size: 13px; }
input {
  width: 100%; font: 13px/1.4 ui-monospace, "SFMono-Regular", Menlo, monospace;
  color: #e8e4de; background: #100e0b; border: 1px solid #2b2620;
  border-radius: 8px; padding: 10px 12px; min-height: 42px;
}
input:focus { border-color: #7aa0ff; outline: none; box-shadow: 0 0 0 3px rgba(122,160,255,.16); }
button {
  font: 650 13px/1.2 inherit; margin-top: 12px; width: 100%; min-height: 42px;
  color: #141210; background: #7aa0ff; border: 1px solid #7aa0ff;
  border-radius: 8px; padding: 10px 12px; cursor: pointer;
}
button:hover { background: #9ab8ff; border-color: #9ab8ff; }
button:focus-visible { outline: 2px solid #7aa0ff; outline-offset: 2px; }
.notice { color: #f4c14d; margin: 0 0 10px; font-size: 13px; }
</style>
</head>
<body>
<form method="post" action="/session">
<div class="brand">
<div class="logo" aria-hidden="true">
<svg width="18" height="18" viewBox="0 0 18 18" fill="none">
<g stroke="#7a72a0" stroke-width="1" opacity=".55" stroke-linecap="round">
<path d="M2 13.5v1.6M5.5 13.5v1.6M9 13.5v1.6M12.5 13.5v1.6M16 13.5v1.6"/>
</g>
<path d="M1.5 12.8H16.5" stroke="#56507a" stroke-width="1" opacity=".5" stroke-linecap="round"/>
<path d="M1.5 9.5 4 9.5 5.6 4.6 8 13.2 10.4 6.8 12.4 9.5 16.5 9.5" stroke="#b18cff" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/>
</svg>
</div>
<h1>millivolt</h1>
</div>
{{login-notice}}
<input type="password" name="token" autocomplete="current-password" autofocus aria-label="Operator token" required>
<button type="submit">Sign in</button>
</form>
<script>
if (window.isSecureContext && navigator.serviceWorker) {
  navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache: 'none'}).catch(function () {});
}
</script>
</body>
</html>
`

// loginPageMarker is the template slot the two notice variants fill.
const loginPageMarker = "{{login-notice}}"

// loginPageProtectedNotice is the page's ask for the credential. It and
// loginPageRejectedNotice interpolate mcp.ProxyTokenEnv, so their prose names
// exactly the variable the boot gate reads, not a hand copy.
var loginPageProtectedNotice = "<p>This dashboard is protected. Enter the " + mcp.ProxyTokenEnv + " value.</p>"

// loginPageRejectedNotice prefixes the same ask with a visible rejection
// notice: the identical silent re-serve read as "nothing happened" and had the
// operator sign in twice without ever seeing why.
var loginPageRejectedNotice = `<p class="notice" role="alert">That token was rejected. Enter the current ` + mcp.ProxyTokenEnv + ` value and try again.</p>
` + loginPageProtectedNotice

// loginPageLockedNotice is the lockout variant of the ask: the form is still
// served and still works, and the notice says how long the lockout lasts so the
// operator does not read the page as a failure of the page itself.
func loginPageLockedNotice(retry int) string {
	return `<p class="notice" role="alert">Too many rejected tokens from this address. The form still works, or try again in ` +
		strconv.Itoa(retry) + ` s.</p>
` + loginPageProtectedNotice
}

// loginPage renders one page variant into the template's notice slot.
func loginPage(notice string) string {
	return strings.Replace(loginPageTemplate, loginPageMarker, notice, 1)
}

// serveLoginPage is the one writer of the sign-in form, so every response that
// carries the form is no-store, unframeable and typed alike. The status stays a
// property of the caller: 401 for the ask, 429 while the source is locked out.
func serveLoginPage(w http.ResponseWriter, status int, notice string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(loginPage(notice)))
}

// loginPageHTML is the clean sign-in page, loginPageRejectedHTML the same page
// with the rejection notice, served when a form POST carried a wrong
// credential. The rejected variant is never carried on a first paint: the
// unauthenticated page cannot reveal that any attempt happened.
var (
	loginPageHTML         = loginPage(loginPageProtectedNotice)
	loginPageRejectedHTML = loginPage(loginPageRejectedNotice)
)

// handleHealthz is GET /healthz: the unauthenticated liveness probe for
// Docker HEALTHCHECK and load balancers. It reports only that the process is
// up and serving HTTP on this listener; storage depth lives on the
// /metrics/prometheus and dashboard surfaces.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Cache-Control", "no-store")
		adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "GET or HEAD only")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
