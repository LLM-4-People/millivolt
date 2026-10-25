package proxy

// Round 4 batch 9 regression (E4): after a truncation rescue, a non-SSE 2xx
// retry body is surfaced as one in-band error and never injected into the
// committed event-stream socket.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestStreamingRetryNonSSE200StaysInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"retry answer"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, got)
	}
	if strings.Contains(got, `"c2"`) || strings.Contains(got, "retry answer") || strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("non-SSE retry body was injected into the stream: %s", got)
	}
	if strings.Count(got, `"code":"truncated"`) != 1 {
		t.Fatalf("want exactly one in-band truncated error: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE]: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorCode != metrics.CodeTruncated || rec.ErrorType == "" || !rec.IsError() {
		t.Fatalf("record = type %q code %q error %v, want a truncated failure", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
	if rec.Retries != 1 {
		t.Fatalf("retries = %d, want 1", rec.Retries)
	}
}

func TestStreamingRetryRedirectStaysInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "http://elsewhere.example/steal")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`{"error":{"message":"moved-away","type":"api_error"}}`))
	}))
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, got)
	}
	if strings.Contains(got, "moved-away") || strings.Contains(got, `{"error":{"message":"moved-away"`) {
		t.Fatalf("redirect body was injected into the stream: %s", got)
	}
	if strings.Count(got, `"code":"truncated"`) != 1 {
		t.Fatalf("want exactly one in-band truncated error: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE]: %s", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.StatusCode != http.StatusFound || rec.ErrorType != "upstream_error" ||
		rec.ErrorCode != metrics.CodeTruncated || !rec.IsError() {
		t.Fatalf("record = status %d type %q code %q error %v, want 302/upstream_error/truncated",
			rec.StatusCode, rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
	if rec.Retries != 1 {
		t.Fatalf("retries = %d, want 1", rec.Retries)
	}
}
