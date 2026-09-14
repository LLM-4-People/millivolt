package proxy

// Pins the upstreamErrorFields default through every surface that renders an
// upstream 4xx/5xx to the client: the cursor bidi rejection, the
// pacer-committed in-band error, and the relay's committed-SSE re-send. The
// trigger in each case is an upstream error with an EMPTY body: nothing is
// captured onto the record, so the client must see the shared owner's default
// (api_error plus "upstream HTTP <status>"), never a surface-local
// hand-rolled string. The cursor rejection additionally records the defaults
// (writeClientError stamps empty record fields); the committed-SSE surfaces
// keep the defaults view-only, with the record holding the decided status
// and empty error fields.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestUpstreamErrorDefaultsSurfaces(t *testing.T) {
	t.Run("cursor-bidi-rejection", func(t *testing.T) {
		// Storm is disabled in Default(), so the cursor send loop surfaces
		// the retryable 500 on the first attempt instead of absorbing it.
		buf := metrics.NewBuffer(10)
		s := New(config.Default(), buf)
		s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			readFrame(t, r.Body)
			return stormCursorReply(500, ""), nil // empty body: unclassified
		})}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`))
		r.Header.Set(hdrBaseURL, "http://neutral.invalid")
		r.Header.Set(hdrFormat, "cursor")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)

		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500 (the cursor rejection keeps the upstream status)", rec.Code)
		}
		want := `{"error":{"message":"upstream HTTP 500","type":"api_error"}}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("body = %s, want %s (upstreamErrorFields default, byte-exact)", got, want)
		}
		got := waitForRecord(t, buf, 1)[0]
		if got.StatusCode != 500 || got.ErrorType != "api_error" || got.ErrorMsg != "upstream HTTP 500" {
			t.Fatalf("record must keep the decided 500, and writeClientError stamps the defaulted fields onto the empty record: %+v", got)
		}
	})

	t.Run("pacer-committed-in-band", func(t *testing.T) {
		// The upstream stalls past the keepalive interval, so the idle pacer
		// commits 200 SSE before the 500 arrives: the error must surface
		// in-band with the default fields, and the record keeps the decided
		// 500. MaxRetries=0 keeps the first 500 terminal.
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			time.Sleep(150 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500) // empty body: unclassified
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.MaxRetries = 0
		cfg.SSEKeepaliveInterval = 20 * time.Millisecond
		buf := metrics.NewBuffer(10)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		status, got := streamRequest(t, srv, upstream)
		if status != 200 {
			t.Fatalf("stream status = %d, want 200 (the pacer already committed; the error must be in-band)", status)
		}
		if !strings.Contains(got, `"api_error"`) || !strings.Contains(got, "upstream HTTP 500") {
			t.Fatalf("in-band error must carry the upstreamErrorFields default: %s", got)
		}
		if strings.Count(got, "data: [DONE]") != 1 {
			t.Fatalf("exactly one [DONE] expected: %s", got)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("upstream calls = %d, want 1", n)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.StatusCode != 500 || rec.ErrorType != "" || rec.ErrorMsg != "" {
			t.Fatalf("record must keep the decided 500 with empty error fields (defaults are view-only): %+v", rec)
		}
	})

	t.Run("relay-committed-resend", func(t *testing.T) {
		// Mirrors the captured-error re-send test, but the re-send returns a
		// 500 with an empty body: the committed status line cannot change,
		// so the default fields must surface in-band.
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				// Truncated-empty stream: absorbed by the quality loop.
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
				w.(http.Flusher).Flush()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500) // empty body: unclassified
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.MaxRetries = 0
		cfg.QualityRetries = 1
		buf := metrics.NewBuffer(10)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		status, got := streamRequest(t, srv, upstream)
		if status != 200 {
			t.Fatalf("stream status = %d, want 200 (the error must be in-band)", status)
		}
		if !strings.Contains(got, `"api_error"`) || !strings.Contains(got, "upstream HTTP 500") {
			t.Fatalf("in-band error must carry the upstreamErrorFields default: %s", got)
		}
		if strings.Count(got, "data: [DONE]") != 1 {
			t.Fatalf("exactly one [DONE] expected: %s", got)
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("upstream calls = %d, want 2 (truncated absorbed + erroring re-send)", n)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.StatusCode != 500 || rec.ErrorType != "" || rec.ErrorMsg != "" {
			t.Fatalf("record must keep the decided 500 with empty error fields (defaults are view-only): %+v", rec)
		}
	})
}
