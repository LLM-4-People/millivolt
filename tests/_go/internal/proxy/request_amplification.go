package proxy

// Request amplification bounds: the lexical preflight rejects oversized stop
// arrays, distinct top-level member sets, and aggregate content-part arrays
// before the metadata decode or the rewrite engine can materialize them.
// These rows pin the acceptance controls (a string stop, a 10k-key body,
// normal multimodal content) beside the over-bound rejections, the malformed
// passthrough contract, the shared 400 before any upstream call, and the
// allocation ceiling that motivated each bound.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// amplificationArray renders n copies of elem as one JSON array.
func amplificationArray(n int, elem string) string {
	if n == 0 {
		return "[]"
	}
	return "[" + strings.TrimSuffix(strings.Repeat(elem+",", n), ",") + "]"
}

// amplificationMembers renders n distinct top-level members starting at start.
func amplificationMembers(start, n int) string {
	buf := make([]byte, 0, n*12)
	for i := start; i < start+n; i++ {
		if i > start {
			buf = append(buf, ',')
		}
		buf = append(buf, '"', 'k')
		buf = strconv.AppendInt(buf, int64(i), 10)
		buf = append(buf, '"', ':', '0')
	}
	return string(buf)
}

// amplificationChatBody prefixes model/messages to a member tail. The prefix
// contributes two distinct top-level keys.
func amplificationChatBody(members string) string {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]`
	if members != "" {
		body += "," + members
	}
	return body + "}"
}

// amplificationStopBody renders a stop array with entries quoted strings,
// the shape that expands into []string for the count decode.
func amplificationStopBody(entries int) []byte {
	return []byte(`{"model":"m","stop":` + amplificationArray(entries, `"xxx"`) + `}`)
}

// amplificationAggregateBody renders messages messages, each carrying one
// content array of parts empty objects - the shape that composes a typed part
// slice per message.
func amplificationAggregateBody(messages, parts int) []byte {
	content := amplificationArray(parts, "{}")
	var b strings.Builder
	b.Grow(messages * (len(content) + 40))
	b.WriteString(`{"model":"m","messages":[`)
	for i := 0; i < messages; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":`)
		b.WriteString(content)
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func TestRequestStopArrayBound(t *testing.T) {
	over := amplificationArray(requestMetadataMaxElements+1, `"x"`)
	atCap := amplificationArray(requestMetadataMaxElements, `"x"`)
	for _, tc := range []struct {
		name     string
		body     string
		wantErr  bool
		wantStop int
	}{
		{"string is one value", `{"model":"m","stop":"done"}`, false, 1},
		{"array at cap", `{"model":"m","stop":` + atCap + `}`, false, requestMetadataMaxElements},
		{"array over cap", `{"model":"m","stop":` + over + `}`, true, 0},
		{"case-folded key over cap", `{"model":"m","STOP":` + over + `}`, true, 0},
		{"escaped key over cap", `{"model":"m","st\u006fp":` + over + `}`, true, 0},
		{"object is tolerated", `{"model":"m","stop":{"k":1}}`, false, 0},
		{"number is tolerated", `{"model":"m","stop":3}`, false, 0},
		{"null is tolerated", `{"model":"m","stop":null}`, false, 1},
		{"malformed over-cap prefix stays passthrough", `{"model":"m","stop":` + over + `,X}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			err := parseLLMRequest([]byte(tc.body), &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), "stop") || !strings.Contains(err.Error(), strconv.Itoa(requestMetadataMaxElements)) {
					t.Fatalf("stop overflow error = %v, want the bounded stop wording", err)
				}
			}
			if rec.ReqStop != tc.wantStop {
				t.Fatalf("ReqStop = %d, want %d", rec.ReqStop, tc.wantStop)
			}
		})
	}
}

func TestRequestStopOverflowReturns400BeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	body := amplificationStopBody(requestMetadataMaxElements + 1)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("invalid_request_error")) || !bytes.Contains(raw, []byte("stop")) {
		t.Fatalf("body = %q, want the shared bounded invalid_request_error", raw)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times, want 0", got)
	}
}

func TestRequestStopArrayAllocationBound(t *testing.T) {
	const entries = 4_000_000
	raw := amplificationStopBody(entries)
	var parseErr error
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			var rec metrics.Record
			parseErr = parseLLMRequest(raw, &rec, false)
			if parseErr == nil {
				b.Fatalf("%d-entry stop array was accepted", entries)
			}
		}
	})
	if alloc := res.AllocedBytesPerOp(); alloc > 4<<20 {
		t.Fatalf("%d-byte %d-entry stop array allocated %d bytes/op, want the bounded preflight", len(raw), entries, alloc)
	}
	t.Logf("stop over-bound: input=%d entries=%d alloc=%d", len(raw), entries, res.AllocedBytesPerOp())
}

func TestRequestTopLevelMemberBound(t *testing.T) {
	atCap := amplificationChatBody(amplificationMembers(0, requestMetadataMaxElements-2))
	over := amplificationChatBody(amplificationMembers(0, requestMetadataMaxElements-1))
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"distinct members at cap", atCap, false},
		{"distinct members over cap", over, true},
		{"duplicate members stay accepted", `{"model":"m",` + strings.Repeat(`"k":0,`, 20_000) + `"tail":0}`, false},
		{"escaped duplicate counts once", `{"model":"m","a":0,"\u0061":0,` + amplificationMembers(0, requestMetadataMaxElements-2) + `}`, false},
		{"escaped key set over cap", `{"model":"m",` + amplificationMembers(0, requestMetadataMaxElements-1) + `,"\u0062":0}`, true},
		{"last duplicate model wins", `{"model":"a","model":"b"}`, false},
		{"malformed over-cap prefix stays passthrough", `{"model":"m",` + amplificationMembers(0, requestMetadataMaxElements-1) + `,X}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			err := parseLLMRequest([]byte(tc.body), &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "top-level members") {
				t.Fatalf("member overflow error = %v, want top-level member wording", err)
			}
			if tc.name == "last duplicate model wins" && rec.Model != "b" {
				t.Fatalf("Model = %q, want last duplicate value b", rec.Model)
			}
		})
	}
}

