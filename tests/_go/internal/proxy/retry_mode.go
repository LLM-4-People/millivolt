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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

type pauseSignalRecorder struct {
	paused chan struct{}
	once   sync.Once
}

func (r *pauseSignalRecorder) Record(*metrics.Record) {}

func (r *pauseSignalRecorder) PublishLive(_ string, rec *metrics.Record) {
	if rec.Paused {
		r.once.Do(func() { close(r.paused) })
	}
}

func TestRetryHoldDoesNotCommitSSEForJSON(t *testing.T) {
	cfg := config.Default()
	cfg.QualityRetries = 1
	recorder := &pauseSignalRecorder{paused: make(chan struct{})}
	var calls atomic.Int32
	var s *Server
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() == 1 {
			// The empty completion spends the quality retry. The indefinite
			// hold parks its opening WaitSend until the test observes OnHold,
			// which fires after OnWait (the pacer Start this test pins).
			if err := s.scheduler.AddHold(scheduler.Hold{ID: "r5e1", All: true}); err != nil {
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
	s = New(cfg, recorder)
	defer s.scheduler.RemoveHold("r5e1")
	srv := httptest.NewServer(s)
	defer srv.Close()

	trigger := make(chan struct{})
	type result struct {
		contentType string
		body        string
		err         error
	}
	resultC := make(chan result, 1)
	go func() {
		close(trigger)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", up.URL)
		client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
		resp, err := client.Do(req)
		if err != nil {
			resultC <- result{err: err}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resultC <- result{contentType: resp.Header.Get("Content-Type"), body: string(body)}
	}()
	<-trigger

	select {
	case <-recorder.paused:
	case <-time.After(5 * time.Second):
		t.Fatal("retry never parked in the hold")
	}
	s.scheduler.RemoveHold("r5e1")
	res := <-resultC
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json (body %q)", res.contentType, res.body)
	}
	if strings.Contains(res.body, ": keepalive") {
		t.Fatalf("JSON body carried an SSE keepalive: %q", res.body)
	}
	if !strings.Contains(res.body, `"content":"hello"`) {
		t.Fatalf("body is not the adopted JSON completion: %q", res.body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
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
