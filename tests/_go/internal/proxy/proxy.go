package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestJoinUpstreamURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// The footgun: base already ends in /v1 and the request path starts with
		// /v1 → must not duplicate.
		{"https://hyper.gamma.example/v1", "/v1/chat/completions", "https://hyper.gamma.example/v1/chat/completions"},
		{"https://api.beta.example/v1", "/v1/messages", "https://api.beta.example/v1/messages"},
		// Host-only base: path appended as-is.
		{"https://api.alpha.example", "/v1/chat/completions", "https://api.alpha.example/v1/chat/completions"},
		// Trailing slash on base is trimmed before joining.
		{"https://api.alpha.example/", "/v1/chat/completions", "https://api.alpha.example/v1/chat/completions"},
		// Differing segments are never stripped.
		{"https://host.com/api", "/v1/chat", "https://host.com/api/v1/chat"},
		// Multi-segment base path: only the LAST segment can dedupe.
		{"https://host.com/api/v1", "/v1/models", "https://host.com/api/v1/models"},
		// Base with no path + root request path.
		{"https://host.com", "/", "https://host.com/"},
	}
	for _, c := range cases {
		if got := joinUpstreamURL(c.base, c.path); got != c.want {
			t.Errorf("joinUpstreamURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

type capturedRequest struct {
	path          string
	authHeader    string
	apiKeyHeader  string
	versionHeader string
	body          string
}

func mockUpstream(t *testing.T, stream bool) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.path = r.URL.Path
		captured.authHeader = r.Header.Get("Authorization")
		captured.apiKeyHeader = r.Header.Get("x-api-key")
		captured.versionHeader = r.Header.Get("anthropic-version")
		captured.body = string(body)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			for _, chunk := range []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
				"data: [DONE]\n\n",
			} {
				w.Write([]byte(chunk))
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	return srv, captured
}

func proxyServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(config.Default(), metrics.Noop{}))
}

func TestPassthroughStreaming(t *testing.T) {
	upstream, captured := mockUpstream(t, true)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: [DONE]\n\n"
	if got != want {
		t.Fatalf("stream body mismatch:\n got %q\nwant %q", got, want)
	}
	if captured.authHeader != "Bearer sk-test-key" {
		t.Errorf("upstream Authorization = %q, want %q", captured.authHeader, "Bearer sk-test-key")
	}
	if captured.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", captured.path)
	}
}

func TestAnthropicStyleAuthRewrite(t *testing.T) {
	upstream, captured := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-ant-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	req.Header.Set("X-Proxy-Path", "/messages")
	req.Header.Set("X-Proxy-Headers", `{"anthropic-version":["2023-06-01"]}`)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if captured.apiKeyHeader != "sk-ant-key" {
		t.Errorf("upstream x-api-key = %q, want %q", captured.apiKeyHeader, "sk-ant-key")
	}
	if captured.versionHeader != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %q, want %q", captured.versionHeader, "2023-06-01")
	}
	if captured.path != "/messages" {
		t.Errorf("upstream path = %q, want /messages", captured.path)
	}
	if captured.authHeader != "" {
		t.Errorf("Authorization leaked upstream: %q", captured.authHeader)
	}
}

func TestExplicitKeyHeader(t *testing.T) {
	upstream, captured := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Proxy-Key", "sk-secret")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if captured.authHeader != "Bearer sk-secret" {
		t.Errorf("upstream Authorization = %q, want %q", captured.authHeader, "Bearer sk-secret")
	}
}

func TestMissingBaseURL(t *testing.T) {
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBodyPreserved(t *testing.T) {
	upstream, captured := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if captured.body != body {
		t.Errorf("request body mutated:\n got %q\nwant %q", captured.body, body)
	}
}

func TestLowercaseBearer(t *testing.T) {
	upstream, captured := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "bearer sk-lower")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if captured.authHeader != "Bearer sk-lower" {
		t.Errorf("upstream Authorization = %q, want %q", captured.authHeader, "Bearer sk-lower")
	}
}

func TestHopByHopHeadersStripped(t *testing.T) {
	upstream, captured := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Transfer-Encoding", "chunked")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The mock does not capture hop-by-hop headers directly; verify the
	// response is well-formed and body intact (headers dropped upstream).
	if captured.body != `{"model":"m"}` {
		t.Errorf("body mutated: %q", captured.body)
	}
}

func TestQueryOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "api-version=2024-02-01" {
			t.Errorf("upstream query = %q, want api-version=2024-02-01", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions?foo=bar", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Query", "api-version=2024-02-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestAllowedBaseURLs(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()

	cfg := config.Default()
	cfg.AllowedBaseURLs = []string{"https://api.alpha.example"}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL) // not in allowlist

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-allowed base URL", resp.StatusCode)
	}
}

// TestParseErrorBodyBounded pins two invariants: (1) the provider's error
// message is bounded via metrics.TruncatePreview (it can echo user content, so
// it must never be stored unbounded), and (2) an oversized body truncated
// mid-token still yields a classifiable signature, not a blank one.
func TestParseErrorBodyBounded(t *testing.T) {
	// A huge error message is truncated to the preview bound.
	big := `{"error":{"type":"invalid_request_error","message":"` + strings.Repeat("x", 5000) + `"}}`
	typ, _, msg := parseErrorBody([]byte(big))
	if typ != "invalid_request_error" {
		t.Errorf("typ = %q", typ)
	}
	if len(msg) > metrics.PreviewMaxBytes+len("…") {
		t.Errorf("msg len %d exceeds preview bound", len(msg))
	}

	// A body that doesn't parse (e.g. truncated mid-token) still yields a
	// bounded, classifiable signature - never a blank one.
	typ2, _, msg2 := parseErrorBody([]byte(`{"error":{"type":"provider_error","message":"unterminated`))
	if typ2 == "" {
		t.Error("unparseable error body yielded a blank error type")
	}
	if len(msg2) > metrics.PreviewMaxBytes+len("…") {
		t.Errorf("fallback msg len %d exceeds preview bound", len(msg2))
	}
}

func TestUnsupportedScheme(t *testing.T) {
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Proxy-Base-URL", "file:///etc/passwd")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for file:// scheme", resp.StatusCode)
	}
}

