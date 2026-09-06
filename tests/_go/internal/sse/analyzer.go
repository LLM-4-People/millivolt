package sse

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestFeedHappyPathZeroAlloc(t *testing.T) {
	// The common content chunk (the SSE hot path) must not allocate. Warm up
	// once so the Analyzer holds its one-time state (model, reqID, preview),
	// then measure a steady-state content line.
	var a Analyzer
	now := time.Now()
	a.Feed([]byte(`data: {"id":"1","model":"gpt-4","choices":[{"delta":{"content":"hello world"}}]}`), now)

	line := []byte(`data: {"id":"1","model":"gpt-4","choices":[{"delta":{"content":"hello world"}}]}`)
	allocs := testing.AllocsPerRun(200, func() {
		a.Feed(line, now)
	})
	if allocs != 0 {
		t.Errorf("Feed(content chunk) allocated %v times, want 0", allocs)
	}
}

func TestAnalyzerTokenTiming(t *testing.T) {
	var a Analyzer
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// A content-bearing delta should register.
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`), now)
	if a.firstTokenAt != now {
		t.Errorf("firstTokenAt = %v, want %v", a.firstTokenAt, now)
	}
	if a.lastTokenAt != now {
		t.Errorf("lastTokenAt = %v, want %v", a.lastTokenAt, now)
	}
	now2 := now.Add(time.Second)
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"there"}}]}`), now2)
	if a.firstTokenAt != now {
		t.Errorf("firstTokenAt shifted: %v", a.firstTokenAt)
	}
	if a.lastTokenAt != now2 {
		t.Errorf("lastTokenAt = %v, want %v", a.lastTokenAt, now2)
	}
	if a.chunkCount != 2 {
		t.Errorf("chunkCount = %d, want 2", a.chunkCount)
	}
}

// TTFT must fire on the FIRST token of ANY kind - reasoning or answer - across
// providers' many reasoning field names. A reasoning-only stream must set
// firstTokenAt (TTFT) at the first reasoning chunk, not wait for answer content.
func TestReasoningMarksTTFT(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		payload string
		want    bool // should this chunk mark a token (TTFT)?
	}{
		{"deepseek reasoning_content", `{"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`, true},
		{"openrouter bare reasoning", `{"choices":[{"delta":{"reasoning":"let me think"}}]}`, true},
		{"openrouter reasoning_details", `{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"x"}]}}]}`, true},
		{"vllm reasoning", `{"choices":[{"delta":{"reasoning":"step 1"}}]}`, true},
		{"anthropic thinking_delta", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`, true},
		{"gemini thought part", `{"candidates":[{"content":{"parts":[{"text":"think","thought":true}]}}]}`, true},
		{"responses reasoning_text", `{"type":"response.reasoning_text.delta","delta":"pondering"}`, true},
		// Must NOT mark TTFT:
		{"empty reasoning_content (role chunk)", `{"choices":[{"delta":{"role":"assistant","reasoning_content":""}}]}`, false},
		{"null reasoning_content", `{"choices":[{"delta":{"reasoning_content":null}}]}`, false},
		{"reasoning_effort config echo", `{"choices":[{"delta":{"content":"hi"}}],"reasoning_effort":"high"}`, true}, // content present → true, but effort alone below:
		{"reasoning_effort only, no token", `{"choices":[{"delta":{"role":"assistant","content":""}}],"reasoning_effort":"high"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.Feed([]byte("data: "+tc.payload), now)
			got := !a.firstTokenAt.IsZero()
			if got != tc.want {
				t.Errorf("firstTokenAt set = %v, want %v", got, tc.want)
			}
		})
	}
}

