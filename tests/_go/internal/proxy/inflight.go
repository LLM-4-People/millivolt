package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestInflightLimiterBoundsAndReloads(t *testing.T) {
	var l inflightLimiter
	l.set(2, 100)
	if !l.acquire(40) || !l.acquire(40) {
		t.Fatal("admission below the bounds failed")
	}
	if l.acquire(10) {
		t.Fatal("admission above the request bound succeeded")
	}
	l.release(40)
	if !l.acquire(40) {
		t.Fatal("released slot not reusable")
	}
	l.release(40)
	l.release(40)
	if got := l.cur.Load(); got != 0 {
		t.Fatalf("request counter = %d, want 0", got)
	}

	var b inflightLimiter
	b.set(10, 100)
	if !b.acquire(60) {
		t.Fatal("body below the byte budget refused")
	}
	if b.acquire(60) {
		t.Fatal("body over the byte budget accepted")
	}
	if got := b.bytes.Load(); got != 60 {
		t.Fatalf("byte counter = %d after refusal, want 60", got)
	}
	b.release(60)
	if !b.acquire(100) {
		t.Fatal("released byte budget not reusable")
	}
	b.release(100)

	b.set(0, 0) // unlimited: a live reload must not strand capacity
	if !b.acquire(1<<30) || !b.acquire(1<<30) {
		t.Fatal("unlimited limiter refused")
	}
	b.release(1 << 30)
	b.release(1 << 30)
	if got := b.bytes.Load(); got != 0 {
		t.Fatalf("byte counter = %d, want 0", got)
	}
}

func TestResponseBufferBudgetBoundsAndReleases(t *testing.T) {
	var b responseBufferBudget
	b.set(100)
	if !b.acquire(60) {
		t.Fatal("response buffer below the budget refused")
	}
	if b.acquire(60) {
		t.Fatal("response buffer above the budget accepted")
	}
	if got := b.bytes.Load(); got != 60 {
		t.Fatalf("response buffer bytes = %d after refusal, want 60", got)
	}
	b.release(60)
	if !b.acquire(100) {
		t.Fatal("released response budget not reusable")
	}
	b.release(100)
	if got := b.bytes.Load(); got != 0 {
		t.Fatalf("response buffer bytes = %d, want 0", got)
	}

	b.set(0)
	if !b.acquire(1) || !b.acquire(1<<40) {
		t.Fatal("unlimited response budget refused")
	}
	b.release(1)
	b.release(1 << 40)
	if got := b.bytes.Load(); got != 0 {
		t.Fatalf("unlimited response buffer bytes = %d, want 0", got)
	}
	if !b.acquire(0) {
		t.Fatal("zero-byte reservation refused")
	}
}

func TestResponseReadReserveUsesDeclaredLengthOrCap(t *testing.T) {
	cases := []struct {
		contentLength int64
		readCap       int64
		want          int64
	}{
		{150, 32 << 20, 150},
		{0, 32 << 20, 0},
		{-1, 32 << 20, 32 << 20},
		{32 << 20, 32 << 20, 32 << 20},
		{32<<20 + 1, 32 << 20, 32 << 20},
	}
	for _, tc := range cases {
		if got := responseReadReserve(tc.contentLength, tc.readCap); got != tc.want {
			t.Errorf("responseReadReserve(%d, %d) = %d, want %d", tc.contentLength, tc.readCap, got, tc.want)
		}
	}
}

func TestWriteResponseBufferBudgetIsRetryable503(t *testing.T) {
	w := httptest.NewRecorder()
	rec := new(metrics.Record)
	writeResponseBufferBudget(w, rec, ir.Chat)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if rec.StatusCode != http.StatusServiceUnavailable || rec.ErrorType != "response_buffer_budget" {
		t.Fatalf("record = status %d type %q, want 503 response_buffer_budget", rec.StatusCode, rec.ErrorType)
	}
}

type gatedResponseBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
}

func (b *gatedResponseBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *gatedResponseBody) Close() error { return nil }

type countingResponseBody struct {
	r     io.Reader
	reads int
}

func (b *countingResponseBody) Read(p []byte) (int, error) {
	b.reads++
	return b.r.Read(p)
}

func (b *countingResponseBody) Close() error { return nil }

