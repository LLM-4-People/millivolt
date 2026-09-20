package proxy

// Sub-conversation tracking (config sub_conversations): the proxy mechanism
// only. The config layer's validation, schema and coercion live in
// tests/_go/internal/config/subconversations.go; these rows pin the runtime
// contract - per-client extraction, first-present-wins param order, the
// drop-not-reject value bound, the k: identity and its precedence under the
// s: header and c- auto grouping, strip folded into the single body-rewrite
// engine (one rewrite across relay retries and quality re-sends), the
// translated-path prompt_cache_key injection, and the cursor wire gate.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// scBody builds a compact chat-completions body with n user turns and an
// optional promptCacheKey field, the tracked-param exemplar shape.
func scBody(n int, param string) string {
	var b strings.Builder
	b.WriteString(`{"model":"model-a","messages":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"turn"}`)
	}
	b.WriteString(`]`)
	if param != "" {
		b.WriteString(`,"promptCacheKey":"` + param + `"`)
	}
	b.WriteString(`}`)
	return b.String()
}

// scCfg builds a validated default config tracking one classified client.
func scCfg(t *testing.T, strip bool, params ...string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.SubConversations = []config.SubConversation{{
		Client: "sc-client",
		Params: params,
		Strip:  strip,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// scUpstreamBody decodes one captured upstream body or fails the test.
func scUpstreamBody(t *testing.T, bodies []string) map[string]json.RawMessage {
	t.Helper()
	if len(bodies) != 1 {
		t.Fatalf("upstream sends = %d, want 1", len(bodies))
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bodies[0]), &doc); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, bodies[0])
	}
	return doc
}

// TestSubConversationsNoConfigByteTransparency pins the feature-off row: a
// body that happens to carry a tracked-shaped field relays byte-identical,
// and grouping follows the pre-feature auto rules (monotonic turn growth
// continues one conversation).
func TestSubConversationsNoConfigByteTransparency(t *testing.T) {
	upstream, capt := scriptedUpstream(t,
		overrideReply{200, overrideGoodCompletion},
		overrideReply{200, overrideGoodCompletion})
	defer upstream.Close()
	cfg := config.Default()
	if cfg.SubConversations != nil {
		t.Fatal("Default() must leave sub_conversations nil (the only default site)")
	}
	buf := metrics.NewBuffer(4)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	first := scBody(3, "task-1")
	second := scBody(4, "task-1")
	for _, body := range []string{first, second} {
		status, respBody := postOverrideChat(t, srv.URL, upstream.URL, body, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", status, respBody)
		}
	}
	bodies, _ := capt.snapshot()
	if len(bodies) != 2 {
		t.Fatalf("upstream sends = %d, want 2", len(bodies))
	}
	if bodies[0] != first || bodies[1] != second {
		t.Errorf("feature-off body mutated:\n got %q / %q\nwant %q / %q", bodies[0], bodies[1], first, second)
	}
	recs := waitForRecord(t, buf, 2)
	if !strings.HasPrefix(recs[0].ConversationID, "c-") || recs[0].ConversationID != recs[1].ConversationID {
		t.Errorf("feature-off grouping changed: %q then %q, want one c- auto conversation",
			recs[0].ConversationID, recs[1].ConversationID)
	}
}