// Whitespace after the colon (a re-serializing gateway emits `"content": "hi"`)
// must still mark TTFT and must NOT false-fire on an empty value. Regression
// test for the byte-scan missing space-after-colon payloads.
func TestWhitespaceAfterColon(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Space after colon with real content → TTFT marked.
	var a Analyzer
	a.Feed([]byte(`data: {"choices":[{"delta":{"content": "hi"}}]}`), now)
	if a.firstTokenAt.IsZero() {
		t.Error("space-after-colon content chunk did not mark TTFT")
	}

	// Space after colon with EMPTY reasoning → must NOT mark TTFT.
	var b Analyzer
	b.Feed([]byte(`data: {"choices":[{"delta":{"reasoning": ""}}]}`), now)
	if !b.firstTokenAt.IsZero() {
		t.Error("space-after-colon empty reasoning falsely marked TTFT")
	}

	// Space after colon: model/id/finish_reason still extracted.
	var c Analyzer
	c.Feed([]byte(`data: {"id": "req-1", "model": "model-a", "choices":[{"delta":{}}], "finish_reason": "stop"}`), now)
	var rec metrics.Record
	c.Fill(&rec)
	if rec.ProviderModel != "model-a" {
		t.Errorf("model = %q, want gpt-4o (space-after-colon)", rec.ProviderModel)
	}
	if rec.ProviderRequestID != "req-1" {
		t.Errorf("reqID = %q, want req-1 (space-after-colon)", rec.ProviderRequestID)
	}
	if rec.FinishReason != "stop" {
		t.Errorf("finishReason = %q, want stop (space-after-colon)", rec.FinishReason)
	}
}

// A reasoning-then-answer stream: TTFT fires at the first reasoning chunk
// (early), and firstAnswerAt fires later at the first content chunk.
func TestReasoningThenAnswerTTFT(t *testing.T) {
	var a Analyzer
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tReason := t0.Add(300 * time.Millisecond) // reasoning starts early
	tAnswer := t0.Add(5 * time.Second)        // answer starts much later
	a.Feed([]byte(`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":""}}]}`), t0)
	a.Feed([]byte(`data: {"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`), tReason)
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"the answer"}}]}`), tAnswer)
	if !a.firstTokenAt.Equal(tReason) {
		t.Errorf("firstTokenAt (TTFT) = %v, want %v (first reasoning token, not answer)", a.firstTokenAt, tReason)
	}
	if !a.firstAnswerAt.Equal(tAnswer) {
		t.Errorf("firstAnswerAt = %v, want %v", a.firstAnswerAt, tAnswer)
	}
}

func TestAnalyzerIgnoreEmptyContent(t *testing.T) {
	var a Analyzer
	now := time.Now()

	// Empty content or role-only chunks should not count as tokens.
	a.Feed([]byte(`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`), now)
	if a.chunkCount != 0 {
		t.Errorf("chunkCount = %d, want 0 for empty content", a.chunkCount)
	}
}

// A short or garbled line that merely starts with 'd' (bare "data", "d",
// "data\r") must be ignored, never panic on the payload slice. Regression test
// for the slice-out-of-range panic on short d-prefixed lines.
func TestFeedShortDataLineNoPanic(t *testing.T) {
	var a Analyzer
	now := time.Now()
	for _, line := range []string{"data", "d", "da", "data\r", "dat", "data:"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Feed(%q) panicked: %v", line, r)
				}
			}()
			a.Feed([]byte(line), now)
		}()
	}
	// A real "data:" line still parses after the garbage.
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`), now)
	if a.chunkCount != 1 {
		t.Errorf("chunkCount = %d after garbage lines, want 1", a.chunkCount)
	}
}

// The stored response preview must be an owned copy, not a slice aliasing the
// reusable line buffer - otherwise later Feed calls clobber it before Fill.
func TestPreviewDoesNotAliasLineBuffer(t *testing.T) {
	var a Analyzer
	a.CapturePreview = true
	now := time.Now()
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"HELLO-FIRST"}}]}`), now)
	// Grab the stored preview, then simulate the line buffer being reused and
	// overwritten by a later chunk (the proxy reuses one buffer across lines).
	p1 := a.preview
	if p1 == nil {
		t.Fatal("no preview captured")
	}
	before := string(p1)
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"ZZZZZZZZZZZZZZ-second-chunk"}}]}`), now)
	if string(p1) != before {
		t.Errorf("stored preview mutated after later Feed: %q -> %q", before, string(p1))
	}
}