// TestTranslatedNonStreamResponseBudgetRefusesBeforeReadAndReleases pins the
// process-wide reservation around the one whole-document translation read: a
// concurrent buffered response holds the budget, the next translation is
// refused with 503 + Retry-After before its upstream body is read, and the
// release restores capacity.
func TestTranslatedNonStreamResponseBudgetRefusesBeforeReadAndReleases(t *testing.T) {
	upstreamBody := []byte(`{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`)
	p := New(config.Default(), metrics.Noop{})
	p.respBudget.set(int64(len(upstreamBody)))

	first := &gatedResponseBody{started: make(chan struct{}), release: make(chan struct{}), data: upstreamBody}
	firstResp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          first,
		ContentLength: int64(len(upstreamBody)),
	}
	firstW := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		p.serveNonStreaming(context.Background(), firstW, firstResp, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			&target{format: "anthropic"}, "key", []byte("request"), new(metrics.Record), "group", scheduler.WaiterHooks{})
	}()

	select {
	case <-first.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first translation never began reading")
	}

	secondBody := &countingResponseBody{r: bytes.NewReader(upstreamBody)}
	secondResp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          secondBody,
		ContentLength: int64(len(upstreamBody)),
	}
	secondRec := new(metrics.Record)
	secondW := httptest.NewRecorder()
	p.serveNonStreaming(context.Background(), secondW, secondResp, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&target{format: "anthropic"}, "key", []byte("request"), secondRec, "group", scheduler.WaiterHooks{})
	if secondW.Code != http.StatusServiceUnavailable {
		t.Fatalf("second translation status = %d, want 503", secondW.Code)
	}
	if got := secondW.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("second Retry-After = %q, want 1", got)
	}
	if secondBody.reads != 0 {
		t.Fatalf("refused response was read %d times before admission", secondBody.reads)
	}
	if secondRec.ErrorType != "response_buffer_budget" {
		t.Fatalf("second record error type = %q, want response_buffer_budget", secondRec.ErrorType)
	}

	close(first.release)
	<-firstDone
	if firstW.Code != http.StatusOK {
		t.Fatalf("first translation status = %d, want 200: %s", firstW.Code, firstW.Body.String())
	}
	if got := p.respBudget.bytes.Load(); got != 0 {
		t.Fatalf("response budget after release = %d, want 0", got)
	}

	thirdBody := &countingResponseBody{r: bytes.NewReader(upstreamBody)}
	thirdResp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          thirdBody,
		ContentLength: int64(len(upstreamBody)),
	}
	thirdW := httptest.NewRecorder()
	p.serveNonStreaming(context.Background(), thirdW, thirdResp, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&target{format: "anthropic"}, "key", []byte("request"), new(metrics.Record), "group", scheduler.WaiterHooks{})
	if thirdW.Code != http.StatusOK {
		t.Fatalf("third translation status = %d, want 200", thirdW.Code)
	}
	if thirdBody.reads == 0 {
		t.Fatal("third translation did not read its upstream body after release")
	}
	if got := p.respBudget.bytes.Load(); got != 0 {
		t.Fatalf("response budget after third translation = %d, want 0", got)
	}
}

// TestResponseBufferBudgetHotReload pins that max_inflight_response_bytes is
// wired through the same live reload path as the inbound body budget.
func TestResponseBufferBudgetHotReload(t *testing.T) {
	cfg := config.Default()
	cfg.MaxInflightResponseBytes = config.MaxInflightResponseBytesMin
	p := New(cfg, metrics.Noop{})
	if got := p.respBudget.limit.Load(); got != int64(config.MaxInflightResponseBytesMin) {
		t.Fatalf("initial response budget = %d, want %d", got, config.MaxInflightResponseBytesMin)
	}
	next := cfg.Clone()
	next.MaxInflightResponseBytes = 2 * config.MaxInflightResponseBytesMin
	p.Reload(next)
	if got := p.respBudget.limit.Load(); got != int64(2*config.MaxInflightResponseBytesMin) {
		t.Fatalf("reloaded response budget = %d, want %d", got, 2*config.MaxInflightResponseBytesMin)
	}
}

// TestInflightBodyBudgetChargesBytesNotDeclarations pins the admission rule that
// replaced up-front declaration reserving. With a 256 MiB budget and a 32 MiB
// per-request cap, the old owner reserved each request's declared length, so
// eight idle connections declaring 32 MiB and sending nothing consumed the whole
// budget and every other client was refused. Charging bytes as they arrive keeps
// the heap bound while leaving an idle declaration cheap.
func TestInflightBodyBudgetChargesBytesNotDeclarations(t *testing.T) {
	const budget = 256 << 20
	const declared = 32 << 20
	const hint = 1 << 20

	var l inflightLimiter
	l.set(10000, budget)

	// Eight idle requests that declare 32 MiB but send nothing must not exhaust
	// the budget: only their speculative hints are charged.
	reservations := make([]*bodyReservation, 0, 8)
	for range 8 {
		r, ok := l.acquireBody(min(declared, hint))
		if !ok {
			t.Fatalf("idle request %d was refused: an idle declaration must not consume budget", len(reservations))
		}
		reservations = append(reservations, r)
	}
	if got := l.bytes.Load(); got != 8*hint {
		t.Fatalf("charged %d bytes for eight idle declarations, want %d", got, 8*hint)
	}
	// The budget is still available for real traffic.
	next, ok := l.acquireBody(hint)
	if !ok {
		t.Fatal("a further request was refused while every idle request had sent nothing")
	}
	next.release()
	for _, r := range reservations {
		r.release()
	}
	if got := l.bytes.Load(); got != 0 {
		t.Fatalf("byte counter = %d after release, want 0", got)
	}

	// A request that really uploads is charged for what it sends, and the
	// budget still refuses a body that would cross it.
	var small inflightLimiter
	small.set(10000, 300)
	r, ok := small.acquireBody(100)
	if !ok {
		t.Fatal("initial speculative reservation refused")
	}
	if !r.chargeRead(100) {
		t.Fatal("bytes within the speculative reservation were charged twice")
	}
	if got := small.bytes.Load(); got != 100 {
		t.Fatalf("bytes = %d after reading the reserved 100, want 100", got)
	}
	if !r.chargeRead(150) {
		t.Fatal("reading to the budget was refused")
	}
	if got := small.bytes.Load(); got != 250 {
		t.Fatalf("bytes = %d after reading 250 total, want 250", got)
	}
	if r.chargeRead(100) {
		t.Fatal("a read past the budget was admitted")
	}
	if got := small.bytes.Load(); got != 250 {
		t.Fatalf("bytes = %d after a refused read, want the unchanged 250", got)
	}
	r.release()
	if got := small.bytes.Load(); got != 0 {
		t.Fatalf("bytes = %d after release, want 0", got)
	}

	// A refused read surfaces as the retryable budget error, not as a body-cap
	// overflow, so the handler can answer 503 with Retry-After rather than 413.
	body := &chargedReader{r: bytes.NewReader(make([]byte, 10)), charge: func(int64) bool { return false }}
	if _, err := io.ReadAll(body); !errors.Is(err, errBodyBudget) {
		t.Fatalf("refused read error = %v, want errBodyBudget", err)
	}
}
