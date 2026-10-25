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
	if !waitFor(t, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first request never reached upstream")
	}

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

// committedStatusRun holds the first request, lets a queued second request
// commit a keepalive 200, then answers it with status/payload. The queued
// client body and the finalized records by model are returned.
func committedStatusRun(t *testing.T, status int, payload string) (string, map[string]*metrics.Record) {
	t.Helper()
	hold := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-hold
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueWait = 5 * time.Second
	cfg.SSEKeepaliveInterval = time.Millisecond
	cfg.MaxRetries = 0
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(cfg, buf))
	t.Cleanup(srv.Close)

	newChat := func(model string, stream bool) *http.Request {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, err := http.DefaultClient.Do(newChat("hold", false))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if !waitFor(t, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("first request never reached upstream")
	}

	resp, err := http.DefaultClient.Do(newChat("queued", true))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queued status = %d, want the committed keepalive 200", resp.StatusCode)
	}
	close(hold)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	<-firstDone

	byModel := map[string]*metrics.Record{}
	for _, rec := range waitForRecord(t, buf, 2) {
		byModel[rec.Model] = rec
	}
	return string(body), byModel
}

// TestCommittedKeepaliveRateLimitKeepsHealthSignal pins NEW-8: a 429 whose
// status line the committed 200 could not relay still counts once in the
// distinct rate-limit total, through an in-band attempt entry that is not a
// retry. The uncommitted control keeps the ordinary 429 classification.
func TestCommittedKeepaliveRateLimitKeepsHealthSignal(t *testing.T) {
	body, records := committedStatusRun(t, http.StatusTooManyRequests,
		`{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	if !strings.Contains(body, `"type":"rate_limit_error"`) {
		t.Fatalf("queued body = %q, want an in-band rate-limit error", body)
	}
	queued := records["queued"]
	if queued == nil {
		t.Fatalf("no queued record: %+v", records)
	}
	if queued.StatusCode != http.StatusOK {
		t.Fatalf("queued status = %d, want the committed 200", queued.StatusCode)
	}
	if !queued.IsError() {
		t.Fatalf("queued record lost the in-band failure: %+v", queued)
	}
	if !queued.HasRateLimit() {
		t.Fatalf("queued record lost the upstream 429 rate-limit signal: %+v", queued)
	}
	if queued.Retries != 0 {
		t.Fatalf("in-band entry counted as a retry: %+v", queued)
	}
	if len(queued.Attempts) != 1 || queued.Attempts[0].StatusCode != http.StatusTooManyRequests || !queued.Attempts[0].InBand {
		t.Fatalf("attempts = %+v, want one in-band 429 entry", queued.Attempts)
	}
}

// TestUncommittedRateLimitKeepsFlowClassification is the no-commit control:
// an ordinary streaming 429 records the real status and stays a flow event,
// not an error, with no attempt entry.
func TestUncommittedRateLimitKeepsFlowClassification(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.MaxRetries = 0
	buf := metrics.NewBuffer(4)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"control","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("control status = %d, want the uncommitted 429", resp.StatusCode)
	}
	records := waitForRecord(t, buf, 1)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	control := records[0]
	if control.StatusCode != http.StatusTooManyRequests || control.IsError() || !control.HasRateLimit() {
		t.Fatalf("control record = status %d isError=%v hasRateLimit=%v, want the ordinary 429 flow event",
			control.StatusCode, control.IsError(), control.HasRateLimit())
	}
	if len(control.Attempts) != 0 {
		t.Fatalf("control attempts = %+v, want none", control.Attempts)
	}
}

// TestCommittedKeepaliveServerFailureKeepsClassification pins the retryable
// failure twin: the committed 200 remains the client status, the failure stays
// an error, no rate-limit count appears, and no synthetic attempt entry is
// invented. Only the 429 health signal needs the in-band entry.
func TestCommittedKeepaliveServerFailureKeepsClassification(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body, records := committedStatusRun(t, status,
				`{"error":{"type":"server_error","message":"boom"}}`)
			if !strings.Contains(body, `"type":"server_error"`) {
				t.Fatalf("queued body = %q, want an in-band server error", body)
			}
			queued := records["queued"]
			if queued == nil {
				t.Fatalf("no queued record: %+v", records)
			}
			if queued.StatusCode != http.StatusOK || !queued.IsError() || queued.HasRateLimit() {
				t.Fatalf("queued record = status %d isError=%v hasRateLimit=%v, want 200/error/no-rate-limit",
					queued.StatusCode, queued.IsError(), queued.HasRateLimit())
			}
			if queued.ErrorType == "" || queued.ErrorMsg == "" {
				t.Fatalf("queued record lost the in-band error detail: %+v", queued)
			}
			if len(queued.Attempts) != 0 {
				t.Fatalf("retryable in-band failure invented an attempt entry: %+v", queued.Attempts)
			}
		})
	}
}