func TestAnalyzerFinishReason(t *testing.T) {
	var a Analyzer

	a.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`), time.Now())
	if a.finishReason != "stop" {
		t.Errorf("finishReason = %q, want stop", a.finishReason)
	}
}

// Response-preview capture is gated on CapturePreview (the capture_body_preview
// config): by default no message text is ever retained; when enabled, a bounded
// rune-safe preview is kept.
func TestPreviewGatedOnCapturePreview(t *testing.T) {
	now := time.Now()
	chunk := `data: {"choices":[{"delta":{"content":"hello world, this is the answer text"}}]}`

	// Default (off): nothing captured, nothing filled.
	var off Analyzer
	off.Feed([]byte(chunk), now)
	var recOff metrics.Record
	off.Fill(&recOff)
	if recOff.ResponsePreview != "" {
		t.Errorf("ResponsePreview = %q with CapturePreview off, want empty", recOff.ResponsePreview)
	}

	// Enabled: a bounded preview is captured.
	var on Analyzer
	on.CapturePreview = true
	on.Feed([]byte(chunk), now)
	var recOn metrics.Record
	on.Fill(&recOn)
	if recOn.ResponsePreview == "" {
		t.Error("ResponsePreview empty with CapturePreview on, want a preview")
	}
	if len(recOn.ResponsePreview) > metrics.PreviewMaxBytes+len("…") {
		t.Errorf("ResponsePreview len %d exceeds bound", len(recOn.ResponsePreview))
	}
}

// Truncation backs off to a rune boundary: a multi-byte character straddling
// the cap is never split (the shared metrics.TruncatePreview contract).
func TestPreviewTruncationRuneSafe(t *testing.T) {
	// Build a content string whose byte at the cap is inside a multi-byte rune.
	prefix := strings.Repeat("a", metrics.PreviewMaxBytes-1)
	content := prefix + "é" + strings.Repeat("b", 10) // é is 2 bytes, straddles the cap
	payload := `data: {"choices":[{"delta":{"content":"` + content + `"}}]}`

	var a Analyzer
	a.CapturePreview = true
	a.Feed([]byte(payload), time.Now())
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ResponsePreview == "" {
		t.Fatal("no preview captured")
	}
	trimmed := strings.TrimSuffix(rec.ResponsePreview, "…")
	if !utf8.ValidString(trimmed) {
		t.Errorf("preview %q is not valid UTF-8 (rune split at the cap)", rec.ResponsePreview)
	}
	if strings.HasSuffix(trimmed, "é") == false && len(trimmed) > metrics.PreviewMaxBytes-1 {
		// The é straddled the cap; it must have been dropped entirely.
		t.Errorf("preview kept a split rune: %q", rec.ResponsePreview)
	}
}

func TestAnalyzerUsage(t *testing.T) {
	var a Analyzer

	line := `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`
	a.Feed([]byte(line), time.Now())

	rec := &metrics.Record{}
	a.Fill(rec)
	if rec.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", rec.Usage.InputTokens)
	}
	if rec.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", rec.Usage.OutputTokens)
	}
	if rec.Usage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", rec.Usage.TotalTokens)
	}
	if rec.Usage.CacheReadTokens != 3 {
		t.Errorf("CacheReadTokens = %d, want 3", rec.Usage.CacheReadTokens)
	}
	if rec.Usage.ReasoningTokens != 2 {
		t.Errorf("ReasoningTokens = %d, want 2", rec.Usage.ReasoningTokens)
	}
}

// Cost key paths are response-root relative. On a stream the root is the
// terminal chunk, so a provider that reports cost in a SIBLING object beside
// usage - or with no usage object at all (nano-gpt: token counts ride the
// x_nanogpt_pricing object on the finish_reason chunk) - is reachable via
// config key maps. The bare usage object stays a fallback root.
func TestAnalyzerCostFromSiblingPricingObject(t *testing.T) {
	// Real nano-gpt stream shape: finish chunk carries x_nanogpt_pricing and
	// NO usage object.
	chunk := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"x_nanogpt_pricing":{"amount":0.000066583125,"currency":"USD","cost":0.000066583125,"inputTokens":90,"outputTokens":16,"cacheReadInputTokens":90,"costUsd":0.000066583125}}`

	// Cost resolves from the sibling object via the response-root path.
	var a Analyzer
	a.CostKeys = []string{"x_nanogpt_pricing.cost"}
	a.Feed([]byte(chunk), time.Now())
	rec := &metrics.Record{}
	a.Fill(rec)
	if rec.Cost != 0.000066583125 {
		t.Errorf("Cost = %v, want 0.000066583125 (sibling pricing object)", rec.Cost)
	}

	// Token counts resolve from the same sibling via usage_keys (streams have
	// no usage object to anchor on).
	a.UsageKeys = map[string]string{
		"input_tokens":      "x_nanogpt_pricing.inputTokens",
		"output_tokens":     "x_nanogpt_pricing.outputTokens",
		"cache_read_tokens": "x_nanogpt_pricing.cacheReadInputTokens",
	}
	rec2 := &metrics.Record{}
	a.Fill(rec2)
	if rec2.Usage.InputTokens != 90 || rec2.Usage.OutputTokens != 16 || rec2.Usage.CacheReadTokens != 90 {
		t.Errorf("Usage = %+v, want in=90 out=16 cache_read=90 (pricing object)", rec2.Usage)
	}

	// Usage-object-rooted cost paths keep working through the fallback.
	var b Analyzer
	b.CostKeys = []string{"cost"}
	b.Feed([]byte(`data: {"choices":[],"usage":{"prompt_tokens":1,"cost":0.25}}`), time.Now())
	rec3 := &metrics.Record{}
	b.Fill(rec3)
	if rec3.Cost != 0.25 {
		t.Errorf("Cost = %v, want 0.25 (usage-root fallback)", rec3.Cost)
	}

	// No cost keys → no fabricated cost.
	var c Analyzer
	c.Feed([]byte(chunk), time.Now())
	rec4 := &metrics.Record{}
	c.Fill(rec4)
	if rec4.Cost != 0 {
		t.Errorf("Cost = %v, want 0 (no matching key)", rec4.Cost)
	}
}

