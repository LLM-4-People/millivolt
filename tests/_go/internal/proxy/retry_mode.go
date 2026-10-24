package proxy

// Round 5 E1 regression: the pacer must never commit SSE after applyUpstream
// proved the body is a buffered non-SSE response. The retry hold calls Start,
// which previously committed text/event-stream and wrote a keepalive; the
// adopted JSON completion then landed under that committed status.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestRetryHoldDoesNotCommitSSEForJSON(t *testing.T) {
	cfg := config.Default()
	cfg.QualityRetries = 1
	var calls int
	var s *Server
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// The empty completion is degenerate and spends the quality
			// retry, whose opening WaitSend honors this hold.
			if err := s.scheduler.AddHold(scheduler.Hold{ID: "r5e1", All: true, Until: time.Now().Add(400 * time.Millisecond)}); err != nil {
				t.Errorf("AddHold: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	s = New(cfg, metrics.Noop{})
	defer s.scheduler.RemoveHold("r5e1")
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", up.URL)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json (body %q)", got, string(body))
	}
	if strings.Contains(string(body), ": keepalive") {
		t.Fatalf("JSON body carried an SSE keepalive: %q", string(body))
	}
	if !strings.Contains(string(body), `"content":"hello"`) {
		t.Fatalf("body is not the adopted JSON completion: %q", string(body))
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestPacerStartHonorsProvenJSONMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		wantComment bool
		wantType    string
	}{
		{name: "json stays buffered", contentType: "application/json", wantType: "application/json"},
		{name: "sse keeps alive", contentType: "text/event-stream", wantComment: true, wantType: "text/event-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			p := newSSEPacer(rec, 10*time.Millisecond, 0)
			p.Arm()
			p.applyUpstream(http.Header{"Content-Type": []string{tc.contentType}})
			p.Start()
			time.Sleep(40 * time.Millisecond)
			p.Stop()
			got := rec.Body.String()
			if strings.Contains(got, ": keepalive") != tc.wantComment {
				t.Fatalf("body %q, want keepalive %v", got, tc.wantComment)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantType {
				t.Fatalf("content type = %q, want %q", got, tc.wantType)
			}
		})
	}
}
