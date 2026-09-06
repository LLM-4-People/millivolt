package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Stateless per-request access-token refresh.
//
// The proxy holds no credentials, no token cache, and no session state: a
// client that wants expired-token healing sends its own refresh token per
// request in the X-Proxy-Refresh-Token routing header. When the presented
// access token is a JWT whose `exp` has passed (or is about to), the proxy
// exchanges that refresh token at the provider's refresh endpoint. Inference
// always uses handback: HTTP 401 with code "token_expired", the fresh access
// token in X-Proxy-Access-Token and X-Proxy-Refresh-Token when returned.
// No inference is sent. The client adopts the returned credentials and retries
// so subsequent requests do not keep re-exchanging the expired access token.
// Model discovery instead uses the refreshed key transparently and returns no
// token headers, preserving its separate constructed-list contract.
//
// Everything is derived from the request itself: the decision reads the exp
// claim out of the presented token, and the only credential involved is the
// one the client just sent. Providers without a known refresh mechanism, keys
// that are not JWTs (plain API keys), absent refresh tokens, and a disabled
// config knob all pass through completely untouched (deny by default). A
// failed exchange is fail-open: the original request is proxied unchanged and
// the provider's own 401 reaches the client - the proxy never invents an
// error and never blocks a request over metrics-grade extras.
const (
	// jwtRefreshMargin refreshes before the exp claim to reduce early-request
	// expiry risk; it cannot guarantee validity throughout a long generation.
	// Internal safety margin, not user-tunable.
	jwtRefreshMargin = 60 * time.Second
	// tokenRespMax bounds how much of a token-endpoint response is read.
	// Safety guardrail, not user-tunable.
	tokenRespMax = 1 << 20
	// tokenHTTPTimeout bounds one token-endpoint exchange (a small JSON
	// request/response, never an LLM stream). Internal guardrail, not
	// user-tunable.
	tokenHTTPTimeout = 15 * time.Second
)

// xaiOAuthClientID is xAI's OAuth client for the official grok CLI device
// flow (scopes include grok-cli:access api:access). scripts/grok-login.sh -
// the END USER's first-time interactive login, which the proxy never invokes -
// mints tokens for the SAME registration, so the two must stay identical:
// pinned by TestAutoRefreshClientIDMatchesLoginScript.
const xaiOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"

// tokenHTTPClient calls only provider token endpoints (small JSON replies,
// never an LLM stream), so it shares nothing with the upstream pool.
// Test hook: tests repoint it at an httptest server.
var tokenHTTPClient = &http.Client{Timeout: tokenHTTPTimeout, CheckRedirect: preserveUpstreamRedirect}

// xaiTokenEndpoint is xAI's OAuth token endpoint - refresh lives on the auth
// host, not under the API base URL. Test hook for mock endpoints.
var xaiTokenEndpoint = "https://auth.x.ai/oauth2/token"

// refreshedTokens is the outcome of a successful refresh exchange. refresh is
// empty when the provider did not return (a new) refresh token.
type refreshedTokens struct {
	access  string
	refresh string
}

// jwtClaims carries the only claim the refresh decision reads. The proxy
// never verifies the signature - the upstream validates the token; exp only
// decides whether a refresh is worth attempting.
type jwtClaims struct {
	Exp int64 `json:"exp"`
}

// parseJWTClaims decodes the payload segment of a JWT-shaped token. Anything
// that is not a well-formed three-segment token with a decodable payload
// yields ok=false.
func parseJWTClaims(token string) (jwtClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return jwtClaims{}, false
	}
	p := parts[1]
	if pad := len(p) % 4; pad != 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return jwtClaims{}, false
	}
	var c jwtClaims
	if json.Unmarshal(raw, &c) != nil {
		return jwtClaims{}, false
	}
	return c, true
}

// jwtExpiring reports whether the token is a JWT whose exp has passed or
// falls within the refresh margin. Non-JWT keys (xai-…, sk-…) are never
// "expiring" and never touched.
func jwtExpiring(token string, now time.Time) bool {
	c, ok := parseJWTClaims(token)
	if !ok || c.Exp == 0 {
		return false
	}
	return now.Add(jwtRefreshMargin).After(time.Unix(c.Exp, 0))
}

// refreshAccessToken exchanges the client's refresh token with the provider.
// The provider label is derived from the base URL (never client-supplied), so
// the switch is on the same identity the dashboard groups by; unknown
// providers are rejected here and the caller passes the original key through.
func refreshAccessToken(ctx context.Context, t *target, refreshToken string) (refreshedTokens, error) {
	switch t.provider {
	case "x.ai":
		return refreshXAI(ctx, refreshToken)
	case "cursor.sh":
		return refreshCursor(ctx, t.baseURL, refreshToken)
	default:
		return refreshedTokens{}, fmt.Errorf("no token refresh mechanism for provider %q", t.provider)
	}
}

