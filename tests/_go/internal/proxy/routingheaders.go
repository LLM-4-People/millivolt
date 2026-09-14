package proxy

// Drift fences for the routing-header boundary. The strip-set test pins the
// constants behind isProxyControlHeaders; the grammar test pins the single
// exported RFC 7230 field-value check and the deliberate empty-value split
// between its callers.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestRoutingControlConstantsAreStripped(t *testing.T) {
	// The enumeration of every routing/control header constant. A new
	// constant joins this list; if it is missing from the strip set the
	// test fails.
	for _, h := range []string{
		hdrBaseURL, hdrAuthHeader, hdrAuthPrefix, hdrPath, hdrQuery,
		hdrHeaders, hdrKey, hdrRefreshToken, hdrProvider, hdrAccessToken,
		hdrTimeout, hdrFormat, hdrMaxConcurrency, hdrSession,
		hdrParentSession, hdrClient,
		hdrLimitConcurrency, hdrLimitRequests, hdrLimitTokens,
	} {
		for _, spelling := range []string{h, strings.ToLower(h), strings.ToUpper(h)} {
			if !isProxyControlHeader(spelling) {
				t.Errorf("routing control header %q (%q) is not stripped", h, spelling)
			}
		}
	}
	// Defensive extras without constants stay stripped, with their reasons
	// documented in the strip-set builder.
	for _, h := range []string{"Authorization", "COOKIE"} {
		if !isProxyControlHeader(h) {
			t.Errorf("defensive strip entry %q was lost", h)
		}
	}
	// Everything else keeps passing: the strip must not over-strip client
	// headers (deny by default never widens into a deny-all).
	for _, h := range []string{"X-Custom-Header", "User-Agent", "Accept", "X-Stainless-Lang", "X-Request-Id"} {
		if isProxyControlHeader(h) {
			t.Errorf("unrelated header %q must not be stripped", h)
		}
	}
}

// The RFC 7230 field-value grammar has one shared export
// (config.ValidHeaderValue); the empty-string policy deliberately stays with
// each caller: the auth-prefix parse accepts an explicitly empty prefix
// (raw key with no scheme), the provider-header mapping rejects one (pinned
// by the "empty value" row in the config suite), and invalid control bytes
// never pass anywhere.
func TestHeaderValueGrammarSplit(t *testing.T) {
	if !config.ValidHeaderValue("") {
		t.Error("grammar must accept the empty value: emptiness is caller policy")
	}
	for _, v := range []string{"v", "Bearer ", "tab\tvalue", "caf\xc3\xa9 unicode", "obs-text \xff"} {
		if !config.ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"one\ntwo", "one\rtwo", "nul\x00byte", "ctl\x01byte", "del\x7fbyte"} {
		if config.ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = true, want false", v)
		}
	}
	// resolveTarget keeps the split: an explicit empty X-Proxy-Auth-Prefix
	// parses (raw key with no "Bearer "), an invalid character is rejected
	// at the boundary, and X-Proxy-Headers values share the same grammar.
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://api.example")
	r.Header.Set(hdrAuthPrefix, "")
	if _, err := resolveTarget(r, config.Default()); err != nil {
		t.Fatalf("explicit empty auth prefix must parse: %v", err)
	}
	r = httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://api.example")
	r.Header.Set(hdrAuthPrefix, "bad\nvalue")
	if _, err := resolveTarget(r, config.Default()); err == nil {
		t.Error("newline in auth prefix accepted: want a routing-boundary rejection")
	}
	r = httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://api.example")
	r.Header.Set(hdrHeaders, "{\"X-A\":[\"bad\x01value\"]}")
	if _, err := resolveTarget(r, config.Default()); err == nil {
		t.Error("control byte in X-Proxy-Headers value accepted: want a routing-boundary rejection")
	}
}
