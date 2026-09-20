package proxy

// Scoped request overrides (config request_overrides): the proxy mechanism
// only. The config layer's validation, schema and coercion live in
// tests/_go/internal/config/requestoverrides.go; these rows pin the runtime
// contract - exact-leaf scope matching, one body rewrite covering every
// relay attempt / quality re-send / cursor re-ask, header application last
// in the generic chain and at the cursor native send, the credential safety
// belt, and fail-closed passthrough on undecodable bodies. The feature-off
// byte-passthrough pins live in bounds_regression.go and must stay green
// untouched.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// overrideReply is one scripted upstream response.
type overrideReply struct {
	status int
	body   string
}

// overrideCapture records the raw body bytes and header set of every
// upstream request, in arrival order.
type overrideCapture struct {
	mu     sync.Mutex
	bodies []string
	hdrs   []http.Header
}

func (c *overrideCapture) snapshot() ([]string, []http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bodies := make([]string, len(c.bodies))
	copy(bodies, c.bodies)
	hdrs := make([]http.Header, len(c.hdrs))
	copy(hdrs, c.hdrs)
	return bodies, hdrs
}

// scriptedUpstream answers the n-th request with the n-th scripted reply
// (repeating the last one when the script runs out) and records every
// request's raw body bytes and headers.
func scriptedUpstream(t *testing.T, replies ...overrideReply) (*httptest.Server, *overrideCapture) {
	t.Helper()
	capt := &overrideCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capt.mu.Lock()
		i := len(capt.bodies)
		capt.bodies = append(capt.bodies, string(raw))
		capt.hdrs = append(capt.hdrs, r.Header.Clone())
		capt.mu.Unlock()
		if i >= len(replies) {
			i = len(replies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(replies[i].status)
		_, _ = w.Write([]byte(replies[i].body))
	}))
	return srv, capt
}