// TestRequestTopLevelMemberAllocationBound keeps the member preflight lexical:
// a million distinct keys must be rejected without materializing the decoded
// raw-value map the rewrite engine would otherwise build.
func TestRequestTopLevelMemberAllocationBound(t *testing.T) {
	const members = 1_000_000
	raw := []byte(amplificationChatBody(amplificationMembers(0, members)))
	var parseErr error
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			var rec metrics.Record
			parseErr = parseLLMRequest(raw, &rec, false)
			if parseErr == nil {
				b.Fatalf("%d-key body was accepted", members)
			}
		}
	})
	if alloc := res.AllocedBytesPerOp(); alloc > 8<<20 {
		t.Fatalf("%d-byte %d-key body allocated %d bytes/op, want the bounded lexical preflight", len(raw), members, alloc)
	}
	t.Logf("top-level member over-bound: input=%d members=%d alloc=%d", len(raw), members, res.AllocedBytesPerOp())
}

// TestRequestEscapedTopLevelKeyDecodesOnce pins the single decode per member.
// An escaped or non-ASCII top-level key used to be decoded once for the
// distinct-key set and again for every candidate name, so one member paid six
// decodes. The lexical preflight must decode the key once and reuse the result
// for the distinct count and every field comparison.
func TestRequestEscapedTopLevelKeyDecodesOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"escaped ASCII", `"\u006dodel"`},
		{"escaped non-ASCII", `"m\u00f6del"`},
		{"raw non-ASCII", `"mödel"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{` + tc.key + `:"m","messages":[{"role":"user","content":"hi"}],"metadata":{}}`)
			var rec metrics.Record
			if err := parseLLMRequest(body, &rec, false); err != nil {
				t.Fatal(err)
			}
			if tc.name == "escaped ASCII" && rec.Model != "m" {
				t.Fatalf("Model = %q, want the escaped key's value m", rec.Model)
			}
			var decodeOK bool
			decodeAllocs := testing.AllocsPerRun(100, func() {
				_, decodeOK = decodeRequestKey([]byte(tc.key))
			})
			if !decodeOK {
				t.Fatal("decodeRequestKey rejected the fixture key")
			}
			var runErr error
			allocs := testing.AllocsPerRun(100, func() {
				runErr = boundRequestMetadata(body)
			})
			if runErr != nil {
				t.Fatalf("boundRequestMetadata = %v", runErr)
			}
			// The preflight adds only the distinct-key map to the member's one
			// decode. A second decode per candidate name (the former shape)
			// adds several more json.Unmarshal allocations per candidate.
			// AllocsPerRun can report one extra allocation under CI scheduling
			// and GC, so allow a small fixed slack; the former per-candidate
			// decode shape still exceeds it by several allocations.
			if allocs > decodeAllocs+5 {
				t.Fatalf("%s key preflight allocated %.1f objects/op, want one decode (%.1f) plus the bounded key set",
					tc.name, allocs, decodeAllocs)
			}
			t.Logf("%s: %.1f allocs/op (one decode %.1f)", tc.name, allocs, decodeAllocs)
		})
	}
}

