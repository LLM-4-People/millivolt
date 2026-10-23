package proxy

// Drift fences for the routing-header boundary. The strip-set test pins the
// constants behind isProxyControlHeaders; the grammar test pins the single
// exported RFC 7230 field-value check and the deliberate empty-value split
// between its callers.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRoutingControlConstantsAreStripped(t *testing.T) {
	// The enumeration of every routing/control header constant. A new
	// constant joins this list; if it is missing from the strip set the
	// test fails.
	for _, h := range []string{
		hdrBaseURL, hdrAuthHeader, hdrAuthPrefix, hdrPath, hdrQuery,
		hdrHeaders, hdrKey, hdrRefreshToken, hdrProvider, hdrAccessToken,
		hdrTimeout, hdrFormat, hdrInFormat, hdrMaxConcurrency, hdrSession,
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

func TestUnknownPathSameProtocolKeepsPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://proxy/openai/deployments/foo/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://api.example")
	r.Header.Set(hdrInFormat, "openai")
	r.Header.Set(hdrFormat, "openai")
	tgt, err := resolveTarget(r, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if tgt.translate {
		t.Fatalf("same-protocol request was marked translated: %+v", tgt)
	}
	if tgt.path != "" {
		t.Fatalf("unknown same-protocol path was rewritten to %q", tgt.path)
	}
}

func TestUnknownPathTranslatedUsesTargetPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://proxy/openai/deployments/foo/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://api.example")
	r.Header.Set(hdrInFormat, "openai")
	r.Header.Set(hdrFormat, "anthropic")
	tgt, err := resolveTarget(r, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !tgt.translate {
		t.Fatalf("translated request was not marked translated: %+v", tgt)
	}
	if tgt.path != "/v1/messages" {
		t.Fatalf("translated path = %q, want /v1/messages", tgt.path)
	}
}

// TestResponsesHostileMaxOutputTokensRejected proves the hostile bound covers
// the Responses spelling and that mixing cap families fails closed before any
// upstream send.
func TestResponsesHostileMaxOutputTokensRejected(t *testing.T) {
	for name, body := range map[string]string{
		"hostile max_output_tokens": `{"model":"m","input":"hi","max_output_tokens":1000001}`,
		"conflicting spellings":     `{"model":"m","input":"hi","max_tokens":1,"max_output_tokens":1000001}`,
	} {
		t.Run(name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", 200, `{}`)
			s := New(config.Default(), metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			w := proxyRequest(t, s, "/v1/responses", "anthropic", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if up.calls.Load() != 0 {
				t.Fatalf("upstream was called %d times before the cap rejection", up.calls.Load())
			}
			if !strings.Contains(w.Body.String(), "max_tokens") {
				t.Fatalf("rejection does not name a cap field: %s", w.Body.String())
			}
		})
	}
}

// TestInFormatStripFence pins the client-protocol override in the same strip
// set as every other routing control header: it selects the inbound wire and
// must never reach the upstream.
func TestInFormatStripFence(t *testing.T) {
	for _, spelling := range []string{hdrInFormat, "x-proxy-in-format", "X-PROXY-IN-FORMAT"} {
		if !isProxyControlHeader(spelling) {
			t.Fatalf("client-protocol override %q is not stripped", spelling)
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

// TestInvalidRoutingHeaderRejectionWording pins the 400 wire contract for a
// malformed routing header served through ServeHTTP: errInvalidHeader's
// "invalid <header> <value>" wording inside the OpenAI JSON error envelope.
// The sink is the canonical EncodeOpenAIError envelope (message/type plus the
// nullable param/code fields); the wording is the contract SDKs and docs match.
func TestInvalidRoutingHeaderRejectionWording(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	r.Header.Set(hdrBaseURL, "http://127.0.0.1:9")
	r.Header.Set(hdrTimeout, "abc")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body %q is not a JSON error envelope: %v", rec.Body.String(), err)
	}
	if envelope.Error.Message != `invalid X-Proxy-Timeout-Ms "abc"` ||
		envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("body = %s, want invalid_request_error / %q", rec.Body.String(), `invalid X-Proxy-Timeout-Ms "abc"`)
	}
}

// TestRoutingHeaderErrorEchoBounded pins the bounded echo on every
// client-controlled value that reaches a routing rejection. A header-sized
// value (or the HTML escaping of one) must not turn a 400 into a multi-megabyte
// JSON body, while normal malformed values keep their exact wording.
func TestRoutingHeaderErrorEchoBounded(t *testing.T) {
	hostile := strings.Repeat("<", hostileLabelBytes)
	hostileASCII := strings.Repeat("a", hostileLabelBytes)
	// adminjson's own body cap is 1 MiB, so the duplicate-key echo fixture stays
	// under it while still being far larger than the error echo bound.
	hostileHalf := strings.Repeat("<", 400<<10)
	allowed := func() *config.Config {
		cfg := config.Default()
		cfg.AllowedBaseURLs = []string{"https://allowed.example"}
		return cfg
	}
	for _, tc := range []struct {
		name      string
		configure func(*http.Request)
		cfg       *config.Config
	}{
		{"timeout", func(r *http.Request) { r.Header.Set(hdrTimeout, hostile) }, config.Default()},
		{"base userinfo", func(r *http.Request) { r.Header.Set(hdrBaseURL, "https://u:p@"+hostileASCII) }, config.Default()},
		{"base scheme", func(r *http.Request) { r.Header.Set(hdrBaseURL, hostileASCII+"://fixture.example") }, config.Default()},
		{"base allowed list", func(r *http.Request) { r.Header.Set(hdrBaseURL, "http://fixture.example/"+hostileASCII) }, allowed()},
		{"auth header name", func(r *http.Request) { r.Header.Set(hdrAuthHeader, hostile) }, config.Default()},
		{"format", func(r *http.Request) { r.Header.Set(hdrFormat, hostile) }, config.Default()},
		{"injected header name", func(r *http.Request) { r.Header.Set(hdrHeaders, `{"`+hostileHalf+`":["v"]}`) }, config.Default()},
		{"injected duplicate header name", func(r *http.Request) {
			r.Header.Set(hdrHeaders, `{"`+hostileHalf+`":["v"],"`+hostileHalf+`":["v2"]}`)
		}, config.Default()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
			r.Header.Set(hdrBaseURL, "http://fixture.example")
			tc.configure(r)
			_, err := resolveTarget(r, tc.cfg)
			if err == nil {
				t.Fatal("expected a routing rejection")
			}
			msg := err.Error()
			if len(msg) > 1024 {
				t.Fatalf("error message retained %d bytes, want a bounded echo", len(msg))
			}
			if body := errJSON(typeInvalidRequestError, msg); len(body) > 8<<10 {
				t.Fatalf("error body = %d bytes, want <= 8 KiB", len(body))
			}
		})
	}

	for _, tc := range []struct {
		name, header, value, want string
		cfg                       *config.Config
	}{
		{"timeout", hdrTimeout, "nope", `invalid X-Proxy-Timeout-Ms "nope"`, config.Default()},
		{"base parse", hdrBaseURL, "://", `invalid X-Proxy-Base-URL "://"`, config.Default()},
		{"base userinfo", hdrBaseURL, "https://u:p@fixture.example/v1",
			`invalid X-Proxy-Base-URL "https://u:p@fixture.example/v1": userinfo, query, and fragment are not allowed in a base URL`, config.Default()},
		{"scheme", hdrBaseURL, "ftp://fixture.example/v1", `unsupported X-Proxy-Base-URL scheme "ftp"`, config.Default()},
		{"auth header", hdrAuthHeader, "bad header", `invalid X-Proxy-Auth-Header header "bad header"`, config.Default()},
		{"format", hdrFormat, "xml", `unsupported X-Proxy-Format "xml"`, config.Default()},
		{"allowed list", hdrBaseURL, "https://other.example/v1", `base URL "https://other.example/v1" not in allowed list`, allowed()},
		{"injected header name", hdrHeaders, `{"bad name":["v"]}`,
			`invalid X-Proxy-Headers header: "bad name" is not a valid header name (RFC 7230 token)`, config.Default()},
	} {
		t.Run("normal "+tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
			r.Header.Set(hdrBaseURL, "http://fixture.example")
			r.Header.Set(tc.header, tc.value)
			_, err := resolveTarget(r, tc.cfg)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	// The full wire response stays bounded too, including JSON HTML escaping
	// and http.Error's trailing newline.
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	r.Header.Set(hdrBaseURL, "http://127.0.0.1:9")
	r.Header.Set(hdrTimeout, hostile)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest || rec.Body.Len() > 8<<10 {
		t.Fatalf("hostile timeout response = %d/%d bytes, want 400 with a bounded body", rec.Code, rec.Body.Len())
	}
}
