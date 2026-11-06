package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// budget and every other client was refused. Reserving even one read hint per
// socket kept a smaller version of the same lever, so admission now reserves no
// body bytes at all and charges only what is read.
func TestInflightBodyBudgetChargesBytesNotDeclarations(t *testing.T) {
	const budget = 256 << 20

	var l inflightLimiter
	l.set(10000, budget)

	// Any number of idle requests must not consume the budget, because nothing
	// is charged until bytes arrive.
	reservations := make([]*bodyReservation, 0, 64)
	for i := range 64 {
		r, ok := l.acquireBody()
		if !ok {
			t.Fatalf("idle request %d was refused: an idle declaration must not consume budget", i)
		}
		reservations = append(reservations, r)
	}
	if got := l.bytes.Load(); got != 0 {
		t.Fatalf("charged %d bytes for 64 idle requests, want 0", got)
	}
	// The budget is still fully available for real traffic.
	next, ok := l.acquireBody()
	if !ok {
		t.Fatal("a further request was refused while every idle request had sent nothing")
	}
	if !next.chargeRead(4096) {
		t.Fatal("a real 4 KiB upload was refused behind idle requests")
	}
	next.release()
	for _, r := range reservations {
		r.release()
	}
	if got := l.bytes.Load(); got != 0 {
		t.Fatalf("byte counter = %d after release, want 0", got)
	}

	// A request that really uploads is charged for exactly what it sends, and
	// the budget still refuses a body that would cross it.
	var small inflightLimiter
	small.set(10000, 300)
	r, ok := small.acquireBody()
	if !ok {
		t.Fatal("initial reservation refused")
	}
	if !r.chargeRead(100) {
		t.Fatal("reading to the budget was refused")
	}
	if got := small.bytes.Load(); got != 100 {
		t.Fatalf("bytes = %d after reading 100, want 100", got)
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
	// A refused read must not be double-counted on the next successful one.
	if !r.chargeRead(50) {
		t.Fatal("a read that fits the remaining budget was refused")
	}
	if got := small.bytes.Load(); got != 300 {
		t.Fatalf("bytes = %d after a retried read, want 300 (no double charge)", got)
	}
	r.release()
	if got := small.bytes.Load(); got != 0 {
		t.Fatalf("bytes = %d after release, want 0", got)
	}

	// A refused chunk is counted but not delivered: the reader reports the
	// refusal immediately so the body owner releases its scratch instead of
	// buffering the rest uncharged. readRequestHint owns the cap drain.
	refused := &chargedReader{
		r:      bytes.NewReader(make([]byte, 10)),
		charge: func(int64) bool { return false },
	}
	got, err := io.ReadAll(refused)
	if !errors.Is(err, errBodyBudget) || len(got) != 0 {
		t.Fatalf("refused read = %d bytes, err=%v; want 0 bytes and errBodyBudget", len(got), err)
	}
	if refused.read != 10 {
		t.Fatalf("refused chunk count = %d, want 10", refused.read)
	}
	// A read that is chargeable is delivered in full.
	fine := &chargedReader{
		r:      bytes.NewReader(make([]byte, 10)),
		charge: func(int64) bool { return true },
	}
	if got, err := io.ReadAll(fine); err != nil || fine.read != 10 || len(got) != 10 {
		t.Fatalf("a chargeable read delivered %d bytes, read=%d err=%v", len(got), fine.read, err)
	}
}

// TestReadRequestHintBudgetRefusalBoundsRetention pins the cap/budget split
// after a refused charge: the body owner must not retain the upload, but it
// still has to answer 413 for an over-cap body (a retry can never succeed)
// and 503 only for a body that fits the cap.
func TestReadRequestHintBudgetRefusalBoundsRetention(t *testing.T) {
	const maxBytes = 1 << 20
	cfg := config.Default()
	for _, tc := range []struct {
		name    string
		size    int
		want413 bool
	}{
		{"fits cap", maxBytes / 2, false},
		{"exactly cap", maxBytes, false},
		{"one byte over cap", maxBytes + 1, true},
		{"far over cap", 4 * maxBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				bytes.NewReader(make([]byte, tc.size)))
			req.ContentLength = -1 // unknown length exercises the grower
			body, _, err := readRequestHint(req, maxBytes, false, requestReadHintMax,
				func(int64) bool { return false }, cfg)
			if body != nil {
				t.Fatalf("refused request retained %d body bytes", len(body))
			}
			var overflow *http.MaxBytesError
			if tc.want413 {
				if !errors.As(err, &overflow) || overflow.Limit != maxBytes {
					t.Fatalf("over-cap refusal = %T %v, want MaxBytesError(%d)", err, err, maxBytes)
				}
			} else if !errors.Is(err, errBodyBudget) {
				t.Fatalf("fits-cap refusal = %T %v, want errBodyBudget", err, err)
			}
		})
	}
}