// amplificationOverrideProxy starts a proxy with one body-rewriting override,
// the example-config shape that reaches overrideRequestBody.
func amplificationOverrideProxy(t *testing.T) *httptest.Server {
	t.Helper()
	tokens := 1234
	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Client: "ov-client",
		Body:   &config.OverrideBody{MaxTokens: &tokens},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(New(cfg, metrics.NewBuffer(10)))
}

func amplificationOverridePost(t *testing.T, srvURL, upstreamURL, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("X-Proxy-Client", "ov-client")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestRequestTopLevelMembersReturn400BeforeRewriteAndUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	srv := amplificationOverrideProxy(t)
	defer srv.Close()

	// A million distinct keys would otherwise decode into the rewrite
	// engine's map and re-marshal before the upstream call.
	over := amplificationChatBody(amplificationMembers(0, 1_000_000))
	status, respBody := amplificationOverridePost(t, srv.URL, upstream.URL, over)
	if status != http.StatusBadRequest {
		t.Fatalf("million-key status = %d, want 400 (body: %s)", status, respBody)
	}
	if !strings.Contains(respBody, "invalid_request_error") || !strings.Contains(respBody, "top-level members") {
		t.Fatalf("million-key body = %q, want the shared bounded invalid_request_error", respBody)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times for the over-bound body, want 0", got)
	}

	// The 10k accepted control keeps the rewrite working end to end.
	atCap := amplificationChatBody(amplificationMembers(0, requestMetadataMaxElements-2))
	status, respBody = amplificationOverridePost(t, srv.URL, upstream.URL, atCap)
	if status != http.StatusOK {
		t.Fatalf("10k-key status = %d, want 200 (body: %s)", status, respBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 after the accepted control", got)
	}
}

func TestRequestAggregateContentPartsBound(t *testing.T) {
	parts := func(n int) string { return amplificationArray(n, "{}") }
	message := func(content string) string { return `{"role":"user","content":` + content + `}` }
	atCap := `{"model":"m","messages":[` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `]}`
	overCap := `{"model":"m","messages":[` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(metrics.ContentPartMax)) + `,` +
		message(parts(1)) + `]}`
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"normal multimodal accepted", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`, false},
		{"per-array over per-message cap still accepted", `{"model":"m","messages":[` + message(parts(metrics.ContentPartMax+1)) + `]}`, false},
		{"aggregate at cap accepted", atCap, false},
		{"aggregate over cap rejected", overCap, true},
		{"aggregate across many messages rejected", `{"model":"m","messages":[` + strings.TrimSuffix(strings.Repeat(message(parts(100))+",", 300), ",") + `]}`, true},
		{"top-level system counts", `{"model":"m","messages":[` + message(parts(metrics.ContentPartMax)) + `],"system":` + parts(requestContentPartsMax-metrics.ContentPartMax+1) + `}`, true},
		{"top-level system accepted at cap", `{"model":"m","messages":[` + message(parts(metrics.ContentPartMax)) + `],"system":` + parts(requestContentPartsMax-metrics.ContentPartMax) + `}`, false},
		{"text length alone is not rejected", `{"model":"m","messages":[` + message(`"`+strings.Repeat("x", 2<<20)+`"`) + `]}`, false},
		{"malformed over-budget prefix stays passthrough", overCap[:len(overCap)-1] + `,X}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			err := parseLLMRequest([]byte(tc.body), &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "content parts") {
				t.Fatalf("content-part overflow error = %v, want the aggregate wording", err)
			}
			if tc.name == "normal multimodal accepted" && rec.Images != 1 {
				t.Fatalf("Images = %d, want the normal multimodal control preserved", rec.Images)
			}
		})
	}
}

