package proxy

// Round 13 E-U1 regression: a streaming request whose pacer already committed
// a keepalive 200 cannot relay a later upstream 202. The client saw 200, so
// the record must report 200 with the in-band error, matching the cursor and
// non-completion twin. Without a keepalive commit the real 202 is recorded.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestCommittedKeepaliveNonCompletionRecords200(t *testing.T) {
	hold := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-hold
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"id\":\"x\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"accepted"}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueWait = 5 * time.Second
	cfg.SSEKeepaliveInterval = time.Millisecond
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"hold","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(30 * time.Millisecond)

	queued, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"wait","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	queued.Header.Set("Authorization", "Bearer sk-test")
	queued.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(queued)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queued status = %d, want the committed keepalive 200", resp.StatusCode)
	}
	// Headers are committed, so the request holds the keepalive status while
	// it waits. Releasing the first request lets the queued one acquire and
	// receive the upstream 202, which can only be reported in band.
	close(hold)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"type":"api_error"`) && !strings.Contains(string(body), `"type":"rate_limit_error"`) {
		t.Fatalf("queued body %q, want an in-band error", body)
	}
	<-firstDone

	control, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"control","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	control.Header.Set("Authorization", "Bearer sk-test")
	control.Header.Set("X-Proxy-Base-URL", upstream.URL)
	cresp, err := http.DefaultClient.Do(control)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, cresp.Body)
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusAccepted {
		t.Fatalf("control status = %d, want the uncommitted 202", cresp.StatusCode)
	}

	records := waitForRecord(t, buf, 3)
	byModel := map[string]*metrics.Record{}
	for _, rec := range records {
		byModel[rec.Model] = rec
	}
	wait := byModel["wait"]
	if wait == nil {
		t.Fatalf("no record for the queued request: %+v", records)
	}
	if wait.StatusCode != http.StatusOK {
		t.Fatalf("queued record status = %d, want the committed 200", wait.StatusCode)
	}
	if wait.ErrorType == "" || wait.ErrorMsg == "" {
		t.Fatalf("queued record lost the in-band error: %+v", wait)
	}
	ctrl := byModel["control"]
	if ctrl == nil {
		t.Fatalf("no record for the control request: %+v", records)
	}
	if ctrl.StatusCode != http.StatusAccepted || ctrl.ErrorType != "" {
		t.Fatalf("control record = status %d error %q, want 202 with no error", ctrl.StatusCode, ctrl.ErrorType)
	}
}
