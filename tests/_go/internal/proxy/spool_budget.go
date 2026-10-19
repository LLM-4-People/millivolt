package proxy

import (
	"bytes"
	"fmt"
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
)

// paddedSpoolBody pads the healthy realBody with JSON whitespace to exactly
// size bytes, so a document at the spool cap remains a valid response the
// quality classifier accepts.
func paddedSpoolBody(t *testing.T, size int) []byte {
	t.Helper()
	if size < len(realBody) {
		t.Fatalf("size %d cannot hold the %d-byte healthy body", size, len(realBody))
	}
	body := make([]byte, 0, size)
	body = append(body, realBody...)
	body = append(body, bytes.Repeat([]byte{' '}, size-len(realBody))...)
	return body
}

// waitForSpoolBudgetRelease polls for the reservation to drain; the handler
// releases it right after the client write, which can complete a moment after
// the client observes the last body byte.
func waitForSpoolBudgetRelease(t *testing.T, p *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.respBudget.bytes.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("response budget still holds %d bytes, want 0", p.respBudget.bytes.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func spoolChatRequest(t *testing.T, srvURL, upstreamURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	return http.DefaultClient.Do(req)
}

// TestQualitySpoolBudgetAdmitsLargestSpool is the control: a single response
// whose declared body exactly fills the spool still fits a one-MiB response
// budget and relays verbatim, with the reservation released afterwards.
func TestQualitySpoolBudgetAdmitsLargestSpool(t *testing.T) {
	cfg := config.Default()
	cfg.MaxInflightResponseBytes = config.MaxInflightResponseBytesMin
	cfg.QualityRetries = 0
	body := paddedSpoolBody(t, qualitySpoolMax)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer upstream.Close()

	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := spoolChatRequest(t, srv.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a body at the spool cap fits the budget)", resp.StatusCode)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("relayed %d bytes, want the %d-byte body verbatim", len(got), len(body))
	}
	waitForSpoolBudgetRelease(t, p)
}

// TestQualitySpoolReservesResponseBudget reproduces the whole-document
// admission hole on the non-streaming quality spool: a spool holds up to
// qualitySpoolMax bytes before any client write, but took no respBudget
// reservation, so two concurrent one-MiB documents were both admitted under a
// one-MiB budget. The first spool holds its reservation while its upstream
// body is stalled; the second request must be refused with the shared
// retryable 503 before reading, and the first must still complete verbatim.
func TestQualitySpoolReservesResponseBudget(t *testing.T) {
	cfg := config.Default()
	cfg.MaxInflightResponseBytes = config.MaxInflightResponseBytesMin
	cfg.QualityRetries = 0
	body := paddedSpoolBody(t, qualitySpoolMax)

	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(firstStarted)
			<-releaseFirst
			w.Write(body)
			return
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	p := New(cfg, buf)
	srv := httptest.NewServer(p)
	defer srv.Close()
	defer release()

	type result struct {
		status int
		body   []byte
		err    error
	}
	firstDone := make(chan result, 1)
	go func() {
		resp, err := spoolChatRequest(t, srv.URL, upstream.URL)
		if err != nil {
			firstDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		got, rerr := io.ReadAll(resp.Body)
		firstDone <- result{status: resp.StatusCode, body: got, err: rerr}
	}()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first upstream request never started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.respBudget.bytes.Load() < int64(qualitySpoolMax) {
		if time.Now().After(deadline) {
			t.Fatalf("first spool never reserved its bounded bytes: held %d, want %d", p.respBudget.bytes.Load(), qualitySpoolMax)
		}
		time.Sleep(time.Millisecond)
	}

	resp, err := spoolChatRequest(t, srv.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent spool status = %d body %q, want 503 from the shared response-buffer budget", resp.StatusCode, secondBody)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if !bytes.Contains(secondBody, []byte(errResponseBufferBudget.Error())) {
		t.Fatalf("second body = %q, want the shared budget error envelope", secondBody)
	}

	release()
	first := <-firstDone
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.status != http.StatusOK || !bytes.Equal(first.body, body) {
		t.Fatalf("first spool status=%d len=%d, want 200 and the %d-byte body verbatim", first.status, len(first.body), len(body))
	}
	waitForSpoolBudgetRelease(t, p)

	recs := waitForRecord(t, buf, 2)
	budgetRefusals := 0
	for _, rec := range recs {
		if rec.ErrorType == "response_buffer_budget" {
			budgetRefusals++
			if rec.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("budget refusal record status = %d, want 503", rec.StatusCode)
			}
		}
	}
	if budgetRefusals != 1 {
		t.Fatalf("response_buffer_budget records = %d, want 1 (records: %+v)", budgetRefusals, recs)
	}
}
