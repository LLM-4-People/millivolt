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
	if !waitFor(t, func() bool { return p.respBudget.bytes.Load() == 0 }) {
		t.Fatalf("response budget still holds %d bytes, want 0", p.respBudget.bytes.Load())
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

// gatedResponseWriter holds the handler's first body write until allow is
// closed. The write itself is the proof that nonStreamBody is relaying the
// over-cap prefix, so the test can inspect the reservation at exactly that
// point without depending on client-side buffering.
type gatedResponseWriter struct {
	header  http.Header
	started chan struct{}
	allow   chan struct{}
	once    sync.Once
	status  int
	body    bytes.Buffer
}

func newGatedResponseWriter() *gatedResponseWriter {
	return &gatedResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		allow:   make(chan struct{}),
	}
}

func (w *gatedResponseWriter) Header() http.Header { return w.header }
func (w *gatedResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *gatedResponseWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.allow
	return w.body.Write(p)
}

// TestQualitySpoolOverflowHoldsReservation reproduces the over-cap admission
// hole: spoolBody keeps a bounded prefix (qualitySpoolReadCap) for the
// verbatim passthrough, but the reservation was released before nonStreamBody
// wrote that prefix. Two ~1.08 MiB bodies under a 2 MiB budget were both
// admitted and both held their prefixes with the budget at zero. The first
// handler is held at its first passthrough write, so the prefix is certainly
// still live when the second request arrives; the second must be refused with
// the shared 503 until the first prefix is written and freed.
func TestQualitySpoolOverflowHoldsReservation(t *testing.T) {
	cfg := config.Default()
	cfg.MaxInflightResponseBytes = 2 << 20
	cfg.QualityRetries = 0
	// One chunk past the full spool read cap: the declared length forces the
	// cap-sized reservation, and spoolBody certainly overflows.
	body := paddedSpoolBody(t, qualitySpoolReadCap+nonStreamChunk)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer upstream.Close()

	p := New(cfg, metrics.Noop{})
	newRequest := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-k")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	firstWriter := newGatedResponseWriter()
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(firstWriter.allow) }) }
	defer allow()

	firstRequest := newRequest()
	firstDone := make(chan struct{})
	go func() {
		p.ServeHTTP(firstWriter, firstRequest)
		close(firstDone)
	}()

	select {
	case <-firstWriter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first over-cap prefix never reached the response writer")
	}
	if got, want := p.respBudget.bytes.Load(), int64(qualitySpoolReadCap); got != want {
		t.Fatalf("response budget while the first over-cap prefix is live = %d, want %d", got, want)
	}

	second := httptest.NewRecorder()
	p.ServeHTTP(second, newRequest())
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second over-cap spool status = %d body %q, want 503 while the first prefix is still live", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if !bytes.Contains(second.Body.Bytes(), []byte(errResponseBufferBudget.Error())) {
		t.Fatalf("second body = %q, want the shared budget error envelope", second.Body.String())
	}

	allow()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first over-cap relay never finished")
	}
	if firstWriter.status != http.StatusOK || !bytes.Equal(firstWriter.body.Bytes(), body) {
		t.Fatalf("first over-cap spool status=%d len=%d, want 200 and the %d-byte body verbatim", firstWriter.status, firstWriter.body.Len(), len(body))
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
	if !waitFor(t, func() bool { return p.respBudget.bytes.Load() >= int64(qualitySpoolMax) }) {
		t.Fatalf("first spool never reserved its bounded bytes: held %d, want %d", p.respBudget.bytes.Load(), qualitySpoolMax)
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