// postOverrideChat posts one chat-completions request through the proxy with
// a caller-supplied body and header hook, returning the status and the
// relayed response body.
func postOverrideChat(t *testing.T, srvURL, upstreamURL, reqBody string, extra func(*http.Request)) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Content-Type", "application/json")
	if extra != nil {
		extra(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// overrideGoodCompletion is a healthy non-streaming completion body.
const overrideGoodCompletion = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`

// overrideChatBody is the fixed client request the passthrough rows send.
const overrideChatBody = `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`

// TestRequestOverridesRewriteBodyAcrossRetriesAndResends pins the single
// body seam end to end: a matching rule's max_tokens reaches the upstream
// fixture on EVERY send - the initial relay attempt, the absorbed 503 retry
// and the degenerate-200 quality re-send all reuse the same rewritten
// bytes - and the record re-stamps ReqMaxTokens from the effective body so
// the scheduler reservation and the hostile-cap boundary read the truth.
func TestRequestOverridesRewriteBodyAcrossRetriesAndResends(t *testing.T) {
	tokens := 1234
	upstream, capt := scriptedUpstream(t,
		overrideReply{503, `{"error":{"message":"overloaded","type":"server_error","code":"server_is_overloaded"}}`},
		// Degenerate void: choices present, no answer content, no tools -
		// serveNonStreaming absorbs it and re-sends before any byte reaches
		// the client.
		overrideReply{200, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`},
		overrideReply{200, overrideGoodCompletion},
	)
	defer upstream.Close()
	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Client: "ov-client",
		Body:   &config.OverrideBody{MaxTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, respBody := postOverrideChat(t, srv.URL, upstream.URL, overrideChatBody,
		func(r *http.Request) { r.Header.Set("X-Proxy-Client", "ov-client") })
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, respBody)
	}
	if !strings.Contains(respBody, `"content":"hello"`) {
		t.Fatalf("relayed body %q, want the healthy completion", respBody)
	}

	bodies, _ := capt.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("upstream sends = %d, want 3 (first attempt, absorbed 503 retry, degenerate quality re-send)", len(bodies))
	}
	for i, b := range bodies {
		var got struct {
			MaxTokens int    `json:"max_tokens"`
			Model     string `json:"model"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(b), &got); err != nil {
			t.Fatalf("send %d body is not JSON: %v (%s)", i, err, b)
		}
		if got.MaxTokens != tokens {
			t.Errorf("send %d: upstream max_tokens = %d, want the override value %d", i, got.MaxTokens, tokens)
		}
		if got.Model != "model-a" || len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
			t.Errorf("send %d: unrelated fields not preserved verbatim: %s", i, b)
		}
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != tokens {
		t.Errorf("record ReqMaxTokens = %v, want %d re-stamped from the effective body", rec.ReqMaxTokens, tokens)
	}
	if rec.Retries != 2 {
		t.Errorf("Retries = %d, want 2 (the absorbed 503 and the degenerate re-send)", rec.Retries)
	}
}

// TestRequestOverridesTranslatedAnthropicCarriesThrough pins the translated
// path: the rewrite lands on the OpenAI body BEFORE translateRequest, the
// anthropic translator carries the effective ceiling (max_completion_tokens
// wins its precedence) into the upstream body as max_tokens, and the fresh
// record the translated body re-decodes into carries the same value.
func TestRequestOverridesTranslatedAnthropicCarriesThrough(t *testing.T) {
	tokens := 777
	upstream, capt := scriptedUpstream(t, overrideReply{200,
		`{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`})
	defer upstream.Close()
	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Provider: upstreamLabel(upstream.URL),
		Body:     &config.OverrideBody{MaxCompletionTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, respBody := postOverrideChat(t, srv.URL, upstream.URL,
		`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`,
		func(r *http.Request) {
			r.Header.Set("X-Proxy-Auth-Header", "x-api-key")
			r.Header.Set("X-Proxy-Auth-Prefix", "")
			r.Header.Set("X-Proxy-Path", "/messages")
			r.Header.Set("X-Proxy-Format", "anthropic")
		})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, respBody)
	}
	if !strings.Contains(respBody, `"object":"chat.completion"`) {
		t.Fatalf("response not translated back to OpenAI shape: %s", respBody)
	}

	bodies, _ := capt.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	var got struct {
		MaxTokens interface{} `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatalf("translated body is not JSON: %v (%s)", err, bodies[0])
	}
	if got.MaxTokens != float64(tokens) {
		t.Errorf("translated anthropic max_tokens = %v, want the override value %d", got.MaxTokens, tokens)
	}
	if strings.Contains(bodies[0], "max_completion_tokens") {
		t.Errorf("translated body carries the openai field spelling: %s", bodies[0])
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != tokens {
		t.Errorf("record ReqMaxTokens = %v, want %d re-decoded from the translated body", rec.ReqMaxTokens, tokens)
	}
}

// TestRequestOverridesCursorSkipsBodyAppliesHeaders pins the cursor contract:
// the rule matches on the RECORDED base model id (the dashboard's id, not the
// fused display id the client sent), its header rules ride the native agent.v1
// send (the single send owner shared by fresh runs and re-asks), and its body
// section is skipped - token ceilings have no wire meaning in the run_request,
// so the record keeps the client's own (absent) cap instead of a restamped one.
func TestRequestOverridesCursorSkipsBodyAppliesHeaders(t *testing.T) {
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Lab-Mode"); got != "ov" {
			t.Errorf("X-Lab-Mode = %q, want ov (the override header at the native send)", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cursor-key" {
			t.Errorf("Authorization = %q, want the credential intact at its owner", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/connect+proto" {
			t.Errorf("Content-Type = %q, want the protocol constant", ct)
		}
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, payload := readFrame(t, body)
		if !bytes.Contains(payload, []byte("claude-sonnet-4-5")) {
			t.Errorf("model id not in run_request: %x", payload)
		}
		cursorConnectHandshake(t, w, body, fl)
		textDelta := cmsg(1, cmsg(1, cstr(1, "bonjour")))
		tokenDelta := cmsg(1, cmsg(8, cvint(1, 4)))
		turnEnded := cmsg(1, cmsg(14, nil))
		w.Write(cframe(textDelta))
		w.Write(cframe(tokenDelta))
		w.Write(cframe(turnEnded))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	tokens := 5555
	cfg := cursorTestCfg(upstream.URL)
	cfg.RequestOverrides = []config.RequestOverride{{
		Model:   "claude-sonnet-4-5",
		Headers: map[string]string{"X-Lab-Mode": "ov"},
		Body:    &config.OverrideBody{MaxTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sse, _ := io.ReadAll(resp.Body)
	got := string(sse)
	if !strings.Contains(got, `"content":"bonjour"`) {
		t.Errorf("translated content missing: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one [DONE]: %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.Model != "claude-sonnet-4-5" {
		t.Errorf("record model = %q, want the canonical base id", rec.Model)
	}
	if rec.ReqMaxTokens != nil {
		t.Errorf("ReqMaxTokens = %v, want nil (the cursor body section is skipped; no restamp)", *rec.ReqMaxTokens)
	}
}

// TestRequestOverridesInvalidJSONFailsClosed pins the fail-closed seam: a
// matching body rule on a client body that is not a JSON object must relay
// the ORIGINAL bytes verbatim with exactly one operator-actionable log
// line, never a 400 of the proxy's own and never a silent rewrite.
func TestRequestOverridesInvalidJSONFailsClosed(t *testing.T) {
	tokens := 1234
	upstream, capt := scriptedUpstream(t, overrideReply{200, `{"ok":true}`})
	defer upstream.Close()
	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Client: "ov-client",
		Body:   &config.OverrideBody{MaxTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	// Truncated JSON: parseLLMRequest passes it through (the invalid-JSON
	// passthrough invariant) and the rewrite must fail closed on it too.
	rawBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":`
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)
	status, respBody := postOverrideChat(t, srv.URL, upstream.URL, rawBody,
		func(r *http.Request) { r.Header.Set("X-Proxy-Client", "ov-client") })
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the invalid body relays verbatim)", status)
	}
	if strings.TrimSpace(respBody) != `{"ok":true}` {
		t.Errorf("client body = %q, want the upstream reply", respBody)
	}

	bodies, _ := capt.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	if bodies[0] != rawBody {
		t.Errorf("upstream body mutated by the failed rewrite:\n got %q\nwant %q", bodies[0], rawBody)
	}
	logged := logBuf.String()
	if n := strings.Count(logged, "request overrides:"); n != 1 {
		t.Fatalf("request-overrides log lines = %d, want exactly 1: %q", n, logged)
	}
	for _, want := range []string{"body rewrite skipped", `"ov-client"`, "not a JSON object", "relay"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line omits %q: %q", want, logged)
		}
	}

	recs := waitForRecord(t, buf, 1)
	if recs[0].ReqMaxTokens != nil {
		t.Errorf("ReqMaxTokens = %v, want nil (nothing was restamped)", *recs[0].ReqMaxTokens)
	}
}

// TestRequestOverridesOversizeRewriteFailsClosed pins the size arm of the
// fail-closed contract: a rewritten body that would exceed
// max_request_bytes (here at its validation minimum, with a padded client
// body the added field tips over the cap) must relay the ORIGINAL bytes
// verbatim with the one actionable log line naming the limit.
func TestRequestOverridesOversizeRewriteFailsClosed(t *testing.T) {
	tokens := 1234
	upstream, capt := scriptedUpstream(t, overrideReply{200, `{"ok":true}`})
	defer upstream.Close()
	cfg := config.Default()
	cfg.MaxRequestBytes = config.MaxRequestBytesMin
	cfg.RequestOverrides = []config.RequestOverride{{
		Client: "ov-client",
		Body:   &config.OverrideBody{MaxTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	// Compact JSON at 1010 bytes (under the 1024 cap the upload accepts);
	// adding "max_tokens":1234 plus its separator tips the re-marshaled
	// document over the cap, so the rewrite must fail closed.
	const head = `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"pad":"`
	const tail = `"}`
	rawBody := head + strings.Repeat("A", 1010-len(head)-len(tail)) + tail
	if len(rawBody) != 1010 {
		t.Fatalf("fixture body = %d bytes, want 1010", len(rawBody))
	}
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)
	status, respBody := postOverrideChat(t, srv.URL, upstream.URL, rawBody,
		func(r *http.Request) { r.Header.Set("X-Proxy-Client", "ov-client") })
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the oversized rewrite relays the original verbatim)", status)
	}
	if strings.TrimSpace(respBody) != `{"ok":true}` {
		t.Errorf("client body = %q, want the upstream reply", respBody)
	}

	bodies, _ := capt.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	if bodies[0] != rawBody {
		t.Errorf("upstream body mutated by the failed rewrite:\n got %q\nwant %q", bodies[0], rawBody)
	}
	logged := logBuf.String()
	if n := strings.Count(logged, "request overrides:"); n != 1 {
		t.Fatalf("request-overrides log lines = %d, want exactly 1: %q", n, logged)
	}
	if !strings.Contains(logged, "max_request_bytes") {
		t.Errorf("log line must name the byte limit to be actionable: %q", logged)
	}

	recs := waitForRecord(t, buf, 1)
	if recs[0].ReqMaxTokens != nil {
		t.Errorf("ReqMaxTokens = %v, want nil (the client sent no cap and nothing was restamped)", *recs[0].ReqMaxTokens)
	}
}

// TestRequestOverridesScopeNoMatch pins the exact-leaf scope: a rule whose
// client, provider or model scope differs from the request never applies -
// the body bytes and the header set reach the upstream untouched - and a
// proxy with no rules configured is byte-identical (the feature-off row).
func TestRequestOverridesScopeNoMatch(t *testing.T) {
	tokens := 1234
	for _, tc := range []struct {
		name string
		rule *config.RequestOverride
	}{
		{"wrong client", &config.RequestOverride{Client: "other-client",
			Body: &config.OverrideBody{MaxTokens: &tokens}, Headers: map[string]string{"X-Lab-Mode": "ov"}}},
		{"wrong provider", &config.RequestOverride{Provider: "elsewhere.example",
			Body: &config.OverrideBody{MaxTokens: &tokens}, Headers: map[string]string{"X-Lab-Mode": "ov"}}},
		{"wrong model", &config.RequestOverride{Model: "other-model",
			Body: &config.OverrideBody{MaxTokens: &tokens}, Headers: map[string]string{"X-Lab-Mode": "ov"}}},
		{"no rules configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := config.Default()
			if tc.rule != nil {
				cfg.RequestOverrides = []config.RequestOverride{*tc.rule}
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(New(cfg, metrics.Noop{}))
			defer srv.Close()

			status, _ := postOverrideChat(t, srv.URL, upstream.URL, overrideChatBody,
				func(r *http.Request) { r.Header.Set("X-Proxy-Client", "ov-client") })
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			bodies, hdrs := capt.snapshot()
			if len(bodies) != 1 {
				t.Fatalf("upstream sends = %d, want 1", len(bodies))
			}
			if bodies[0] != overrideChatBody {
				t.Errorf("non-matching rule mutated the body:\n got %q\nwant %q", bodies[0], overrideChatBody)
			}
			if got := hdrs[0].Get("X-Lab-Mode"); got != "" {
				t.Errorf("non-matching rule applied header X-Lab-Mode = %q", got)
			}
		})
	}
}

// TestRequestOverridesOverlappingRulesLastActionWins pins the ordered walk
// across rules whose scopes overlap on one request: disjoint scope leaves
// (client and model) both match, and for any one header name or body field
// the last action in list order wins - a later remove beats an earlier
// set, a later set resurrects a name an earlier rule removed, and a later
// rule's body value beats an earlier rule's on the wire and the
// re-stamped record. The belt row pins the within-rule order a validated
// config can never express (one canonical name both set and removed in the
// same rule is rejected at load, so the rule is constructed directly
// without Validate): the rule's sets resolve before its removals, so the
// remove wins.
func TestRequestOverridesOverlappingRulesLastActionWins(t *testing.T) {
	first := 111
	second := 222
	for _, tc := range []struct {
		name       string
		rules      []config.RequestOverride
		validate   bool
		wantXLab   string // "" means absent upstream (every value here is non-empty)
		wantTokens int
	}{
		{"set then remove across rules, the later remove wins", []config.RequestOverride{
			{Client: "ov-client", Headers: map[string]string{"X-Lab-Mode": "rule-one"}},
			{Model: "model-a", RemoveHeaders: []string{"X-Lab-Mode"}},
		}, true, "", 10},
		{"remove then set across rules, the later set resurrects", []config.RequestOverride{
			{Client: "ov-client", RemoveHeaders: []string{"X-Lab-Mode"}},
			{Model: "model-a", Headers: map[string]string{"X-Lab-Mode": "rule-two"}},
		}, true, "rule-two", 10},
		{"later rule's body value wins per field", []config.RequestOverride{
			{Client: "ov-client", Body: &config.OverrideBody{MaxTokens: &first}},
			{Model: "model-a", Body: &config.OverrideBody{MaxTokens: &second}},
		}, true, "client-value", second},
		{"within-rule set and remove, the remove wins (invalid config, constructed directly)", []config.RequestOverride{
			{Client: "ov-client", Headers: map[string]string{"X-Lab-Mode": "rule-one"},
				RemoveHeaders: []string{"X-Lab-Mode"}},
		}, false, "", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := config.Default()
			cfg.RequestOverrides = tc.rules
			if tc.validate {
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			buf := metrics.NewBuffer(4)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, _ := postOverrideChat(t, srv.URL, upstream.URL, overrideChatBody, func(r *http.Request) {
				r.Header.Set("X-Proxy-Client", "ov-client")
				r.Header.Set("X-Lab-Mode", "client-value")
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			bodies, hdrs := capt.snapshot()
			if len(bodies) != 1 {
				t.Fatalf("upstream sends = %d, want 1", len(bodies))
			}
			var got struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
				t.Fatalf("upstream body is not JSON: %v (%s)", err, bodies[0])
			}
			if got.MaxTokens != tc.wantTokens {
				t.Errorf("upstream max_tokens = %d, want %d", got.MaxTokens, tc.wantTokens)
			}
			if gotX := hdrs[0].Get("X-Lab-Mode"); gotX != tc.wantXLab {
				t.Errorf("X-Lab-Mode = %q, want %q", gotX, tc.wantXLab)
			}
			recs := waitForRecord(t, buf, 1)
			if recs[0].ReqMaxTokens == nil || *recs[0].ReqMaxTokens != tc.wantTokens {
				t.Errorf("record ReqMaxTokens = %v, want %d re-read from the effective body",
					recs[0].ReqMaxTokens, tc.wantTokens)
			}
		})
	}
}

// TestRequestOverridesComposedCapSpellings pins the composed-cap rows the
// single-spelling tables cannot express: the two token-ceiling spellings
// are independent fields end to end. The shrug-off direction: a rule that
// sets only max_tokens cannot cap a client max_completion_tokens - the
// stamp lands on its own spelling, the client's survives verbatim on the
// wire, and the re-stamped record keeps the mct-wins precedence, so the
// effective upstream ceiling stays the client's value. The cross-rule
// mixed pair: rule one sets max_completion_tokens, a later rule sets
// max_tokens - the merge is per spelling, so BOTH rule values reach the
// wire (the later rule does not evict the earlier mct) and the effective
// ceiling is the mct value, not the later rule's max_tokens.
func TestRequestOverridesComposedCapSpellings(t *testing.T) {
	ruleMT, firstMCT, laterMT := 1234, 650, 430
	for _, tc := range []struct {
		name       string
		rules      []config.RequestOverride
		clientBody string
		wantMT     int // max_tokens on the rewritten wire body
		wantMCT    int // max_completion_tokens on the rewritten wire body
		wantEff    int // the record's ReqMaxTokens (mct-wins precedence)
	}{
		{"max_tokens rule cannot cap a client max_completion_tokens", []config.RequestOverride{{
			Client: "ov-client",
			Body:   &config.OverrideBody{MaxTokens: &ruleMT},
		}}, `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":900}`, ruleMT, 900, 900},
		{"mixed pair across rules merges per spelling, mct keeps precedence", []config.RequestOverride{
			{Client: "ov-client", Body: &config.OverrideBody{MaxCompletionTokens: &firstMCT}},
			{Model: "model-a", Body: &config.OverrideBody{MaxTokens: &laterMT}},
		}, `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, laterMT, firstMCT, firstMCT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := config.Default()
			cfg.RequestOverrides = tc.rules
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			buf := metrics.NewBuffer(4)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, respBody := postOverrideChat(t, srv.URL, upstream.URL, tc.clientBody,
				func(r *http.Request) { r.Header.Set("X-Proxy-Client", "ov-client") })
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", status, respBody)
			}
			bodies, _ := capt.snapshot()
			if len(bodies) != 1 {
				t.Fatalf("upstream sends = %d, want 1", len(bodies))
			}
			var got struct {
				MaxTokens     int `json:"max_tokens"`
				MaxCompletion int `json:"max_completion_tokens"`
			}
			if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
				t.Fatalf("upstream body is not JSON: %v (%s)", err, bodies[0])
			}
			if got.MaxTokens != tc.wantMT {
				t.Errorf("upstream max_tokens = %d, want %d", got.MaxTokens, tc.wantMT)
			}
			if got.MaxCompletion != tc.wantMCT {
				t.Errorf("upstream max_completion_tokens = %d, want %d", got.MaxCompletion, tc.wantMCT)
			}
			recs := waitForRecord(t, buf, 1)
			if recs[0].ReqMaxTokens == nil || *recs[0].ReqMaxTokens != tc.wantEff {
				t.Errorf("record ReqMaxTokens = %v, want %d (the effective ceiling keeps the mct-wins precedence)",
					recs[0].ReqMaxTokens, tc.wantEff)
			}
		})
	}
}

// TestRequestOverridesHeaderPrecedenceLast pins the chain order: the resolved
// override applies AFTER client-forwarded headers, the provider's configured
// headers and the client's explicit X-Proxy-Headers injection map - the
// operator's config wins over every per-request source - and its removals
// strip headers the provider map set.
func TestRequestOverridesHeaderPrecedenceLast(t *testing.T) {
	upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
	defer upstream.Close()
	cfg := config.Default()
	label := upstreamLabel(upstream.URL)
	cfg.Providers = map[string]config.ProviderOverride{label: {Headers: map[string]string{
		"X-Lab-Mode": "provider",
		"X-Gone":     "provider-set",
	}}}
	cfg.RequestOverrides = []config.RequestOverride{{
		Client:        "ov-client",
		Headers:       map[string]string{"X-Lab-Mode": "override"},
		RemoveHeaders: []string{"X-Gone"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	postOverrideChat(t, srv.URL, upstream.URL, overrideChatBody, func(r *http.Request) {
		r.Header.Set("X-Proxy-Client", "ov-client")
		r.Header.Set("X-Proxy-Headers", `{"X-Lab-Mode":["client-injected"]}`)
	})
	bodies, hdrs := capt.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	if got := hdrs[0].Get("X-Lab-Mode"); got != "override" {
		t.Errorf("X-Lab-Mode = %q, want the override value to win over the provider map and X-Proxy-Headers", got)
	}
	if got := hdrs[0].Get("X-Gone"); got != "" {
		t.Errorf("X-Gone = %q, want the provider-set header stripped by remove_headers", got)
	}
}

// TestRequestOverridesAuthHeaderRuntimeSkip pins the credential safety belt:
// a rule naming the target's configured auth header - set or remove, and
// x-api-key is NOT in the config forbidden set, so this is reachable through
// valid config - and a rule naming literal Authorization (which only a
// future grammar slip could produce; constructed directly here without
// Validate) are both skipped at runtime. The extracted upstream key keeps
// its owner.
func TestRequestOverridesAuthHeaderRuntimeSkip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rule     config.RequestOverride
		validate bool
	}{
		{"set names the target auth header", config.RequestOverride{
			Client:  "ov-client",
			Headers: map[string]string{"X-Api-Key": "evil-key"},
		}, true},
		{"remove names the target auth header", config.RequestOverride{
			Client:        "ov-client",
			RemoveHeaders: []string{"X-Api-Key"},
		}, true},
		{"set names authorization (future grammar slip)", config.RequestOverride{
			Client:  "ov-client",
			Headers: map[string]string{"Authorization": "Bearer evil"},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := config.Default()
			cfg.RequestOverrides = []config.RequestOverride{tc.rule}
			if tc.validate {
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(New(cfg, metrics.Noop{}))
			defer srv.Close()

			status, _ := postOverrideChat(t, srv.URL, upstream.URL, overrideChatBody, func(r *http.Request) {
				r.Header.Set("X-Proxy-Client", "ov-client")
				r.Header.Set("X-Proxy-Auth-Header", "X-Api-Key")
				r.Header.Set("X-Proxy-Auth-Prefix", "")
				r.Header.Set("Authorization", "Bearer sk-real")
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			bodies, hdrs := capt.snapshot()
			if len(bodies) != 1 {
				t.Fatalf("upstream sends = %d, want 1", len(bodies))
			}
			if got := hdrs[0].Get("X-Api-Key"); got != "sk-real" {
				t.Errorf("X-Api-Key = %q, want the extracted credential sk-real to survive the override", got)
			}
			if got := hdrs[0].Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want absent (the belt skips it; buildUpstreamRequest never forwards it)", got)
			}
		})
	}
}

// TestRequestOverridesCursorAuthHeaderRuntimeSkip pins the cursor identity
// stage's credential safety belt for a NONSTANDARD auth header. The target
// stamps its credential on X-Api-Key (X-Proxy-Auth-Header with an empty
// prefix), and the matching rule carries both auth-owned directions: a set
// naming literal Authorization (credential-owned names never pass config
// load, so only a future grammar slip could produce it - constructed
// directly here without Validate, the generic runtime-skip row's slip
// pattern) and a remove naming the configured X-Api-Key. The belt skips
// both at the native agent.v1 send: the wire shows the stamped credential
// intact on X-Api-Key, no Authorization, and the same rule's non-auth
// header applied - the proof that the cursor stage wires its resolved
// override into the shared applyOverrideHeaders.
func TestRequestOverridesCursorAuthHeaderRuntimeSkip(t *testing.T) {
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "cursor-key" {
			t.Errorf("X-Api-Key = %q, want the stamped raw credential intact at its owner", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want absent (the belt skips the rule's set; the cursor send never writes it)", got)
		}
		if got := r.Header.Get("X-Lab-Mode"); got != "ov" {
			t.Errorf("X-Lab-Mode = %q, want ov (the non-auth override header at the native send)", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/connect+proto" {
			t.Errorf("Content-Type = %q, want the protocol constant", ct)
		}
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, payload := readFrame(t, body)
		if !bytes.Contains(payload, []byte("claude-sonnet-4-5")) {
			t.Errorf("model id not in run_request: %x", payload)
		}
		cursorConnectHandshake(t, w, body, fl)
		textDelta := cmsg(1, cmsg(1, cstr(1, "bonjour")))
		tokenDelta := cmsg(1, cmsg(8, cvint(1, 4)))
		turnEnded := cmsg(1, cmsg(14, nil))
		w.Write(cframe(textDelta))
		w.Write(cframe(tokenDelta))
		w.Write(cframe(turnEnded))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	cfg := cursorTestCfg(upstream.URL)
	cfg.RequestOverrides = []config.RequestOverride{{
		Model:         "claude-sonnet-4-5",
		Headers:       map[string]string{"Authorization": "Bearer evil", "X-Lab-Mode": "ov"},
		RemoveHeaders: []string{"X-Api-Key"},
	}}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	req.Header.Set("X-Proxy-Auth-Header", "X-Api-Key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sse, _ := io.ReadAll(resp.Body)
	got := string(sse)
	if !strings.Contains(got, `"content":"bonjour"`) {
		t.Errorf("translated content missing: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one [DONE]: %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	if recs[0].Model != "claude-sonnet-4-5" {
		t.Errorf("record model = %q, want the canonical base id (the scope the rule matched)", recs[0].Model)
	}
}

// TestRequestOverridesSkipModelsFetch pins the "never the models-fetch
// builder" contract: the resolution happens after the models branch has
// returned, so a matching rule never touches discovery traffic.
func TestRequestOverridesSkipModelsFetch(t *testing.T) {
	upstream, capt := scriptedUpstream(t, overrideReply{200, `{"data":[{"id":"m-1","object":"model"}]}`})
	defer upstream.Close()
	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Client:  "ov-client",
		Headers: map[string]string{"X-Lab-Mode": "ov"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Client", "ov-client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200", resp.StatusCode)
	}
	bodies, hdrs := capt.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	if got := hdrs[0].Get("X-Lab-Mode"); got != "" {
		t.Errorf("models fetch carried override header X-Lab-Mode = %q; discovery is not an inference send", got)
	}
}