// TestRequestAggregateContentPartsAllocationBound pins the many-message shape:
// 2,554 messages at the per-message 4,096-part cap previously composed a
// request-wide typed slice of over ten million empty parts. The aggregate
// preflight stops after the budget is crossed, so neither the metadata decode
// nor the requestContent decoder expands the body.
func TestRequestAggregateContentPartsAllocationBound(t *testing.T) {
	const messages = 2_554
	raw := amplificationAggregateBody(messages, metrics.ContentPartMax)
	var parseErr error
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			var rec metrics.Record
			parseErr = parseLLMRequest(raw, &rec, false)
			if parseErr == nil {
				b.Fatalf("%d-message aggregate body was accepted", messages)
			}
		}
	})
	if alloc := res.AllocedBytesPerOp(); alloc > 8<<20 {
		t.Fatalf("%d-byte %d-message aggregate body allocated %d bytes/op, want the aggregate preflight", len(raw), messages, alloc)
	}
	t.Logf("aggregate content over-bound: input=%d messages=%d parts=%d alloc=%d",
		len(raw), messages, messages*metrics.ContentPartMax, res.AllocedBytesPerOp())
}

// duplicateContentBody renders one message whose object repeats the content
// member occurrences times. Duplicate members are legal JSON and encoding/json
// invokes requestContent.UnmarshalJSON once per occurrence before the last
// value wins, so the aggregate guardrail must charge every occurrence's part
// array, not only the retained one.
func duplicateContentBody(occurrences, parts int) []byte {
	content := amplificationArray(parts, "{}")
	var b strings.Builder
	b.Grow(occurrences * (len(content) + 12))
	b.WriteString(`{"model":"m","messages":[{"role":"user"`)
	for i := 0; i < occurrences; i++ {
		b.WriteString(`,"content":`)
		b.WriteString(content)
	}
	b.WriteString(`}]}`)
	return []byte(b.String())
}

// TestRequestDuplicateContentPartsBound pins the duplicate-member bypass of
// the aggregate content-part budget: five 4,096-part content arrays in one
// message decode 20,480 parts through five UnmarshalJSON calls while the
// last-wins counter charged only 4,096.
func TestRequestDuplicateContentPartsBound(t *testing.T) {
	raw := duplicateContentBody(5, metrics.ContentPartMax)
	var rec metrics.Record
	err := parseLLMRequest(raw, &rec, false)
	if err == nil || !strings.Contains(err.Error(), "content parts") {
		t.Fatalf("parseLLMRequest error = %v, want the aggregate content-part overflow for five duplicate content arrays", err)
	}

	// The counter must not change the decoder's last-wins semantics: two
	// occurrences inside the aggregate budget are accepted and the final
	// value is the one measured.
	lastWins := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"ignored"}],"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`)
	var okRec metrics.Record
	if err := parseLLMRequest(lastWins, &okRec, false); err != nil {
		t.Fatalf("duplicate content within the aggregate budget = %v, want accepted", err)
	}
	if okRec.Images != 1 || okRec.CharsUser != 0 {
		t.Fatalf("last-wins composition = images %d chars %d, want the final image part alone", okRec.Images, okRec.CharsUser)
	}
}

// TestRequestDuplicateContentPartsAllocationBound proves the rejection costs
// only one lexical prefix scan: the repeated part arrays are never composed
// into typed slices after the aggregate budget is crossed.
func TestRequestDuplicateContentPartsAllocationBound(t *testing.T) {
	const occurrences = 5
	raw := duplicateContentBody(occurrences, metrics.ContentPartMax)
	var parseErr error
	alloc := proxyContentAllocDelta(func() {
		var rec metrics.Record
		parseErr = parseLLMRequest(raw, &rec, false)
	})
	t.Logf("duplicate content over-bound: input=%d occurrences=%d parts=%d alloc=%d",
		len(raw), occurrences, occurrences*metrics.ContentPartMax, alloc)
	if parseErr == nil || !strings.Contains(parseErr.Error(), "content parts") {
		t.Fatalf("parseLLMRequest error = %v, want the aggregate content-part overflow", parseErr)
	}
	if alloc > 1<<20 {
		t.Fatalf("%d-byte duplicate-content body allocated %d bytes, want the lexical preflight", len(raw), alloc)
	}
}

func TestRequestDuplicateContentPartsReturn400BeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	body := duplicateContentBody(5, metrics.ContentPartMax)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("invalid_request_error")) || !bytes.Contains(raw, []byte("content parts")) {
		t.Fatalf("body = %q, want the shared bounded invalid_request_error", raw)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times, want 0", got)
	}
}

func TestRequestAggregateContentPartsReturn400BeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	body := amplificationAggregateBody(5, metrics.ContentPartMax)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("invalid_request_error")) || !bytes.Contains(raw, []byte("content parts")) {
		t.Fatalf("body = %q, want the shared bounded invalid_request_error", raw)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times, want 0", got)
	}
}