func TestAnalyzerToolCalls(t *testing.T) {
	var a Analyzer

	// First delta: has the tool call id.
	a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"search","arguments":""}}]}}]}`), time.Now())
	// Argument chunk: no id, should not be counted again.
	a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}}]}`), time.Now())
	// Second tool call: new id.
	a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_def456","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`), time.Now())

	rec := &metrics.Record{}
	a.Fill(rec)
	if rec.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", rec.ToolCalls)
	}
}

func TestAnalyzerDONE(t *testing.T) {
	var a Analyzer
	// [DONE] should be a no-op, not crash or affect token state.
	a.Feed([]byte(`data: [DONE]`), time.Now())
	if a.chunkCount != 0 {
		t.Errorf("chunkCount = %d after DONE, want 0", a.chunkCount)
	}
}

func TestAnalyzerBlankLines(t *testing.T) {
	var a Analyzer
	a.Feed(nil, time.Now())
	a.Feed([]byte{}, time.Now())
	a.Feed([]byte("\r"), time.Now())
	if a.chunkCount != 0 {
		t.Errorf("chunkCount changed on blank/comment lines")
	}
}

func TestAnalyzerFillZeroTokens(t *testing.T) {
	rec := &metrics.Record{}
	var a Analyzer
	// Feed a content-bearing token but no usage chunk.
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}`), time.Now())
	a.Fill(rec)
	// Fallback: chunk count used when usage not present.
	if rec.Usage.OutputTokens != 1 {
		t.Errorf("OutputTokens fallback = %d, want 1", rec.Usage.OutputTokens)
	}
}

func TestKeyIndex(t *testing.T) {
	tests := []struct {
		name, haystack, needle string
		want                   int
	}{
		{"present", `{"content":"hello"}`, `"content":`, 1},
		{"absent", `{"data":"x"}`, `"content":`, -1},
		{"nested", `{"delta":{"content":"hi"}}`, `"content":`, 10},
	}
	for _, tt := range tests {
		got := bytes.Index([]byte(tt.haystack), []byte(tt.needle))
		if got != tt.want {
			t.Errorf("%s: bytes.Index(%q, %q) = %d, want %d", tt.name, tt.haystack, tt.needle, got, tt.want)
		}
	}
}

func TestAnalyzerDoneNoSpace(t *testing.T) {
	// "data:[DONE]" (no space) must be recognized as the sentinel, not fall
	// through to the JSON scan. Include decoy keys that would be captured if
	// the line were parsed as JSON.
	var a Analyzer
	a.Feed([]byte(`data:[DONE]`), time.Now())
	if a.chunkCount != 0 {
		t.Errorf("chunkCount = %d after data:[DONE], want 0", a.chunkCount)
	}
	if a.usage != nil {
		t.Errorf("usage = %q after data:[DONE], want nil", a.usage)
	}
}

