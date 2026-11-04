package proxy

// Regression tests for the per-request tool-call bound. A nested messages[]
// tool_calls array is decoded into a typed slice per message by the Anthropic
// and Cursor translators, so a small body of empty calls used to materialize
// six figures of tool-call values (~130 MiB for 100k calls) before the
// aggregate content bound rejected the translated body - and the Cursor
// translation completed anyway. The lexical preflight now rejects the
// aggregate count before any translator or upstream sees the request, while
// the malformed-JSON passthrough contract and normal multi-tool requests stay
// unchanged.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// requestToolCallsArray renders n empty tool-call objects: the cheapest shape
// for amplifying a small body into a typed tool-call slice.
func requestToolCallsArray(calls int) string {
	return amplificationArray(calls, "{}")
}

// requestToolCallsBody renders an Anthropic-shaped conversation whose
// assistant turn carries n empty tool calls inside messages[].
func requestToolCallsBody(calls int) []byte {
	var b strings.Builder
	b.Grow(calls*3 + 128)
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":`)
	b.WriteString(requestToolCallsArray(calls))
	b.WriteString(`}]}`)
	return []byte(b.String())
}

// requestToolCallsMessage renders one assistant message carrying n calls.
func requestToolCallsMessage(calls int) string {
	return `{"role":"assistant","content":"","tool_calls":` + requestToolCallsArray(calls) + `}`
}

// TestRequestToolCallCountBound pins the aggregate lexical bound: every
// tool_calls array member across every message counts (including duplicate
// members and duplicate top-level messages arrays, whose slices encoding/json
// still decodes before last-wins assignment), while non-array shapes and
// malformed documents keep their existing tolerance and passthrough.
func TestRequestToolCallCountBound(t *testing.T) {
	normal := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c3","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`
	over := requestToolCallsBody(testConfig().RequestToolCallsMax + 1)
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"normal multi-tool accepted", normal, false},
		{"aggregate at cap accepted", `{"model":"m","messages":[` + requestToolCallsMessage(testConfig().RequestToolCallsMax) + `]}`, false},
		{"aggregate over cap rejected", string(over), true},
		{"calls across messages accumulate", `{"model":"m","messages":[` + requestToolCallsMessage(600) + `,` + requestToolCallsMessage(425) + `]}`, true},
		{"duplicate tool_calls members accumulate", `{"model":"m","messages":[{"role":"assistant","tool_calls":` + requestToolCallsArray(600) + `,"tool_calls":` + requestToolCallsArray(425) + `}]}`, true},
		{"duplicate tool_calls members at cap accepted", `{"model":"m","messages":[{"role":"assistant","tool_calls":` + requestToolCallsArray(512) + `,"tool_calls":` + requestToolCallsArray(512) + `}]}`, false},
		{"duplicate top-level messages accumulate", `{"model":"m","messages":[` + requestToolCallsMessage(600) + `],"messages":[` + requestToolCallsMessage(425) + `]}`, true},
		{"case-folded key counts", `{"model":"m","messages":[{"role":"assistant","TOOL_CALLS":` + requestToolCallsArray(600) + `},{"role":"assistant","tool_calls":` + requestToolCallsArray(425) + `}]}`, true},
		{"escaped key counts", `{"model":"m","messages":[{"role":"assistant","tool_call\u0073":` + requestToolCallsArray(600) + `},{"role":"assistant","tool_calls":` + requestToolCallsArray(425) + `}]}`, true},
		{"non-array shape tolerated", `{"model":"m","messages":[{"role":"assistant","tool_calls":{"id":"c1"}}]}`, false},
		{"malformed over-bound prefix stays passthrough", string(over[:len(over)-1]) + `,X}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			err := parseTestRequest([]byte(tc.body), &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && (!strings.Contains(err.Error(), "tool calls") || !strings.Contains(err.Error(), strconv.Itoa(testConfig().RequestToolCallsMax))) {
				t.Fatalf("tool-call overflow error = %v, want the bounded tool-call wording", err)
			}
		})
	}
}

// TestRequestToolCallAllocationBound keeps the tool-call preflight lexical:
// the 300 KiB, 100,000-call body that used to allocate ~127 MiB inside the
// Anthropic translator must be rejected from the body bytes alone.
func TestRequestToolCallAllocationBound(t *testing.T) {
	const calls = 100_000
	raw := requestToolCallsBody(calls)
	var parseErr error
	alloc, _ := allocStats(func() {
		var rec metrics.Record
		parseErr = parseTestRequest(raw, &rec, false)
		if parseErr == nil {
			t.Fatalf("%d-call body was accepted", calls)
		}
	})
	if alloc > 4<<20 {
		t.Fatalf("%d-byte %d-call body allocated %d bytes/op, want the bounded lexical preflight", len(raw), calls, alloc)
	}
	t.Logf("tool-call over-bound: input=%d calls=%d alloc=%d", len(raw), calls, alloc)
}