// stalledBody blocks on its first Read until released (or its context is
// cancelled), so a test can hold one request inside admission without sending
// any bytes. Close is what an HTTP client disconnect does.
type stalledBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	sent    bool
}

func newStalledBody() *stalledBody {
	return &stalledBody{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.once.Do(func() { close(b.entered) })
	<-b.release
	b.sent = true
	return copy(p, `{"model":"m","messages":[]}`), nil
}

func (b *stalledBody) Close() error { return nil }

// TestInflightBudgetThroughTheHandler pins the admission rule at the level that
// actually matters. The unit test below exercises the reservation type; this
// one drives ServeHTTP, so it fails if the handler reserves a declared length
// again, if it stops passing the charge function, or if the refusal stops
// mapping to 503 with Retry-After.
//
// The pre-fix handler reserved each request's declared Content-Length, so N
// idle sockets declaring max_request_bytes each exhausted the budget and every
// other client was refused. Here two stalled requests declare 1 MiB and send
// nothing; a healthy request must still be admitted and succeed.
func TestInflightBudgetThroughTheHandler(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxInflightBodyBytes = config.MaxInflightBodyBytesMin // 1 MiB
	cfg.MaxRequestBytes = 1 << 20
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

	// Hold two requests inside admission. Each declares 1 MiB (the whole
	// budget) and has delivered no body bytes.
	stalled := make([]*stalledBody, 2)
	done := make(chan struct{}, len(stalled))
	for i := range stalled {
		body := newStalledBody()
		stalled[i] = body
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", body)
		r.ContentLength = 1 << 20
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", upstream.URL)
		go func() {
			defer func() { done <- struct{}{} }()
			s.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	for i, body := range stalled {
		select {
		case <-body.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("stalled request %d never entered the body read", i)
		}
	}

	// Nothing has been charged, so the whole budget is available.
	if got := s.inflight.bytes.Load(); got != 0 {
		t.Fatalf("idle declarations charged %d bytes, want 0", got)
	}
	healthy := httptest.NewRecorder()
	hr := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"healthy","messages":[{"role":"user","content":"hi"}]}`))
	hr.Header.Set("Authorization", "Bearer sk-test")
	hr.Header.Set("X-Proxy-Base-URL", upstream.URL)
	s.ServeHTTP(healthy, hr)
	if healthy.Code != http.StatusOK {
		t.Fatalf("healthy request status = %d, want 200 while idle requests hold no budget (body %s)",
			healthy.Code, healthy.Body.String())
	}

	// Release the stalled requests; every charge must be returned.
	for _, body := range stalled {
		close(body.release)
	}
	for range stalled {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a stalled request never returned")
		}
	}
	if got := s.inflight.bytes.Load(); got != 0 {
		t.Fatalf("body budget after release = %d, want 0", got)
	}
	if got := s.inflight.cur.Load(); got != 0 {
		t.Fatalf("request slots after release = %d, want 0", got)
	}

	// A body that crosses the budget mid-upload is refused with 503 and
	// Retry-After, in a retryable error class, and the charge is returned.
	cfg2 := config.Default()
	cfg2.MaxInflightBodyBytes = config.MaxInflightBodyBytesMin // 1 MiB
	cfg2.MaxRequestBytes = 2 << 20
	s2 := New(cfg2, metrics.Noop{})
	s2.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

	// Park the whole budget behind one live read.
	parked := newFillBody(int64(cfg2.MaxInflightBodyBytes))
	parkedReq := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", parked)
	parkedReq.ContentLength = int64(cfg2.MaxInflightBodyBytes) + 1
	parkedReq.Header.Set("Authorization", "Bearer sk-test")
	parkedReq.Header.Set("X-Proxy-Base-URL", upstream.URL)
	parkedDone := make(chan struct{})
	go func() {
		defer close(parkedDone)
		s2.ServeHTTP(httptest.NewRecorder(), parkedReq)
	}()
	select {
	case <-parked.filled:
	case <-time.After(5 * time.Second):
		t.Fatal("parked request never filled the budget")
	}
	// The charge is applied after Read returns, so wait for it rather than for
	// the fill signal alone: filling and charging are two steps.
	limit := int64(cfg2.MaxInflightBodyBytes)
	charged := time.Now().Add(5 * time.Second)
	for s2.inflight.bytes.Load() < limit {
		if time.Now().After(charged) {
			t.Fatalf("parked request charged %d of %d budget bytes", s2.inflight.bytes.Load(), limit)
		}
		time.Sleep(time.Millisecond)
	}
	over := httptest.NewRecorder()
	overReq := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"over","messages":[{"role":"user","content":"hi"}]}`))
	overReq.Header.Set("Authorization", "Bearer sk-test")
	overReq.Header.Set("X-Proxy-Base-URL", upstream.URL)
	s2.ServeHTTP(over, overReq)
	if over.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-budget request status = %d, want 503 (body %s)", over.Code, over.Body.String())
	}
	if got := over.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("over-budget Retry-After = %q, want 1", got)
	}
	if !strings.Contains(over.Body.String(), "api_error") {
		t.Fatalf("over-budget error type is not a retryable class: %s", over.Body.String())
	}
	close(parked.release)
	<-parkedDone
	if got := s2.inflight.bytes.Load(); got != 0 {
		t.Fatalf("body budget after the refused request = %d, want 0", got)
	}
}