func TestAnalyzerDoneCR(t *testing.T) {
	var a Analyzer
	a.Feed([]byte("data: [DONE]\r"), time.Now())
	a.Feed([]byte("data:[DONE]\r"), time.Now())
	if a.chunkCount != 0 {
		t.Errorf("chunkCount = %d after [DONE]\\r, want 0", a.chunkCount)
	}
}

func TestExtractObjectBraceInString(t *testing.T) {
	// A '}' inside a string value must not close the object.
	raw := []byte(`{"a":"}","b":1}`)
	got := extractObject(raw)
	if string(got) != string(raw) {
		t.Errorf("extractObject = %q, want %q", got, raw)
	}
}

func TestExtractObjectEscapedQuoteAndBrace(t *testing.T) {
	// Value ends with an escaped backslash: "\\"} - the quote after two
	// backslashes closes the string, so the following '}' closes the object.
	raw := []byte(`{"a":"\\"}tail`)
	want := `{"a":"\\"}`
	got := extractObject(raw)
	if string(got) != want {
		t.Errorf("extractObject = %q, want %q", got, want)
	}
}

func TestExtractObjectNull(t *testing.T) {
	// "usage":null - input doesn't start with '{', must return nil (never the
	// raw non-object bytes).
	if got := extractObject([]byte(`null}`)); got != nil {
		t.Errorf("extractObject(null}) = %q, want nil", got)
	}
	if got := extractObject(nil); got != nil {
		t.Errorf("extractObject(nil) = %q, want nil", got)
	}
}

func TestExtractObjectNested(t *testing.T) {
	raw := []byte(`{"u":{"a":1},"b":[{}]}extra`)
	want := `{"u":{"a":1},"b":[{}]}`
	if got := extractObject(raw); string(got) != want {
		t.Errorf("extractObject = %q, want %q", got, want)
	}
}

func TestExtractPreviewEscapedBackslash(t *testing.T) {
	// JSON "foo\\" - value is foo\. The closing quote is preceded by two
	// backslashes (even), so it terminates the string; the preview must not
	// swallow the bytes after it.
	payload := []byte(`{"content":"foo\\","x":1}`)
	got := extractPreview(payload)
	if string(got) != `foo\` {
		t.Errorf("extractPreview = %q, want %q", got, `foo\`)
	}
}

func TestExtractPreviewEscapedQuote(t *testing.T) {
	// JSON "a\"b" - the quote after one backslash (odd) is escaped, so the
	// string continues to the real closing quote.
	payload := []byte(`{"content":"a\"b","x":1}`)
	got := extractPreview(payload)
	if string(got) != `a"b` {
		t.Errorf("extractPreview = %q, want %q", got, `a"b`)
	}
}

func TestExtractPreviewUTF8Boundary(t *testing.T) {
	// A multi-byte rune straddling the 120-byte cut must not be split: back
	// off to the last rune boundary, then append the ellipsis.
	val := append(bytes.Repeat([]byte("x"), 119), []byte("é ZZZ")...) // é starts at byte 119
	payload := append([]byte(`{"content":"`), val...)
	payload = append(payload, []byte(`"}`)...)
	got := extractPreview(payload)
	if !utf8.Valid(got) {
		t.Errorf("extractPreview = invalid UTF-8: %q", got)
	}
	want := bytes.Repeat([]byte("x"), 119)
	want = append(want, []byte("…")...)
	if !bytes.Equal(got, want) {
		t.Errorf("extractPreview = %q..., want 119 x's + ellipsis (len %d, want %d)", got[:min(20, len(got))], len(got), len(want))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// In-band error payloads: providers and gateways signal failures inside a
// 200-OK stream under a top-level "error" key. The client receives such a
// chunk verbatim, so the record must carry the failure - a request the client
// app displayed as "provider overloaded" must not show up as a clean empty
// 200 on the dashboard. Regression test for a live gateway-overload incident.
func TestAnalyzerInBandError(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		payload  string
		wantType string
		wantCode string
		wantMsg  string
	}{
		{"openai envelope", `{"error":{"message":"provider overloaded","type":"server_error","code":"overloaded"}}`, "server_error", "overloaded", "provider overloaded"},
		{"anthropic shape", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "overloaded_error", "", "Overloaded"},
		{"reserialized whitespace", `{"error":  {"message":  "boom" , "type" : "x" }}`, "x", "", "boom"},
		{"openrouter numeric code", `{"error":{"message":"rate limited","code":429}}`, "provider_error", "429", "rate limited"},
		{"string form", `{"error":"engine overloaded"}`, "provider_error", "", "engine overloaded"},
		{"type-only", `{"error":{"type":"server_error"}}`, "server_error", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Analyzer
			a.Feed([]byte("data: "+c.payload), now)
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ErrorType != c.wantType {
				t.Errorf("ErrorType = %q, want %q", rec.ErrorType, c.wantType)
			}
			if rec.ErrorCode != c.wantCode {
				t.Errorf("ErrorCode = %q, want %q", rec.ErrorCode, c.wantCode)
			}
			if rec.ErrorMsg != c.wantMsg {
				t.Errorf("ErrorMsg = %q, want %q", rec.ErrorMsg, c.wantMsg)
			}
			if !rec.IsError() {
				t.Error("record must be flagged as an error")
			}
		})
	}
}