// TestRequestToolCallsReturn400BeforeTranslationAndUpstream pins the HTTP
// boundary: the over-bound body fails with the shared 400 before the
// Anthropic translator (whose 100k-block output used to allocate ~127 MiB) or
// any upstream call, and a normal three-tool request still translates.
func TestRequestToolCallsReturn400BeforeTranslationAndUpstream(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	post := func(body []byte) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-ant")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
		req.Header.Set("X-Proxy-Auth-Prefix", "")
		req.Header.Set("X-Proxy-Path", "/messages")
		req.Header.Set("X-Proxy-Format", "anthropic")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	status, respBody := post(requestToolCallsBody(100_000))
	if status != http.StatusBadRequest {
		t.Fatalf("over-bound status = %d, want 400 (body: %s)", status, respBody)
	}
	if !strings.Contains(respBody, "invalid_request_error") || !strings.Contains(respBody, "tool calls") {
		t.Fatalf("over-bound body = %q, want the shared bounded invalid_request_error", respBody)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times for the over-bound body, want 0", got)
	}

	normal := `{"model":"claude","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c3","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`
	status, respBody = post([]byte(normal))
	if status != http.StatusOK {
		t.Fatalf("normal multi-tool status = %d, want 200 (body: %s)", status, respBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 after the accepted control", got)
	}
	mu.Lock()
	got := string(upstreamBody)
	mu.Unlock()
	if n := strings.Count(got, `"type":"tool_use"`); n != 3 {
		t.Fatalf("translated tool_use blocks = %d, want 3 (body: %s)", n, got)
	}
}

// TestRequestToolCallBoundIsConfigurable proves the preflight applies the
// bound it is given instead of a package constant: the same body passes at a
// raised bound, fails at the documented default, and fails earlier at a
// lowered bound.
func TestRequestToolCallBoundIsConfigurable(t *testing.T) {
	body := requestToolCallsBody(2048)
	parse := func(bound int) error {
		cfg := testConfig()
		cfg.RequestToolCallsMax = bound
		var rec metrics.Record
		return parseLLMRequest(body, &rec, false, cfg)
	}
	if err := parse(testConfig().RequestToolCallsMax); err == nil {
		t.Fatal("default bound accepted 2048 calls")
	}
	if err := parse(4096); err != nil {
		t.Fatalf("raised bound rejected 2048 calls: %v", err)
	}
	small := requestToolCallsBody(2)
	cfg := testConfig()
	cfg.RequestToolCallsMax = 1
	var rec metrics.Record
	err := parseLLMRequest(small, &rec, false, cfg)
	if err == nil || !strings.Contains(err.Error(), "request tool calls exceed 1") {
		t.Fatalf("lowered bound error = %v, want the configured 1-call wording", err)
	}
}

// TestRequestToolCallBoundCountsLegacyFunctionCall pins the deprecated
// per-message function_call shape in the same aggregate as tool_calls: it
// materializes a typed call too, so letting it bypass the setting would reopen
// the amplification the preflight exists to stop.
func TestRequestToolCallBoundCountsLegacyFunctionCall(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"","function_call":{"name":"f","arguments":"{}"}},` +
		`{"role":"assistant","content":"","function_call":{"name":"g","arguments":"{}"}}]}`)
	cfg := testConfig()
	cfg.RequestToolCallsMax = 1
	var rec metrics.Record
	err := parseLLMRequest(body, &rec, false, cfg)
	if err == nil || !strings.Contains(err.Error(), "request tool calls exceed 1") {
		t.Fatalf("two legacy calls error = %v, want the configured 1-call wording", err)
	}
	cfg.RequestToolCallsMax = 2
	if err := parseLLMRequest(body, &rec, false, cfg); err != nil {
		t.Fatalf("two legacy calls at the configured bound rejected: %v", err)
	}
	// A null legacy value is no call.
	nullBody := []byte(`{"model":"m","messages":[{"role":"assistant","content":"","function_call":null}]}`)
	cfg.RequestToolCallsMax = 1
	if err := parseLLMRequest(nullBody, &rec, false, cfg); err != nil {
		t.Fatalf("null function_call counted as a call: %v", err)
	}
}
