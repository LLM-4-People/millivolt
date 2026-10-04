package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestGzipUpstream verifies that when the upstream sends gzip-encoded SSE, the
// proxy decompresses it for metrics and relays readable bytes to the client.
// It also pins the wire-size trio for a gzip'd upstream: request_bytes is the
// sent body length, response_bytes is the decompressed SSE the client
// received, and upstream_gzip carries the transport's auto-decompress fact
// (the stored response headers cannot show it: the transport deletes
// Content-Encoding and Content-Length when it decodes).
func TestGzipUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		for i := 0; i < 5; i++ {
			gz.Write([]byte(`data: {"id":"1","choices":[{"delta":{"content":"tok"}}]}` + "\n\n"))
		}
		gz.Write([]byte(`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		gz.Write([]byte(`data: {"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n"))
		gz.Write([]byte("data: [DONE]\n\n"))
		gz.Close()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	reqBody := `{"model":"m","stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Client should get readable SSE (not gzip bytes).
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("data:")) {
		t.Errorf("client got non-SSE body (gzip not decompressed): %d bytes", len(body))
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		t.Errorf("Content-Encoding gzip was forwarded (client would double-decompress)")
	}

	// Metrics should be captured (usage tokens parsed). Wait for the deferred
	// Record to land instead of assuming EOF implies it already ran.
	snap := waitForRecord(t, buf, 1)
	if len(snap) == 0 {
		t.Fatal("no records")
	}
	rec := snap[0]
	if rec.Usage.OutputTokens == 0 {
		t.Errorf("OutputTokens = 0; gzip body was not analyzed")
	}
	if rec.TTFTMs == 0 {
		t.Errorf("TTFTMs = 0; gzip body was not analyzed")
	}
	if rec.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", rec.FinishReason)
	}
	if rec.RequestBytes != int64(len(reqBody)) {
		t.Errorf("RequestBytes = %d, want %d (the sent request body length)", rec.RequestBytes, len(reqBody))
	}
	if rec.ResponseBytes != int64(len(body)) {
		t.Errorf("ResponseBytes = %d, want %d (the decompressed SSE bytes the client received)", rec.ResponseBytes, len(body))
	}
	if !rec.UpstreamGzip {
		t.Error("UpstreamGzip = false, want true (the transport decoded the gzip body)")
	}
}