// Decoy frames must never mark an error: null/empty error values, an "error"
// key nested in a normal chunk, or the literal text escaped inside content.
func TestAnalyzerInBandErrorNoFalsePositive(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	frames := []string{
		`{"choices":[{"delta":{"content":"hi"}}],"error":null}`,
		`{"choices":[{"delta":{"content":"hi"}}],"error":{}}`,
		`{"choices":[{"delta":{"content":"hi"}}],"error":""}`,
		`{"choices":[{"delta":{"content":"hi"}}],"usage":{"error":null}}`,
		`{"choices":[{"delta":{"content":"the key is \"error\": yes"}}]}`,
	}
	for _, f := range frames {
		var a Analyzer
		a.Feed([]byte("data: "+f), now)
		var rec metrics.Record
		a.Fill(&rec)
		if rec.ErrorType != "" || rec.IsError() {
			t.Errorf("frame %q wrongly flagged an error: type=%q", f, rec.ErrorType)
		}
	}
}

// An in-band error precedes the connection teardown the proxy records as
// stream_read_error; the provider's own payload is the better classification
// and must win in Fill.
func TestAnalyzerInBandErrorBeatsStreamReadError(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Analyzer
	a.Feed([]byte(`data: {"error":{"message":"provider overloaded","type":"server_error"}}`), now)
	rec := metrics.Record{ErrorType: "stream_read_error"}
	a.Fill(&rec)
	if rec.ErrorType != "server_error" {
		t.Errorf("ErrorType = %q, want in-band %q to win", rec.ErrorType, "server_error")
	}
}

// The error message is bounded like every stored preview.
func TestAnalyzerInBandErrorMessageBounded(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 1000)
	var a Analyzer
	a.Feed([]byte(`data: {"error":{"message":"`+big+`"}}`), now)
	var rec metrics.Record
	a.Fill(&rec)
	if len(rec.ErrorMsg) > metrics.PreviewMaxBytes+len("…") {
		t.Errorf("ErrorMsg length %d exceeds bound", len(rec.ErrorMsg))
	}
	if rec.ErrorType != "provider_error" {
		t.Errorf("ErrorType = %q, want provider_error", rec.ErrorType)
	}
}