// TestSubConversationsExtractionByClient pins the deny-by-default surface:
// only the classified client named by a configured entry is tracked. A
// configured client's param value becomes its k: conversation identity; an
// unconfigured client and a param-absent body stay byte-identical and on
// automatic grouping.
func TestSubConversationsExtractionByClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client string
		body   string
		wantID string // "" wants any c- auto id
	}{
		{"configured client extracts the param value", "sc-client", scBody(1, "task-1"), "k:task-1"},
		{"unconfigured client untouched", "other-client", scBody(1, "task-1"), ""},
		{"configured client without the param stays auto", "sc-client", scBody(1, ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := scCfg(t, false, "promptCacheKey")
			buf := metrics.NewBuffer(2)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, _ := postOverrideChat(t, srv.URL, upstream.URL, tc.body,
				func(r *http.Request) { r.Header.Set("X-Proxy-Client", tc.client) })
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			bodies, _ := capt.snapshot()
			if len(bodies) != 1 || bodies[0] != tc.body {
				t.Fatalf("strip is off but the body mutated:\n got %q\nwant %q", bodies, tc.body)
			}
			recs := waitForRecord(t, buf, 1)
			got := recs[0].ConversationID
			if tc.wantID == "" {
				if !strings.HasPrefix(got, "c-") {
					t.Errorf("conversation id = %q, want a c- auto id (untracked)", got)
				}
				return
			}
			if got != tc.wantID {
				t.Errorf("conversation id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestSubConversationsFirstPresentWins pins the ordered param walk: the
// first param whose value tail supplies a decodable string wins; a non-string
// value does not supply (the next param is checked), but a present value that
// fails the identity bound is dropped without falling through - the
// configured order stays presence-ordered, never value-quality-ordered.
func TestSubConversationsFirstPresentWins(t *testing.T) {
	cfg := scCfg(t, false, "primaryKey", "secondaryKey")
	for _, tc := range []struct {
		name   string
		body   string
		wantID string // "" wants any c- auto id
	}{
		{"both present, the first wins",
			`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"primaryKey":"first-val","secondaryKey":"second-val"}`,
			"k:first-val"},
		{"only the second is present",
			`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"secondaryKey":"second-val"}`,
			"k:second-val"},
		{"a non-string value does not supply, the next param is checked",
			`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"primaryKey":123,"secondaryKey":"second-val"}`,
			"k:second-val"},
		{"an invalid first value is dropped without falling through",
			`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"primaryKey":"bad` + `\u0000` + `value","secondaryKey":"second-val"}`,
			""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			buf := metrics.NewBuffer(2)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, _ := postOverrideChat(t, srv.URL, upstream.URL, tc.body,
				func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			bodies, _ := capt.snapshot()
			if len(bodies) != 1 || bodies[0] != tc.body {
				t.Fatalf("strip is off but the body mutated:\n got %q\nwant %q", bodies, tc.body)
			}
			recs := waitForRecord(t, buf, 1)
			got := recs[0].ConversationID
			if tc.wantID == "" {
				if !strings.HasPrefix(got, "c-") {
					t.Errorf("dropped value must leave the request untracked, got %q", got)
				}
				return
			}
			if got != tc.wantID {
				t.Errorf("conversation id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestSubConversationsValueBoundsDropAndStrip pins the drop-not-reject
// contract: an invalid tracked value (control bytes, over-length, invalid
// UTF-8, empty after trim) never rejects the request - the identity is
// dropped and the request stays on automatic grouping - while strip, keyed
// on field presence, still removes the field for the strict upstream. The
// absent row pins that no param present means no rewrite at all.
func TestSubConversationsValueBoundsDropAndStrip(t *testing.T) {
	const head = `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"max_tokens":10`
	rows := []struct {
		name string
		body string
		// stripped rows have the field removed upstream; the absent row must
		// relay byte-identical (no rewrite fires at all).
		stripped bool
	}{
		{"control bytes are dropped, the field still stripped",
			head + `,"promptCacheKey":"bad\u0000value"}`, true},
		{"513 bytes are dropped, the field still stripped",
			head + `,"promptCacheKey":"` + strings.Repeat("x", 513) + `"}`, true},
		{"invalid utf-8 is dropped, the field still stripped",
			head + `,"promptCacheKey":"a` + "\xff" + `b"}`, true},
		{"empty after trim is dropped, the field still stripped",
			head + `,"promptCacheKey":"   "}`, true},
		{"param absent, no rewrite fires",
			head + `}`, false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
			defer upstream.Close()
			cfg := scCfg(t, true, "promptCacheKey")
			buf := metrics.NewBuffer(2)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, respBody := postOverrideChat(t, srv.URL, upstream.URL, tc.body,
				func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a bad tracked value is never a 400; body %s)", status, respBody)
			}
			bodies, _ := capt.snapshot()
			if len(bodies) != 1 {
				t.Fatalf("upstream sends = %d, want 1", len(bodies))
			}
			if !tc.stripped {
				if bodies[0] != tc.body {
					t.Fatalf("no param was present, but the body mutated:\n got %q\nwant %q", bodies[0], tc.body)
				}
			} else {
				doc := scUpstreamBody(t, bodies)
				if _, ok := doc["promptCacheKey"]; ok {
					t.Errorf("strip left the tracked field upstream: %s", bodies[0])
				}
				var got struct {
					MaxTokens int `json:"max_tokens"`
					Model     string
					Messages  []json.RawMessage
				}
				if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil || got.MaxTokens != 10 || got.Model != "model-a" || len(got.Messages) != 1 {
					t.Errorf("strip damaged unrelated fields: %s", bodies[0])
				}
			}
			recs := waitForRecord(t, buf, 1)
			if got := recs[0].ConversationID; !strings.HasPrefix(got, "c-") {
				t.Errorf("conversation id = %q, want a c- auto id (the invalid value was dropped)", got)
			}
		})
	}
}

// TestSubConversationsHeaderPrecedence pins the identity order: X-Proxy-Session
// wins over the tracked param, which is then ignored for identity - while
// strip, keyed on the entry and orthogonal to identity precedence, still
// removes the field.
func TestSubConversationsHeaderPrecedence(t *testing.T) {
	upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
	defer upstream.Close()
	cfg := scCfg(t, true, "promptCacheKey")
	buf := metrics.NewBuffer(2)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	body := scBody(1, "task-9")
	status, _ := postOverrideChat(t, srv.URL, upstream.URL, body, func(r *http.Request) {
		r.Header.Set("X-Proxy-Client", "sc-client")
		r.Header.Set("X-Proxy-Session", "sess-1")
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	doc := scUpstreamBody(t, mustCapture(capt))
	if _, ok := doc["promptCacheKey"]; ok {
		t.Errorf("strip must follow the entry even when the header wins identity: %s", mustCapture(capt)[0])
	}
	recs := waitForRecord(t, buf, 1)
	if got := recs[0].ConversationID; got != "s:sess-1" {
		t.Errorf("conversation id = %q, want the explicit s:sess-1 (the header wins)", got)
	}
}

// mustCapture snapshots the scripted upstream's captured bodies.
func mustCapture(capt *overrideCapture) []string {
	bodies, _ := capt.snapshot()
	return bodies
}

// TestSubConversationsKIdentityShortCircuitsAuto pins the k: assignment
// semantics: one param value groups across turn-count resets that would
// split an auto conversation (an explicit-identity short-circuit, exactly
// like s:), and distinct values split.
func TestSubConversationsKIdentityShortCircuitsAuto(t *testing.T) {
	upstream, capt := scriptedUpstream(t,
		overrideReply{200, overrideGoodCompletion},
		overrideReply{200, overrideGoodCompletion},
		overrideReply{200, overrideGoodCompletion})
	defer upstream.Close()
	cfg := scCfg(t, false, "promptCacheKey")
	buf := metrics.NewBuffer(4)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	want := []string{"k:task-a", "k:task-a", "k:task-b"}
	sent := []string{scBody(4, "task-a"), scBody(1, "task-a"), scBody(4, "task-b")}
	for _, body := range sent {
		status, _ := postOverrideChat(t, srv.URL, upstream.URL, body,
			func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	}
	bodies, _ := capt.snapshot()
	if len(bodies) != 3 {
		t.Fatalf("upstream sends = %d, want 3", len(bodies))
	}
	for i := range sent {
		if bodies[i] != sent[i] {
			t.Errorf("send %d mutated (strip is off):\n got %q\nwant %q", i, bodies[i], sent[i])
		}
	}
	recs := waitForRecord(t, buf, 3)
	for i := range want {
		if got := recs[i].ConversationID; got != want[i] {
			t.Errorf("request %d conversation id = %q, want %q", i, got, want[i])
		}
	}
}

// TestSubConversationsStripAcrossRetriesAndResends pins the single body seam
// end to end: the stripped field is absent from EVERY upstream send - the
// initial relay attempt, the absorbed 503 retry and the degenerate-200
// quality re-send all reuse the same rewritten bytes - and the extracted
// value still carries the k: identity.
func TestSubConversationsStripAcrossRetriesAndResends(t *testing.T) {
	upstream, capt := scriptedUpstream(t,
		overrideReply{503, `{"error":{"message":"overloaded","type":"server_error","code":"server_is_overloaded"}}`},
		// Degenerate void: choices present, no answer content, no tools -
		// serveNonStreaming absorbs it and re-sends before any byte reaches
		// the client.
		overrideReply{200, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`},
		overrideReply{200, overrideGoodCompletion},
	)
	defer upstream.Close()
	cfg := scCfg(t, true, "promptCacheKey")
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	body := scBody(1, "task-5")
	status, respBody := postOverrideChat(t, srv.URL, upstream.URL, body,
		func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
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
			PromptCacheKey json.RawMessage `json:"promptCacheKey"`
			Model          string
			Messages       []json.RawMessage
		}
		if err := json.Unmarshal([]byte(b), &got); err != nil {
			t.Fatalf("send %d body is not JSON: %v (%s)", i, err, b)
		}
		if len(got.PromptCacheKey) > 0 {
			t.Errorf("send %d: the stripped field reached the upstream: %s", i, b)
		}
		if got.Model != "model-a" || len(got.Messages) != 1 {
			t.Errorf("send %d: unrelated fields not preserved verbatim: %s", i, b)
		}
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ConversationID != "k:task-5" {
		t.Errorf("conversation id = %q, want k:task-5", rec.ConversationID)
	}
	if rec.Retries != 2 {
		t.Errorf("Retries = %d, want 2 (the absorbed 503 and the degenerate re-send)", rec.Retries)
	}
}

// TestSubConversationsStripAndOverrideShareOneRewrite pins the single
// body-rewrite engine: a request-overrides body action and strip both land
// in ONE rewrite (one send, both effects on the wire, one re-stamped
// record), and an undecodable body fails closed through that one engine
// with exactly one operator-actionable log line - a duplicated engine would
// log and relay twice.
func TestSubConversationsStripAndOverrideShareOneRewrite(t *testing.T) {
	tokens := 1234
	newCfg := func(t *testing.T) *config.Config {
		cfg := scCfg(t, true, "promptCacheKey")
		cfg.RequestOverrides = []config.RequestOverride{{
			Client: "sc-client",
			Body:   &config.OverrideBody{MaxTokens: &tokens},
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("both effects in one send", func(t *testing.T) {
		upstream, capt := scriptedUpstream(t, overrideReply{200, overrideGoodCompletion})
		defer upstream.Close()
		buf := metrics.NewBuffer(4)
		srv := httptest.NewServer(New(newCfg(t), buf))
		defer srv.Close()

		body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"promptCacheKey":"task-8"}`
		status, _ := postOverrideChat(t, srv.URL, upstream.URL, body,
			func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		doc := scUpstreamBody(t, mustCapture(capt))
		if _, ok := doc["promptCacheKey"]; ok {
			t.Errorf("strip did not remove the tracked field: %s", mustCapture(capt)[0])
		}
		var got struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal([]byte(mustCapture(capt)[0]), &got); err != nil || got.MaxTokens != tokens {
			t.Errorf("override ceiling missing from the shared rewrite: %s", mustCapture(capt)[0])
		}
		recs := waitForRecord(t, buf, 1)
		if recs[0].ConversationID != "k:task-8" {
			t.Errorf("conversation id = %q, want k:task-8", recs[0].ConversationID)
		}
		if recs[0].ReqMaxTokens == nil || *recs[0].ReqMaxTokens != tokens {
			t.Errorf("record ReqMaxTokens = %v, want %d re-stamped from the one rewrite", recs[0].ReqMaxTokens, tokens)
		}
	})

	t.Run("one fail-closed log line for the shared engine", func(t *testing.T) {
		upstream, capt := scriptedUpstream(t, overrideReply{200, `{"ok":true}`})
		defer upstream.Close()
		buf := metrics.NewBuffer(4)
		srv := httptest.NewServer(New(newCfg(t), buf))
		defer srv.Close()

		// Truncated JSON carrying the param lexically: both features want a
		// rewrite, the one engine fails closed on it once.
		rawBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"promptCacheKey":"x","max_tokens":`
		var logBuf bytes.Buffer
		log.SetOutput(&logBuf)
		defer log.SetOutput(os.Stderr)
		status, respBody := postOverrideChat(t, srv.URL, upstream.URL, rawBody,
			func(r *http.Request) { r.Header.Set("X-Proxy-Client", "sc-client") })
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the invalid body relays verbatim)", status)
		}
		if strings.TrimSpace(respBody) != `{"ok":true}` {
			t.Errorf("client body = %q, want the upstream reply", respBody)
		}
		bodies, _ := capt.snapshot()
		if len(bodies) != 1 || bodies[0] != rawBody {
			t.Fatalf("upstream body mutated by the failed rewrite:\n got %q\nwant %q", bodies, rawBody)
		}
		logged := logBuf.String()
		if n := strings.Count(logged, "request overrides:"); n != 1 {
			t.Fatalf("body-rewrite skip log lines = %d, want exactly 1 (one shared engine, not one per feature): %q", n, logged)
		}
		for _, want := range []string{"body rewrite skipped", `"sc-client"`, "not a JSON object", "relay"} {
			if !strings.Contains(logged, want) {
				t.Errorf("log line omits %q: %q", want, logged)
			}
		}
		recs := waitForRecord(t, buf, 1)
		if recs[0].ReqMaxTokens != nil {
			t.Errorf("ReqMaxTokens = %v, want nil (nothing was restamped)", *recs[0].ReqMaxTokens)
		}
	})
}

// TestSubConversationsTranslatedAnthropicInjectsAndCarries pins the
// translated path: the tracked value survives translation as the record's k:
// identity, the translated upstream body carries it as Anthropic's native
// top-level prompt_cache_key, an untracked body gains no field, and strip
// applies to the original body pre-translation while the injection (keyed on
// extraction alone) still lands.
func TestSubConversationsTranslatedAnthropicInjectsAndCarries(t *testing.T) {
	const anthropicReply = `{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`
	for _, tc := range []struct {
		name    string
		strip   bool
		body    string
		wantKey string // expected prompt_cache_key value; "" means absent
		wantID  string // "" wants any c- auto id
	}{
		{"tracked value injects prompt_cache_key", false,
			`{"model":"claude","messages":[{"role":"user","content":"hi"}],"promptCacheKey":"task-7"}`, "task-7", "k:task-7"},
		{"untracked body omits prompt_cache_key", false,
			`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`, "", ""},
		{"strip then translate keeps the injection", true,
			`{"model":"claude","messages":[{"role":"user","content":"hi"}],"promptCacheKey":"task-7"}`, "task-7", "k:task-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, capt := scriptedUpstream(t, overrideReply{200, anthropicReply})
			defer upstream.Close()
			cfg := scCfg(t, tc.strip, "promptCacheKey")
			buf := metrics.NewBuffer(2)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, respBody := postOverrideChat(t, srv.URL, upstream.URL, tc.body, func(r *http.Request) {
				r.Header.Set("X-Proxy-Client", "sc-client")
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
			doc := scUpstreamBody(t, bodies)
			raw, ok := doc["prompt_cache_key"]
			if tc.wantKey == "" {
				if ok {
					t.Errorf("untracked request gained a prompt_cache_key: %s", bodies[0])
				}
			} else {
				var gotKey string
				if !ok || json.Unmarshal(raw, &gotKey) != nil || gotKey != tc.wantKey {
					t.Errorf("translated prompt_cache_key = %s, want %q (body %s)", raw, tc.wantKey, bodies[0])
				}
			}
			if strings.Contains(bodies[0], "promptCacheKey") {
				t.Errorf("the client's own field spelling reached the translated body: %s", bodies[0])
			}
			recs := waitForRecord(t, buf, 1)
			got := recs[0].ConversationID
			if tc.wantID == "" {
				if !strings.HasPrefix(got, "c-") {
					t.Errorf("conversation id = %q, want a c- auto id", got)
				}
				return
			}
			if got != tc.wantID {
				t.Errorf("conversation id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestSubConversationsCursorWireGate pins the cursor contract: the tracked
// value still carries the k: identity, but cursor bodies never reach the
// OpenAI-wire rewrite engine (the existing wire gate skips them), so strip
// is a no-op and the native agent.v1 run_request carries neither the client's
// field nor an injected key.
func TestSubConversationsCursorWireGate(t *testing.T) {
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/connect+proto" {
			t.Errorf("Content-Type = %q, want the protocol constant", ct)
		}
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, payload := readFrame(t, body)
		if bytes.Contains(payload, []byte("promptCacheKey")) {
			t.Errorf("client field reached the run_request: %x", payload)
		}
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
	cfg.SubConversations = []config.SubConversation{{
		Client: "sc-client",
		Params: []string{"promptCacheKey"},
		Strip:  true,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true,"promptCacheKey":"task-c"}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	req.Header.Set("X-Proxy-Client", "sc-client")
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
	if recs[0].ConversationID != "k:task-c" {
		t.Errorf("conversation id = %q, want k:task-c (extraction runs; the wire gate skips only the rewrite)", recs[0].ConversationID)
	}
}

// TestSubConversationExtractionUnits pins the extraction pipeline at its
// owner: the feature-off and no-entry cheap checks, the exact-leaf client
// match, the JSONKey locate (escape-spelled keys, nesting), the bounded value
// decode, and the declared-session bound's drop rows - including the boundary
// byte and the presence-ordered walk a black-row HTTP test cannot see.
func TestSubConversationExtractionUnits(t *testing.T) {
	entries := []config.SubConversation{
		{Client: "sc-client", Params: []string{"promptCacheKey"}},
		{Client: "sc-two", Params: []string{"primaryKey", "secondaryKey"}},
	}
	bound := strings.Repeat("x", maxDeclaredSessionBytes)
	for _, tc := range []struct {
		name      string
		entries   []config.SubConversation
		client    string
		body      string
		wantEntry bool
		wantValue string
	}{
		{"feature off is one cheap check", nil, "sc-client", `{"promptCacheKey":"task-1"}`, false, ""},
		{"no entry for the client", entries, "other", `{"promptCacheKey":"task-1"}`, false, ""},
		{"exact leaf match extracts", entries, "sc-client", `{"promptCacheKey":"task-1"}`, true, "task-1"},
		{"value is trimmed to its identity", entries, "sc-client", `{"promptCacheKey":"  task-1  "}`, true, "task-1"},
		{"escape-spelled key is located", entries, "sc-client", `{"promptC\u0061cheKey":"task-2"}`, true, "task-2"},
		{"escaped value decodes", entries, "sc-client", `{"promptCacheKey":"a\"b\\c"}`, true, "a\"b\\c"},
		{"nested key is located (the JSONKey class)", entries, "sc-client", `{"wrap":{"promptCacheKey":"task-3"}}`, true, "task-3"},
		{"non-string value does not supply", entries, "sc-client", `{"promptCacheKey":123}`, true, ""},
		{"null value does not supply", entries, "sc-client", `{"promptCacheKey":null}`, true, ""},
		{"empty string is dropped", entries, "sc-client", `{"promptCacheKey":""}`, true, ""},
		{"whitespace-only string is dropped", entries, "sc-client", `{"promptCacheKey":"   "}`, true, ""},
		{"escaped control bytes are dropped", entries, "sc-client", `{"promptCacheKey":"a\u0000b"}`, true, ""},
		{"raw invalid utf-8 is dropped", entries, "sc-client", `{"promptCacheKey":"a` + "\xff" + `b"}`, true, ""},
		{"513 bytes are dropped", entries, "sc-client", `{"promptCacheKey":"` + strings.Repeat("x", maxDeclaredSessionBytes+1) + `"}`, true, ""},
		{"512 bytes are the bound", entries, "sc-client", `{"promptCacheKey":"` + bound + `"}`, true, bound},
		{"an over-scan value is dropped", entries, "sc-client", `{"promptCacheKey":"` + strings.Repeat("y", 6*maxDeclaredSessionBytes+3) + `"}`, true, ""},
		{"unterminated string is dropped", entries, "sc-client", `{"promptCacheKey":"abc`, true, ""},
		{"non-string first param falls through to the second", entries, "sc-two", `{"primaryKey":123,"secondaryKey":"second"}`, true, "second"},
		{"an invalid supplied value never falls through", entries, "sc-two", `{"primaryKey":"bad\u0001x","secondaryKey":"second"}`, true, ""},
		{"the first present param wins", entries, "sc-two", `{"primaryKey":"first","secondaryKey":"second"}`, true, "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, value := resolveSubConversation(tc.entries, tc.client, []byte(tc.body))
			if (entry != nil) != tc.wantEntry {
				t.Fatalf("entry matched = %v, want %v", entry != nil, tc.wantEntry)
			}
			if value != tc.wantValue {
				t.Errorf("value = %q, want %q", value, tc.wantValue)
			}
		})
	}
}

// TestSubConversationIdentityNamespaces pins the k: assignment beside the s:
// header identity at the tracker: the header wins, a param identity groups
// like an explicit one (a turn-count reset does not split it), and the
// namespaces cannot collide with each other or an auto id.
func TestSubConversationIdentityNamespaces(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	if got := ct.Assign("cli", "k", "", "task-1", 0, now); got != "k:task-1" {
		t.Errorf("tracked identity = %q, want k:task-1", got)
	}
	if got := ct.Assign("cli", "k", "sess", "task-1", 0, now); got != "s:sess" {
		t.Errorf("header must win identity: %q, want s:sess", got)
	}
	// A k: identity groups across a turn-count reset like an explicit one.
	a := ct.Assign("cli", "k", "", "task-2", 4, now)
	b := ct.Assign("cli", "k", "", "task-2", 1, now.Add(time.Second))
	if a != "k:task-2" || b != a {
		t.Errorf("k identity split across a reset: %q then %q", a, b)
	}
	// No namespace collides with another or with an auto id.
	c := ct.Assign("cli", "k", "", "task-3", 1, now)
	auto := ct.Assign("cli", "k", "", "", 1, now)
	if !strings.HasPrefix(auto, "c-") || c == a || c == "k:task-1" || c == auto {
		t.Errorf("identity collision: k ids %q %q %q, auto %q", "k:task-1", a, c, auto)
	}
}
