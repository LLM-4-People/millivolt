package proxy

// Round 4 batch 3 regressions (E1, E2, F3): the terminal marker belongs to the
// outbound wire, a zero-event Responses body still closes with one
// response.failed, and an error event split across data lines is classified at
// its boundary instead of being retried as a truncation.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesZeroEventStreamUsesResponsesFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "done-only", body: "data: [DONE]\n\n"},
		{name: "empty", body: ""},
		{name: "comment-only", body: ": keepalive\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.QualityRetries = 0
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.body)
			buf := metrics.NewBuffer(8)
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","stream":true,"input":"hi"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want the committed 200 (body %s)", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if n := strings.Count(body, "event: response.failed"); n != 1 {
				t.Fatalf("response.failed frames = %d, want exactly 1: %q", n, body)
			}
			if strings.Contains(body, "data: [DONE]") || strings.Contains(body, `"choices"`) {
				t.Fatalf("zero-event Responses stream leaked a Chat terminal: %q", body)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			if rec := recs[0]; !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record = type %q code %q error %v, want a truncated failure", rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}

func TestNativeAnthropicDoneSentinelIsNotTerminal(t *testing.T) {
	t.Run("done-only is truncated", func(t *testing.T) {
		_, s, buf := nativeAnthropicStream(t, "data: [DONE]\n\n")
		w := proxyRequest(t, s, "/v1/messages", "anthropic", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 {
			t.Fatalf("records = %d, want 1", len(recs))
		}
		if rec := recs[0]; rec.ErrorCode != metrics.CodeTruncated || rec.ErrorType == "" || !rec.IsError() {
			t.Fatalf("done-only anthropic stream = type %q code %q error %v, want a truncated failure", rec.ErrorType, rec.ErrorCode, rec.IsError())
		}
	})

	t.Run("done then message_stop stays healthy", func(t *testing.T) {
		stream := "data: [DONE]\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		_, s, buf := nativeAnthropicStream(t, stream)
		w := proxyRequest(t, s, "/v1/messages", "anthropic", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "message_stop") {
			t.Fatalf("message_stop after [DONE] was dropped: %q", w.Body.String())
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 {
			t.Fatalf("records = %d, want 1", len(recs))
		}
		if rec := recs[0]; rec.ErrorType != "" || rec.ErrorCode != "" || rec.IsError() {
			t.Fatalf("done-then-stop record = type %q code %q error %v, want healthy", rec.ErrorType, rec.ErrorCode, rec.IsError())
		}
	})
}

func TestResponsesSplitFailedEventRecordsProviderError(t *testing.T) {
	body := "data: {\n" +
		"data:   \"type\": \"response.failed\",\n" +
		"data:   \"response\": {\n" +
		"data:     \"id\": \"resp_1\",\n" +
		"data:     \"status\": \"failed\",\n" +
		"data:     \"error\": {\"type\": \"server_error\", \"message\": \"boom\"}\n" +
		"data:   }\n" +
		"data: }\n\n"
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, body)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","stream":true,"input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("split failed event was not relayed: %q", w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorType != "server_error" || rec.ErrorMsg != "boom" || !rec.IsError() {
		t.Fatalf("record error = type %q code %q msg %q error %v, want server_error/boom", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, rec.IsError())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
}

func TestResponsesSplitStandaloneErrorStopsRetry(t *testing.T) {
	body := "data: {\n" +
		"data:   \"type\": \"error\",\n" +
		"data:   \"code\": \"boom\",\n" +
		"data:   \"message\": \"upstream exploded\"\n" +
		"data: }\n\n"
	cfg := config.Default()
	cfg.QualityRetries = 1
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, body)
	buf := metrics.NewBuffer(8)
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","stream":true,"input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if n := strings.Count(got, "upstream exploded"); n != 1 {
		t.Fatalf("visible error frames = %d, want exactly 1: %q", n, got)
	}
	if strings.Contains(got, metrics.CodeTruncated) {
		t.Fatalf("split error was followed by a synthetic truncation: %q", got)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after a visible error)", up.calls.Load())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorType != "provider_error" || rec.ErrorCode != "boom" || !rec.IsError() {
		t.Fatalf("record error = type %q code %q msg %q error %v, want provider_error/boom", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, rec.IsError())
	}
}