// Live regression: coralbricks streamed its overload error in a form the
// client (an SSE parser) reassembled and displayed, while the proxy's per-line
// analyzer recorded a clean, empty 200. The client received
// {"error":{"message":"Coral Bricks is temporarily unavailable. Please
// retry.","type":"api_error","code":"internal_error"}} - proven by the client's
// own session store - yet the dashboard showed nothing. The error event must be
// caught in every wire shape an SSE client can reassemble: split across
// multiple data: lines (pretty-printed), with whitespace before the colon, and
// as a trailing event with no blank-line terminator (stream ends at EOF).
func TestAnalyzerInBandErrorEventBoundary(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	msg := "Coral Bricks is temporarily unavailable. Please retry."
	inner := `{"message":"` + msg + `","type":"api_error","code":"internal_error"}`

	caught := map[string][]string{
		"single-line baseline": {"data: {\"error\":" + inner + "}", ""},
		"pretty-printed across data: lines": {
			"data: {",
			`data:  "error": {`,
			`data:    "message": "` + msg + `",`,
			`data:    "type": "api_error",`,
			`data:    "code": "internal_error"`,
			"data:  }",
			"data: }",
			"",
		},
		"whitespace before colon": {"data: {\"error\" : " + inner + "}", ""},
		"trailing event, EOF before blank line": {
			"data: {\"error\":" + inner + "}", // Fill must flush the pending event
		},
	}
	for name, lines := range caught {
		t.Run(name, func(t *testing.T) {
			var a Analyzer
			for _, l := range lines {
				a.Feed([]byte(l), now)
			}
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ErrorType != "api_error" || rec.ErrorCode != "internal_error" {
				t.Errorf("ErrorType/Code = %q/%q, want api_error/internal_error", rec.ErrorType, rec.ErrorCode)
			}
			if !rec.IsError() {
				t.Error("record must be flagged as an error")
			}
		})
	}

	// Multi-line accumulation must not turn benign frames into failures: a
	// null/empty error value or a nested error key stays a decoy, even reassembled.
	decoys := map[string][]string{
		"null error across lines": {"data: {", `data:  "choices":[{"delta":{"content":"hi"}}],`, `data:  "error": null`, "data: }", ""},
		"nested error key":        {"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"error\":null}}", ""},
		"error null after chunk":  {"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}", "", "data: {\"error\":null}", ""},
	}
	for name, lines := range decoys {
		t.Run("decoy/"+name, func(t *testing.T) {
			var a Analyzer
			for _, l := range lines {
				a.Feed([]byte(l), now)
			}
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ErrorType != "" || rec.IsError() {
				t.Errorf("decoy wrongly flagged: type=%q", rec.ErrorType)
			}
		})
	}
}

// GenTokens (the gen-TPS numerator) must exclude tool-call argument chunks.
// When the provider sends no usage blob, the analyzer's chunk count is the
// token fallback - but tool-arg chunks are not generation (reasoning or answer
// content), so they must not inflate the rate. Regression: a tool-call-heavy
// stream read an inflated gen_tps.
func TestAnalyzerGenTokensExcludesToolChunks(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Analyzer
	lines := []string{
		// 2 reasoning chunks (generation).
		`data: {"choices":[{"delta":{"reasoning_content":"thinking "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"more"}}]}`,
		// 1 answer content chunk (generation).
		`data: {"choices":[{"delta":{"content":"The answer"}}]}`,
		// 3 tool-call argument chunks (NOT generation).
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"ation\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"Boston\"}"}}]}}]}`,
		// finish, no usage blob.
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	for _, l := range lines {
		a.Feed([]byte(l), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	// chunkCount = 6 (all content-bearing), toolChunks = 3 → GenTokens = 3
	// (2 reasoning + 1 answer). OutputTokens fallback stays 6 (total chunks).
	if rec.GenTokens != 3 {
		t.Errorf("GenTokens = %d, want 3 (2 reasoning + 1 answer; 3 tool-arg chunks excluded)", rec.GenTokens)
	}
	if rec.Usage.OutputTokens != 6 {
		t.Errorf("OutputTokens = %d, want 6 (chunk fallback counts all content-bearing chunks)", rec.Usage.OutputTokens)
	}
	if rec.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", rec.ToolCalls)
	}
}

// When the provider DOES send a usage blob, the analyzer leaves GenTokens 0 so
// FinalizeRecord derives it from the authoritative usage (reasoning + answer);
// the chunk count never overrides the provider's numbers.
func TestAnalyzerGenTokensDefersToUsage(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Analyzer
	lines := []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`,
		`data: [DONE]`,
	}
	for _, l := range lines {
		a.Feed([]byte(l), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.GenTokens != 0 {
		t.Errorf("GenTokens = %d, want 0 pre-FinalizeRecord (usage present; FinalizeRecord derives it)", rec.GenTokens)
	}
	if rec.Usage.OutputTokens != 9 {
		t.Errorf("OutputTokens = %d, want 9 (from usage, not chunk count)", rec.Usage.OutputTokens)
	}
}
