package proxy

// Round 3 batch 18 regression (G2): an error body that drips is read under the
// shared whole-document bound before the translated owner re-reads it, so the
// handler fails closed instead of waiting on the per-read idle bound.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslatedSlowErrorBodyBounded(t *testing.T) {
	stop := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
			}
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(func() {
		close(stop)
		upstream.Close()
	})

	cfg := config.Default()
	cfg.WholeDocumentReadTimeout = 50 * time.Millisecond
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

	start := time.Now()
	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("dripping error body held the handler for %s", elapsed)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
	if w.Body.Len() > 64*1024 {
		t.Fatalf("error body grew past the capture bound: %d bytes", w.Body.Len())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 || recs[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("record = %+v, want status 502", recs)
	}
}