// exchangeToken POSTs a prepared token-endpoint request and decodes the
// response through pick. Non-200, oversize, unparsable, and access-less
// responses are all "exchange failed" - never partial success.
func exchangeToken(req *http.Request, pick func([]byte) (access, refresh string, err error)) (refreshedTokens, error) {
	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return refreshedTokens{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenRespMax+1))
	if err != nil {
		return refreshedTokens{}, err
	}
	if len(body) > tokenRespMax {
		return refreshedTokens{}, fmt.Errorf("token endpoint response exceeds %d bytes", tokenRespMax)
	}
	if resp.StatusCode != http.StatusOK {
		return refreshedTokens{}, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	access, refresh, err := pick(body)
	if err != nil {
		return refreshedTokens{}, err
	}
	if access == "" {
		return refreshedTokens{}, fmt.Errorf("token response carried no access token")
	}
	return refreshedTokens{access: access, refresh: refresh}, nil
}

// refreshXAI owns the OAuth refresh_token grant against auth.x.ai, not the
// client-facing handback policy shared by all inference refresh mechanisms.
func refreshXAI(ctx context.Context, refreshToken string) (refreshedTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", xaiOAuthClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xaiTokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return refreshedTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return exchangeToken(req, func(body []byte) (string, string, error) {
		var out struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", "", fmt.Errorf("unparsable x.ai token response")
		}
		return out.AccessToken, out.RefreshToken, nil
	})
}

// refreshCursor exchanges the refresh token at Cursor's
// auth/exchange_user_api_key endpoint on the SAME host the client targets -
// the refresh token rides as a Bearer header on an empty JSON body (the same
// exchange Cursor's own CLI login performs; not an OAuth grant flow).
func refreshCursor(ctx context.Context, baseURL, refreshToken string) (refreshedTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/auth/exchange_user_api_key", strings.NewReader("{}"))
	if err != nil {
		return refreshedTokens{}, err
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	req.Header.Set("Content-Type", "application/json")
	return exchangeToken(req, func(body []byte) (string, string, error) {
		var out struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", "", fmt.Errorf("unparsable cursor token response")
		}
		return out.AccessToken, out.RefreshToken, nil
	})
}

// refreshKeyIfExpired returns the access token to send upstream: the
// presented key itself, unless auto-refresh is on, the client supplied a
// refresh token, the presented token is an expiring JWT, and the provider has
// a known refresh mechanism - then the fresh pair. The bool reports that an
// exchange happened (and the fresh tokens should ride back to the client).
// The original key is always returned on any failure: fail-open passthrough.
func (s *Server) refreshKeyIfExpired(r *http.Request, t *target, key string) (string, refreshedTokens, bool) {
	if !s.cfg().AutoTokenRefresh {
		return key, refreshedTokens{}, false
	}
	refreshToken := strings.TrimSpace(r.Header.Get(hdrRefreshToken))
	if refreshToken == "" || !jwtExpiring(key, time.Now()) {
		return key, refreshedTokens{}, false
	}
	ctx := r.Context()
	// The client's own Timeout already bounds the whole exchange; only mirror
	// it into the context when one is actually set (a zero Timeout means "no
	// deadline" - a 0s WithTimeout would expire the exchange before it starts).
	if d := tokenHTTPClient.Timeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	fresh, err := refreshAccessToken(ctx, t, refreshToken)
	if err != nil || fresh.access == "" {
		return key, refreshedTokens{}, false
	}
	return fresh.access, fresh, true
}

// resolveKey is the single inference-refresh policy owner. Any successful
// exchange writes handback and returns ok=false: the caller must stop before
// admission, upstream inference or recording. Otherwise preserve the key.
func (s *Server) resolveKey(w http.ResponseWriter, r *http.Request, t *target, key string) (string, bool) {
	_, fresh, refreshed := s.refreshKeyIfExpired(r, t, key)
	if !refreshed {
		return key, true
	}
	writeTokenHandback(w, fresh)
	return "", false
}

// resolveKeyTransparent preserves model discovery's separate contract for every
// mechanism: use the fresh token without returning token headers or handback.
// On failed or inapplicable refresh, the original key goes upstream.
func (s *Server) resolveKeyTransparent(r *http.Request, t *target, key string) string {
	freshKey, _, refreshed := s.refreshKeyIfExpired(r, t, key)
	if refreshed {
		return freshKey
	}
	return key
}

const (
	// tokenHandbackType/Code classify the handback response for clients: an
	// auth failure whose code tells the client to adopt returned credentials
	// and retry. Not user-tunable.
	tokenHandbackType = "authentication_error"
	tokenHandbackCode = "token_expired"
	// tokenHandbackMsg tells the client exactly what happened and what to do.
	tokenHandbackMsg = "access token expired or expiring: adopt X-Proxy-Access-Token and, " +
		"when present, X-Proxy-Refresh-Token from the response headers, then retry; " +
		"retain the existing refresh token if no replacement is returned"
)

// writeTokenHandback returns refreshed credentials instead of sending inference.
// The 401/code/headers tell the client to adopt them before retrying. Client
// retry policies are outside the proxy's control.
// No metrics record is written: no LLM call happened; the retried request
// (with the swapped key) is the one that lands in the log.
func writeTokenHandback(w http.ResponseWriter, fresh refreshedTokens) {
	w.Header().Set("X-Proxy-Access-Token", fresh.access)
	if fresh.refresh != "" {
		w.Header().Set("X-Proxy-Refresh-Token", fresh.refresh)
	}
	http.Error(w, errJSONCode(tokenHandbackType, tokenHandbackMsg, tokenHandbackCode),
		http.StatusUnauthorized)
}