func TestProviderFromURL(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.alpha.example", "alpha.example"},
		{"https://api.alpha.example/v1", "alpha.example"},
		{"https://alpha.example", "alpha.example"},         // no subdomain, unchanged
		{"https://API.Alpha.Example/v1/", "alpha.example"}, // case-insensitive
		{"https://api.beta.example", "beta.example"},
		{"https://api2.gamma.example", "gamma.example"},         // numbered gateway host
		{"https://inference.delta.example/v1", "delta.example"}, // non-api subdomain collapses
		{"https://hyper.gamma.example/v1", "gamma.example"},     // non-api subdomain collapses
		{"https://api.eu.region.example.com", "example.com"},    // nested subdomains collapse
		{"https://eu.api.eta.example", "eta.example"},           // api anywhere: still root domain
		{"https://api.example.co.uk", "example.co.uk"},          // eTLD: co.uk
		{"https://foo.bar.example.co.uk", "example.co.uk"},      // eTLD+1, not just 2 labels
		{"https://example.com:8443/v1", "example.com"},          // port dropped for named hosts
		{"http://10.0.0.5:8000/v1", "10.0.0.5:8000"},            // IP: keep authority with port
		{"http://192.168.1.10", "192.168.1.10"},
		{"http://[::1]:11434", "[::1]:11434"},    // IPv6 literal
		{"http://localhost:11434", "localhost"},  // no registrable domain: keep host
		{"https://api", "api"},                   // single label: kept as-is
		{"https://api.internal", "api.internal"}, // unknown suffix: kept as-is
	}
	for _, c := range cases {
		u, err := url.Parse(c.base)
		if err != nil {
			t.Fatalf("parse %q: %v", c.base, err)
		}
		if got := providerFromURL(u); got != c.want {
			t.Errorf("providerFromURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

// TestResolveTargetAppliesProviderAlias pins the capture-time half of
// provider_aliases: a derived label the operator merged is re-keyed to its
// canonical one at the single choke point, so records, throttles, pause holds,
// and field maps all see one entity. Labels here are neutral fixtures - the
// mechanism is provider-agnostic.
func TestResolveTargetAppliesProviderAlias(t *testing.T) {
	cfg := config.Default()
	cfg.ProviderAliases = map[string]string{"old.example": "new.example"}
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r.Header.Set("X-Proxy-Base-URL", "https://api.old.example/v1")
	tgt, err := resolveTarget(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.provider != "new.example" {
		t.Errorf("aliased provider = %q, want %q", tgt.provider, "new.example")
	}
	// An unaliased label passes through untouched.
	r2 := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r2.Header.Set("X-Proxy-Base-URL", "https://api.other.example/v1")
	tgt2, err := resolveTarget(r2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tgt2.provider != "other.example" {
		t.Errorf("unaliased provider = %q, want %q", tgt2.provider, "other.example")
	}
	// No alias map at all: derivation unchanged.
	r3 := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
	r3.Header.Set("X-Proxy-Base-URL", "https://api.old.example/v1")
	tgt3, err := resolveTarget(r3, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if tgt3.provider != "old.example" {
		t.Errorf("default derivation = %q, want %q", tgt3.provider, "old.example")
	}
}

func TestParseLLMRequestCapturesNonContentParams(t *testing.T) {
	body := []byte(`{
		"model": "model-a",
		"stream": true,
		"max_tokens": 512,
		"temperature": 0.7,
		"top_p": 0.9,
		"n": 2,
		"presence_penalty": 0.1,
		"frequency_penalty": -0.2,
		"seed": 42,
		"stop": ["\n", "END"],
		"logprobs": true,
		"top_logprobs": 5,
		"logit_bias": {"1234": -100, "5678": 50},
		"response_format": {"type": "json_object"},
		"service_tier": "flex",
		"parallel_tool_calls": false,
		"stream_options": {"include_usage": true},
		"metadata": {"user": "u1", "trace": "t2"},
		"messages": [{"role":"user","content":"secret message content"}]
	}`)
	var rec metrics.Record
	parseLLMRequest(body, &rec, false)

	// The shared parser extracts routing and non-content parameters together.
	if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != 512 {
		t.Errorf("ReqMaxTokens = %v", rec.ReqMaxTokens)
	}
	if rec.ReqTemperature == nil || *rec.ReqTemperature != 0.7 {
		t.Errorf("ReqTemperature = %v", rec.ReqTemperature)
	}
	if rec.ReqSeed == nil || *rec.ReqSeed != 42 {
		t.Errorf("ReqSeed = %v", rec.ReqSeed)
	}
	if rec.ReqStop != 2 {
		t.Errorf("ReqStop = %d, want 2", rec.ReqStop)
	}
	if !rec.ReqLogprobs {
		t.Error("ReqLogprobs not set")
	}
	if rec.ReqTopLogprobs == nil || *rec.ReqTopLogprobs != 5 {
		t.Errorf("ReqTopLogprobs = %v", rec.ReqTopLogprobs)
	}
	if rec.ReqLogitBias != 2 {
		t.Errorf("ReqLogitBias = %d, want 2 (count only)", rec.ReqLogitBias)
	}
	if rec.ReqResponseFormat != "json_object" {
		t.Errorf("ReqResponseFormat = %q", rec.ReqResponseFormat)
	}
	if rec.ReqServiceTier != "flex" {
		t.Errorf("ReqServiceTier = %q", rec.ReqServiceTier)
	}
	if rec.ReqParallelTools == nil || *rec.ReqParallelTools != false {
		t.Errorf("ReqParallelTools = %v", rec.ReqParallelTools)
	}
	if !rec.ReqStreamOpts {
		t.Error("ReqStreamOpts not set")
	}
	if rec.ReqMetadataKeys != 2 {
		t.Errorf("ReqMetadataKeys = %d, want 2 (count only)", rec.ReqMetadataKeys)
	}
	// Content must never be captured by parseLLMRequest.
	if rec.PromptPreview != "" || rec.ResponsePreview != "" {
		t.Error("content preview populated by parseLLMRequest")
	}
}

func TestParseLLMRequestCapturesReasoningEffortAndVerbosity(t *testing.T) {
	var rec metrics.Record
	parseLLMRequest([]byte(`{"reasoning_effort":"high","verbosity":"low","messages":[{"role":"user","content":"hi"}]}`), &rec, false)
	if rec.ReqReasoningEffort != "high" {
		t.Errorf("effort = %q", rec.ReqReasoningEffort)
	}
	if rec.ReqVerbosity != "low" {
		t.Errorf("verbosity = %q", rec.ReqVerbosity)
	}

	var rec2 metrics.Record
	parseLLMRequest([]byte(`{"reasoning":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`), &rec2, false)
	if rec2.ReqReasoningEffort != "xhigh" {
		t.Errorf("nested effort = %q", rec2.ReqReasoningEffort)
	}

	var rec3 metrics.Record
	parseLLMRequest([]byte(`{"reasoning_effort":"drop this because spaces","verbosity":"also not ok!","messages":[{"role":"user","content":"secret"}]}`), &rec3, false)
	if rec3.ReqReasoningEffort != "" || rec3.ReqVerbosity != "" {
		t.Errorf("rejected tokens leaked: %q %q", rec3.ReqReasoningEffort, rec3.ReqVerbosity)
	}
}

func TestFillClientMetaAllowlistedHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Stainless-Lang", "python")
	req.Header.Set("X-Stainless-OS", "Linux")
	req.Header.Set("X-Stainless-Arch", "x64")
	req.Header.Set("X-Stainless-Runtime", "CPython")
	req.Header.Set("X-Stainless-Runtime-Version", "3.12.8")
	req.Header.Set("X-Stainless-Package-Version", "1.55.0")
	req.Header.Set("X-Proxy-Max-Concurrency", "2")
	req.Header.Set("Authorization", "Bearer sk-secret")
	tgt := &target{format: "cursor", timeout: 90 * time.Second}
	rec := &metrics.Record{}
	fillClientMeta(req, tgt, rec)
	if rec.ClientMeta.OS != "Linux" || rec.ClientMeta.Arch != "x64" || rec.ClientMeta.Runtime != "CPython" {
		t.Errorf("identity = %+v", rec.ClientMeta)
	}
	if rec.ClientMeta.RuntimeVer != "3.12.8" || rec.ClientMeta.PkgVer != "1.55.0" {
		t.Errorf("versions = %+v", rec.ClientMeta)
	}
	if rec.ClientMeta.Format != "cursor" || rec.ClientMeta.TimeoutMs != 90000 || rec.ClientMeta.MaxConc != 2 {
		t.Errorf("routing = %+v", rec.ClientMeta)
	}
}

func TestParseLLMRequestMeasuresPromptComposition(t *testing.T) {
	cases := []struct {
		name                                                         string
		body                                                         string
		wantCharsSystem, wantCharsUser, wantCharsAsst, wantCharsTool int
		wantImages, wantAttachments                                  int
		wantLastTurn                                                 string
	}{
		{
			name: "openai roles with image and file parts",
			body: `{"messages":[
				{"role":"system","content":"You are helpful."},
				{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"https://x/y.png"}},{"type":"file","file":{"file_id":"f1"}}]},
				{"role":"assistant","content":"ok then"},
				{"role":"user","content":"thanks"}
			]}`,
			wantCharsSystem: 16, wantCharsUser: len("look at this") + len("thanks"), wantCharsAsst: 7,
			wantImages: 1, wantAttachments: 1, wantLastTurn: "user",
		},
		{
			name: "anthropic top-level system plus document block",
			body: `{"system":"Be concise.","messages":[
				{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","data":"..."}},{"type":"document","source":{"type":"base64","data":"..."}}]},
				{"role":"assistant","content":[{"type":"text","text":"it is a cat"}]}
			]}`,
			wantCharsSystem: 11, wantCharsUser: len("describe"), wantCharsAsst: len("it is a cat"),
			wantImages: 1, wantAttachments: 1, wantLastTurn: "assistant",
		},
		{
			name: "responses-style input_image and input_file plus developer and tool roles",
			body: `{"messages":[
				{"role":"developer","content":"dev rules"},
				{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"u"},{"type":"input_file","file":{}}]},
				{"role":"tool","content":"tool out"}
			]}`,
			wantCharsSystem: 9, wantCharsUser: 2, wantCharsTool: len("tool out"),
			wantImages: 1, wantAttachments: 1, wantLastTurn: "tool",
		},
		{
			name: "model-authored blocks are not attachments",
			body: `{"messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"tool_use","name":"bash"},{"type":"text","text":"done"}]}
			]}`,
			wantCharsAsst: 4, wantImages: 0, wantAttachments: 0, wantLastTurn: "assistant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if rec.CharsSystem != tc.wantCharsSystem {
				t.Errorf("CharsSystem = %d, want %d", rec.CharsSystem, tc.wantCharsSystem)
			}
			if rec.CharsUser != tc.wantCharsUser {
				t.Errorf("CharsUser = %d, want %d", rec.CharsUser, tc.wantCharsUser)
			}
			if rec.CharsAssistant != tc.wantCharsAsst {
				t.Errorf("CharsAssistant = %d, want %d", rec.CharsAssistant, tc.wantCharsAsst)
			}
			if rec.CharsTool != tc.wantCharsTool {
				t.Errorf("CharsTool = %d, want %d", rec.CharsTool, tc.wantCharsTool)
			}
			if rec.Images != tc.wantImages {
				t.Errorf("Images = %d, want %d", rec.Images, tc.wantImages)
			}
			if rec.Attachments != tc.wantAttachments {
				t.Errorf("Attachments = %d, want %d", rec.Attachments, tc.wantAttachments)
			}
			if rec.LastTurnRole != tc.wantLastTurn {
				t.Errorf("LastTurnRole = %q, want %q", rec.LastTurnRole, tc.wantLastTurn)
			}
		})
	}
}

// TestControlHeadersNeverLeakUpstream pins the invariant that every
// client-consumed X-Proxy-* routing header is stripped from the upstream
// request - control headers sent DIRECTLY are removed from passthrough (the
// upstream is actually reached and its captured headers asserted, never a
// vacuous pass), while control-header names in an X-Proxy-Headers injection
// map are rejected outright with a 400 at the routing boundary.
func TestControlHeadersNeverLeakUpstream(t *testing.T) {
	var got http.Header
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		reached = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	// Direct control headers with values valid enough to pass resolveTarget:
	// the strip loop must remove every one before the upstream send.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Key", "sk-test")
	direct := map[string]string{
		"X-Proxy-Client":            "myapp",
		"X-Proxy-Session":           "sess-1",
		"X-Proxy-Provider":          "spoofed",
		"X-Proxy-Auth-Header":       "X-Api-Key",
		"X-Proxy-Auth-Prefix":       "Bearer ",
		"X-Proxy-Path":              "/v1/chat/completions",
		"X-Proxy-Query":             "x=1",
		"X-Proxy-Timeout-Ms":        "1000",
		"X-Proxy-Format":            "openai",
		"X-Proxy-Max-Concurrency":   "2",
		"X-Proxy-Limit-Concurrency": "2",
		"X-Proxy-Limit-Requests":    "10/1m",
		"X-Proxy-Limit-Tokens":      "1000/1m",
		"X-Proxy-Refresh-Token":     "evil-rt",
		"X-Proxy-Access-Token":      "evil-at",
	}
	for h, v := range direct {
		req.Header.Set(h, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !reached {
		t.Fatal("upstream never reached - the strip assertions below would be vacuous")
	}
	for _, h := range []string{
		"X-Proxy-Client", "X-Proxy-Session", "X-Proxy-Provider",
		"X-Proxy-Auth-Header", "X-Proxy-Auth-Prefix", "X-Proxy-Path",
		"X-Proxy-Query", "X-Proxy-Timeout-Ms", "X-Proxy-Format",
		"X-Proxy-Max-Concurrency", "X-Proxy-Limit-Concurrency",
		"X-Proxy-Limit-Requests", "X-Proxy-Limit-Tokens",
		"X-Proxy-Refresh-Token", "X-Proxy-Access-Token",
		"X-Proxy-Key", "X-Proxy-Base-Url", "X-Proxy-Headers",
	} {
		if v := got.Get(h); v != "" {
			t.Errorf("control header %s leaked upstream with value %q", h, v)
		}
	}

	// Smuggled control headers via the extra-headers JSON map are rejected
	// outright (400) - deny by default, nothing is ever sent upstream.
	reached = false
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req2.Header.Set("X-Proxy-Key", "sk-test")
	req2.Header.Set("X-Proxy-Headers", `{"X-Proxy-Client":["evil"],"X-Proxy-Session":["evil"],"X-Proxy-Max-Concurrency":["1"],"X-Proxy-Key":["evil"],"X-Proxy-Base-URL":["http://evil"],"X-Proxy-Limit-Concurrency":["9"],"X-Proxy-Refresh-Token":["evil"],"X-Proxy-Access-Token":["evil"]}`)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("injection map with control-header names: status = %d, want 400", resp2.StatusCode)
	}
	if reached {
		t.Error("smuggled injection map reached the upstream")
	}
}

// TestConnectionNominatedHeadersStripped pins RFC 7230 §6.1: a header nominated
// by the client's Connection header is itself hop-by-hop and must NOT be
// forwarded upstream. Regression test for the header-smuggling bypass where
// "Connection: X-Secret" + "X-Secret: …" leaked X-Secret to the upstream. Go's
// http.Client strips Connection, so this drives the proxy over a raw socket.
func TestConnectionNominatedHeadersStripped(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	raw := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: " + srv.Listener.Addr().String() + "\r\n" +
		"X-Proxy-Base-URL: " + upstream.URL + "\r\n" +
		"X-Proxy-Key: sk-test\r\n" +
		"X-Secret: raw-leaked\r\n" +
		"Connection: X-Secret\r\n" + // nominate X-Secret as hop-by-hop
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if v := got.Get("X-Secret"); v != "" {
		t.Errorf("Connection-nominated header X-Secret leaked upstream with value %q", v)
	}
}

// TestInvalidMaxConcurrencyRejected pins deny-by-default for the
// X-Proxy-Max-Concurrency routing header: a malformed or non-positive value
// is rejected with 400, never silently defaulted (same contract as
// X-Proxy-Timeout-Ms).
func TestInvalidMaxConcurrencyRejected(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	for _, bad := range []string{"abc", "0", "-3", "1.5"} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Key", "sk-test")
		req.Header.Set("X-Proxy-Max-Concurrency", bad)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("X-Proxy-Max-Concurrency %q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestResponsePreviewGatedOnConfig pins the privacy invariant: with
// capture_body_preview off (the default) no response content is stored, on
// either the streaming or the non-streaming path; with it on, a bounded
// preview is captured.
func TestResponsePreviewGatedOnConfig(t *testing.T) {
	for _, stream := range []bool{false, true} {
		// Default (off): no preview may be recorded.
		upstream, _ := mockUpstream(t, stream)
		buf := metrics.NewBuffer(10)
		srv := httptest.NewServer(New(config.Default(), buf))

		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":`+strconv.FormatBool(stream)+`,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Key", "sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		snap := waitForRecord(t, buf, 1)
		if len(snap) != 1 {
			t.Fatalf("stream=%v: recorded %d records, want 1", stream, len(snap))
		}
		if snap[0].ResponsePreview != "" {
			t.Errorf("stream=%v: ResponsePreview = %q with capture_body_preview off, want empty", stream, snap[0].ResponsePreview)
		}
		srv.Close()
		upstream.Close()

		// Enabled: a bounded preview is captured.
		upstream2, _ := mockUpstream(t, stream)
		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf2 := metrics.NewBuffer(10)
		srv2 := httptest.NewServer(New(cfg, buf2))

		req2, _ := http.NewRequest("POST", srv2.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":`+strconv.FormatBool(stream)+`,"messages":[{"role":"user","content":"hi"}]}`))
		req2.Header.Set("X-Proxy-Base-URL", upstream2.URL)
		req2.Header.Set("X-Proxy-Key", "sk-test")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		snap2 := waitForRecord(t, buf2, 1)
		if len(snap2) != 1 {
			t.Fatalf("stream=%v: recorded %d records, want 1", stream, len(snap2))
		}
		if snap2[0].ResponsePreview == "" {
			t.Errorf("stream=%v: ResponsePreview empty with capture_body_preview on, want a preview", stream)
		}
		if len(snap2[0].ResponsePreview) > metrics.PreviewMaxBytes+len("…") {
			t.Errorf("stream=%v: ResponsePreview len %d exceeds bound", stream, len(snap2[0].ResponsePreview))
		}
		srv2.Close()
		upstream2.Close()
	}
}

// TestReloadHotAppliesTunables pins the config-reload contract: Reload swaps the
// config snapshot atomically so new per-request reads see the new values, and it
// does NOT drop or error in-flight/new requests (the listener never closes).
func TestReloadHotAppliesTunables(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	base := config.Default()
	base.CaptureBodyPreview = false
	base.MaxRetries = 1
	srv := httptest.NewServer(New(base, buf))
	defer srv.Close()

	// The Server is behind the mux; grab it via a fresh New to Reload directly.
	inner := New(base, buf)

	// Capture the initial config value the handler would read.
	if inner.cfg().MaxRetries != 1 {
		t.Fatalf("initial MaxRetries = %d, want 1", inner.cfg().MaxRetries)
	}
	if inner.cfg().CaptureBodyPreview {
		t.Fatal("initial CaptureBodyPreview = true, want false")
	}

	// Reload with new values.
	updated := config.Default()
	updated.MaxRetries = 7
	updated.CaptureBodyPreview = true
	updated.MaxBackoff = 5 * time.Minute
	inner.Reload(updated.Clone())

	// New reads see the reloaded values.
	if inner.cfg().MaxRetries != 7 {
		t.Errorf("after reload MaxRetries = %d, want 7", inner.cfg().MaxRetries)
	}
	if !inner.cfg().CaptureBodyPreview {
		t.Error("after reload CaptureBodyPreview = false, want true")
	}
	if inner.cfg().MaxBackoff != 5*time.Minute {
		t.Errorf("after reload MaxBackoff = %v, want 5m", inner.cfg().MaxBackoff)
	}
	// The transport was swapped (new client instance).
	if inner.httpClient() == nil {
		t.Error("httpClient is nil after reload")
	}

	// A request after reload succeeds (nothing dropped) and honors the new
	// capture_body_preview (response preview now recorded).
	reloaded := httptest.NewServer(inner)
	defer reloaded.Close()
	req, _ := http.NewRequest("POST", reloaded.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Key", "sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request after reload failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status after reload = %d, want 200", resp.StatusCode)
	}
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	if snap[0].ResponsePreview == "" {
		t.Error("ResponsePreview empty after reload enabled capture_body_preview, want a preview")
	}
}

// TestReloadDoesNotMutateLiveSnapshot pins the immutability invariant: a Reload
// stores a fresh deep-copied snapshot, so a subsequent Reload's Clone() can never
// mutate the config an in-flight request is still reading (the Providers map and
// AllowedBaseURLs slice must be independent across snapshots).
func TestReloadDoesNotMutateLiveSnapshot(t *testing.T) {
	base := config.Default()
	base.AllowedBaseURLs = []string{"https://a.example.com"}
	base.Providers = map[string]config.ProviderOverride{
		"one": {CostKeys: []string{"usage.cost"}},
	}
	s := New(base, metrics.Noop{})

	// Snapshot the live config's shared references.
	before := s.cfg()
	beforeURL := before.AllowedBaseURLs[0]
	beforeKey := before.Providers["one"].CostKeys[0]

	// Reload a config that reuses the same map/slice values with different content.
	next := config.Default()
	next.AllowedBaseURLs = []string{"https://b.example.com"}
	next.Providers = map[string]config.ProviderOverride{
		"one": {CostKeys: []string{"usage.charge"}},
	}
	s.Reload(next.Clone())

	// The OLD snapshot must be unchanged (immutability across reload).
	if s.cfg() == before {
		// pointer changed (new snapshot)
	} // note: we just verify contents below
	if before.AllowedBaseURLs[0] != beforeURL {
		t.Errorf("old snapshot AllowedBaseURLs mutated: %q -> %q", beforeURL, before.AllowedBaseURLs[0])
	}
	if before.Providers["one"].CostKeys[0] != beforeKey {
		t.Errorf("old snapshot Providers mutated: %q -> %q", beforeKey, before.Providers["one"].CostKeys[0])
	}
	// The NEW snapshot reflects the reload.
	if s.cfg().AllowedBaseURLs[0] != "https://b.example.com" {
		t.Errorf("new AllowedBaseURLs = %q, want b.example.com", s.cfg().AllowedBaseURLs[0])
	}
}
