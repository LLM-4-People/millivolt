package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// translatedSSEFixture serves one native Anthropic SSE document and returns
// the mock upstream, the proxy in front of it and the proxy's record buffer.
func translatedSSEFixture(t *testing.T, native string) (*httptest.Server, *httptest.Server, *metrics.Buffer) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, native); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	t.Cleanup(srv.Close)
	return upstream, srv, buf
}

// translatedSSERequest drives the real OpenAI Chat Completions route with an
// Anthropic target and returns the client-visible status and SSE body.
func translatedSSERequest(t *testing.T, srv, upstream *httptest.Server) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	req.Header.Set("X-Proxy-Path", "/messages")
	req.Header.Set("X-Proxy-Format", "anthropic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	return resp.StatusCode, string(body)
}

// translatedSSEPayloads returns the data payloads in wire order. The
// translator emits one data line per frame, so blank-line grouping is not
// needed here.
func translatedSSEPayloads(t *testing.T, body string) []string {
	t.Helper()
	var payloads []string
	for _, line := range strings.Split(body, "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			payloads = append(payloads, payload)
		}
	}
	if len(payloads) == 0 {
		t.Fatalf("translated stream has no data frames: %q", body)
	}
	return payloads
}

func translatedSSEErrorFrames(payloads []string) int {
	n := 0
	for _, payload := range payloads {
		if payload != "[DONE]" && metrics.HasErrorKey([]byte(payload)) {
			n++
		}
	}
	return n
}

// TestTranslatedStreamTranslationErrorClosesInBand pins the committed-200
// failure contract for Anthropic -> OpenAI streaming translation: a native
// duplicate tool index makes StreamToOpenAI abort after the status line is
// already on the wire, so the client must receive exactly one in-band
// upstream_error chunk with the stable translation_error code and then
// [DONE], never an incomplete SSE stream and never a fabricated success.
// The record keeps markStreamErr's existing translation classification.
func TestTranslatedStreamTranslationErrorClosesInBand(t *testing.T) {
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-dup","model":"claude-test","usage":{"input_tokens":4}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-1","name":"lookup"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-2","name":"lookup"}}`,
	}, "\n\n") + "\n\n"
	upstream, srv, buf := translatedSSEFixture(t, native)

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200 (body %s)", status, body)
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 1 {
		t.Fatalf("in-band error frames = %d, want exactly 1 (body %s)", got, body)
	}
	if payloads[len(payloads)-1] != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE] (body %s)", payloads[len(payloads)-1], body)
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payloads[len(payloads)-2]), &envelope); err != nil {
		t.Fatalf("penultimate frame is not the error chunk: %v (%q)", err, payloads[len(payloads)-2])
	}
	if envelope.Error.Type != sse.TypeUpstreamError || envelope.Error.Code != translationErrorCode || envelope.Error.Message != translationErrorMessage {
		t.Fatalf("error chunk = %+v, want %s/%s/%q",
			envelope.Error, sse.TypeUpstreamError, translationErrorCode, translationErrorMessage)
	}
	if strings.Contains(body, "duplicate native tool index") {
		t.Fatalf("translator internals leaked to the client: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("failed translation must not present a successful completion: %s", body)
	}

	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.StatusCode != http.StatusOK || !rec.Stream {
		t.Fatalf("record status/stream = %d/%v, want 200/true", rec.StatusCode, rec.Stream)
	}
	// The synthetic client frame must not relabel the failure as a provider
	// error: markStreamErr's translation classification survives.
	if rec.ErrorType != errStreamRead {
		t.Fatalf("record error type = %q, want %q", rec.ErrorType, errStreamRead)
	}
	if !rec.IsError() || rec.FinishReason != "" || rec.HadAnswerContent {
		t.Fatalf("failed translation classified as success: %+v", rec)
	}
}

// TestTranslatedStreamSuccessClosesWithOneDone is the control: a successful
// translation ends with the translator's own single [DONE] and no synthetic
// error frame.
func TestTranslatedStreamSuccessClosesWithOneDone(t *testing.T) {
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-ok","model":"claude-test","usage":{"input_tokens":4}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	upstream, srv, buf := translatedSSEFixture(t, native)

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 0 {
		t.Fatalf("successful translation emitted %d error frames: %s", got, body)
	}
	if strings.Count(body, "data: [DONE]") != 1 || payloads[len(payloads)-1] != "[DONE]" {
		t.Fatalf("successful translation must end with exactly one [DONE]: %s", body)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	if recs[0].ErrorType != "" || recs[0].FinishReason != "stop" || !recs[0].HadAnswerContent {
		t.Fatalf("successful translation record = %+v", recs[0])
	}
}