// fillBody delivers n bytes, signals filled, then blocks until release. It lets
// a test hold the process-wide body budget without an unbounded body.
type fillBody struct {
	remaining int64
	filled    chan struct{}
	release   chan struct{}
	once      sync.Once
}

func newFillBody(n int64) *fillBody {
	return &fillBody{remaining: n, filled: make(chan struct{}), release: make(chan struct{})}
}

func (b *fillBody) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		n := int64(len(p))
		if n > b.remaining {
			n = b.remaining
		}
		b.remaining -= n
		if b.remaining == 0 {
			b.once.Do(func() { close(b.filled) })
		}
		for i := range p[:n] {
			p[i] = 'x'
		}
		return int(n), nil
	}
	<-b.release
	return 0, io.EOF
}

func (b *fillBody) Close() error { return nil }

// TestOversizedBodyStillAnswers413UnderBudgetPressure pins the status
// precedence. A body that is definitively over max_request_bytes must answer
// 413 even when the process-wide body budget is nearly exhausted: the cap is a
// property of this request, while the budget is transient load. Charging the
// over-cap bytes first would report an unrelated 503 (and invite a retry that
// can never succeed), so bytes past the cap are not charged.
func TestOversizedBodyStillAnswers413UnderBudgetPressure(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxInflightBodyBytes = config.MaxInflightBodyBytesMin // 1 MiB
	cfg.MaxRequestBytes = 1024                                // legal minimum
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

	// Leave only 512 bytes of budget, so an over-cap body would certainly
	// cross it if its bytes were charged.
	if !s.inflight.acquire(int64(cfg.MaxInflightBodyBytes) - 512) {
		t.Fatal("could not park budget for the fixture")
	}

	// 4 KiB body against a 1 KiB cap: definitively too large.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 4096)+`"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", upstream.URL)
	s.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap body under budget pressure status = %d, want 413 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("413 must not carry Retry-After, got %q", got)
	}
}
