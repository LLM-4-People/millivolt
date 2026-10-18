package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func throttleTestProvider(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 48 {
		s += "x"
	}
	return s
}

func applyThrottleHeader(t *testing.T, p *Server, provider, header, value string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(header, value)
	if err := p.applyThrottleHeaders(r, provider, "client-neutral"); err != nil {
		t.Fatalf("apply %s=%s for %s: %v", header, value, provider, err)
	}
}

func throttleGenerations(p *Server) (persistGen, persistedGen uint64) {
	p.throttle.mu.Lock()
	defer p.throttle.mu.Unlock()
	return p.throttle.persistGen, p.throttle.persistedGen
}

func assertThrottleDocumentMatchesPolicy(t *testing.T, raw []byte, p *Server) []persistedThrottle {
	t.Helper()
	items := decodeThrottles(raw)
	want := p.latestThrottleSnap()
	gotJSON, err := json.Marshal(persistedThrottleDoc{Throttles: items})
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(persistedThrottleDoc{Throttles: want})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("persisted policy differs from scheduler policy\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	return items
}

// TestThrottleHeaderPersistenceCoalescesBurst is the H2 regression: 4096
// effective client header changes must not write one document per request.
// With the writer parked, every request returns without durable I/O; a flush
// then writes the final policy exactly once. A second alternating burst must
// also collapse into one whole-document write.
func TestThrottleHeaderPersistenceCoalescesBurst(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	const gates = 4096
	for i := 0; i < gates; i++ {
		applyThrottleHeader(t, p, throttleTestProvider(i), hdrLimitConcurrency, "1")
	}
	if saves, _, _, _ := store.stats(); saves != 0 {
		t.Fatalf("header changes wrote synchronously: %d saves before flush", saves)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	saves, raw, bytes, max := store.stats()
	if saves != 1 {
		t.Fatalf("4096 coalesced changes produced %d writes, want 1", saves)
	}
	if len(raw) > throttlePersistDocMaxBytes || max > throttlePersistDocMaxBytes {
		t.Fatalf("persisted document = %d bytes, cap %d", len(raw), throttlePersistDocMaxBytes)
	}
	if bytes != int64(len(raw)) {
		t.Fatalf("total persisted bytes = %d, want one %d-byte document", bytes, len(raw))
	}
	items := assertThrottleDocumentMatchesPolicy(t, raw, p)
	if len(items) != gates {
		t.Fatalf("persisted gates = %d, want %d", len(items), gates)
	}

	// Alternate one existing gate. Only the latest value matters and the
	// whole-policy document is written once more, not once per alternation.
	provider := throttleTestProvider(0)
	for i := 0; i < 100; i++ {
		value := "1"
		if i%2 == 1 {
			value = "2"
		}
		applyThrottleHeader(t, p, provider, hdrLimitConcurrency, value)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	saves2, raw2, bytes2, _ := store.stats()
	if saves2-saves != 1 {
		t.Fatalf("100 alternations produced %d writes, want 1", saves2-saves)
	}
	if bytes2-bytes > int64(len(raw2)) {
		t.Fatalf("alternation burst wrote %d bytes, want at most one %d-byte document", bytes2-bytes, len(raw2))
	}
	items = assertThrottleDocumentMatchesPolicy(t, raw2, p)
	if len(items) != gates {
		t.Fatalf("persisted gates after alternation = %d, want %d", len(items), gates)
	}
	found := false
	for _, it := range items {
		if it.Provider == provider {
			if it.Concurrency != 2 {
				t.Fatalf("persisted latest policy = %+v, want concurrency 2", it)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("persisted document lost provider %q", provider)
	}

	// A fresh server restores the same store document.
	restored := New(config.Default(), metrics.Noop{})
	t.Cleanup(func() { _ = restored.CloseThrottlePersist(context.Background()) })
	restored.attachThrottlePersist(store)
	if got := schedThrottleFor(restored.scheduler, provider).Limit.Concurrency; got != 2 {
		t.Fatalf("restored concurrency = %d, want 2", got)
	}
	if got := len(restored.scheduler.ThrottlePolicies()); got != gates {
		t.Fatalf("restored gates = %d, want %d", got, gates)
	}
}

// TestThrottleHeaderWriterCoalescesWithoutFlush lets the background writer run
// through a rapid burst and bounds both its write count and total bytes. This
// is the request-path decoupling proof: no flush is needed for the writer to
// land the latest policy.
func TestThrottleHeaderWriterCoalescesWithoutFlush(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	p.throttle.debounce = 25 * time.Millisecond
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	const gates = 512
	for i := 0; i < gates; i++ {
		applyThrottleHeader(t, p, throttleTestProvider(i), hdrLimitConcurrency, "1")
	}
	deadline := time.Now().Add(3 * time.Second)
	for store.saveCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("background writer never persisted the burst")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	saves, raw, bytes, _ := store.stats()
	if saves > 16 {
		t.Fatalf("background writer made %d writes for one burst", saves)
	}
	if bytes > int64(saves)*int64(len(raw)) {
		t.Fatalf("persisted bytes = %d, bound %d", bytes, int64(saves)*int64(len(raw)))
	}
	items := assertThrottleDocumentMatchesPolicy(t, raw, p)
	if len(items) != gates {
		t.Fatalf("persisted gates = %d, want %d", len(items), gates)
	}
}

// TestThrottleHeaderSinkFailureDoesNotFailInference drives the real HTTP path
// with a failing persistence sink: the validated cap is applied, the request
// still reaches the upstream, and the dirty generation remains for a flush.
func TestThrottleHeaderSinkFailureDoesNotFailInference(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	sink := &flakyThrottlePersist{remain: 1 << 30, saved: make(chan struct{}, 1)}
	p.attachThrottlePersist(sink)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	srv := httptest.NewServer(p)
	defer srv.Close()
	resp := throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		hdrLimitConcurrency: "2",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inference status = %d body %s", resp.StatusCode, body)
	}
	if th := schedThrottleFor(p.scheduler, testProvider(t, upstream.URL)); th.Limit.Concurrency != 2 {
		t.Fatalf("runtime cap = %+v, want 2", th.Limit)
	}
	if err := p.FlushThrottles(context.Background()); err == nil {
		t.Fatal("flush against a failing sink returned success")
	}
}

// TestThrottleHeaderPersistenceOneChangeWritesOnce pins that one change after
// an idle period writes one document, not one write per existing gate.
func TestThrottleHeaderPersistenceOneChangeWritesOnce(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	const gates = 64
	for i := 0; i < gates; i++ {
		applyThrottleHeader(t, p, throttleTestProvider(i), hdrLimitConcurrency, "1")
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	baseline, _, _, _ := store.stats()
	applyThrottleHeader(t, p, throttleTestProvider(0), hdrLimitConcurrency, "7")
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	saves, raw, _, _ := store.stats()
	if saves-baseline != 1 {
		t.Fatalf("one change after idle produced %d writes, want 1", saves-baseline)
	}
	items := assertThrottleDocumentMatchesPolicy(t, raw, p)
	if len(items) != gates {
		t.Fatalf("persisted gates = %d, want %d", len(items), gates)
	}
}

// TestThrottlePersistenceConcurrentChangesConverge drives concurrent partial
// header changes and asserts the documented policy equals the scheduler's
// latest policy, with no unpersisted generation left behind.
func TestThrottlePersistenceConcurrentChangesConverge(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		provider := throttleTestProvider(i % 8)
		header, value := hdrLimitConcurrency, strconv.Itoa(i+1)
		if i%3 == 0 {
			header, value = hdrLimitRequests, strconv.Itoa(i+1)+"/1m"
		}
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set(header, value)
			if err := p.applyThrottleHeaders(r, provider, "client-neutral"); err != nil {
				t.Errorf("apply %s=%s for %s: %v", header, value, provider, err)
			}
		})
	}
	wg.Wait()
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	saves, raw, _, _ := store.stats()
	if saves > 8 {
		t.Fatalf("concurrent changes produced %d writes, want a coalesced count", saves)
	}
	assertThrottleDocumentMatchesPolicy(t, raw, p)
	gen, persisted := throttleGenerations(p)
	if persisted != gen {
		t.Fatalf("persisted generation = %d, latest = %d", persisted, gen)
	}
}

// TestThrottleStaleWriteRewritesLatest pins the ordering rule: when a slower
// operator write lands after a newer generation has already been persisted, the
// stale loop must rewrite the latest policy before returning. Without that, the
// durable document could end on the older snapshot even though the generation
// marker advanced.
func TestThrottleStaleWriteRewritesLatest(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var writes [][]byte
	var calls atomic.Int32
	p.throttle.persist = testOperatorPersist{save: func(raw []byte) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		mu.Lock()
		writes = append(writes, append([]byte(nil), raw...))
		mu.Unlock()
		return nil
	}}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- p.applyThrottle(scheduler.Throttle{
			Provider: "provider.example", Limit: scheduler.Limit{Concurrency: 1},
		})
	}()
	<-entered
	if err := p.applyThrottle(scheduler.Throttle{
		Provider: "provider.example", Limit: scheduler.Limit{Concurrency: 2},
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]byte(nil), writes[len(writes)-1]...)
	count := len(writes)
	mu.Unlock()
	if count != 3 {
		t.Fatalf("writes = %d, want stale write plus latest rewrite", count)
	}
	items := decodeThrottles(got)
	if len(items) != 1 || items[0].Provider != "provider.example" || items[0].Concurrency != 2 {
		t.Fatalf("final durable policy = %+v, want the latest concurrency 2", items)
	}
}

// flakyThrottlePersist fails the configured number of writes, then records the
// successful document and signals it. It exercises the background retry path
// without sleeping for a production backoff window.
type flakyThrottlePersist struct {
	mu     sync.Mutex
	remain int
	calls  int
	raw    []byte
	saved  chan struct{}
}

func (p *flakyThrottlePersist) SaveThrottle(_ context.Context, raw []byte) error {
	p.mu.Lock()
	p.calls++
	fail := p.remain > 0
	if fail {
		p.remain--
	}
	p.mu.Unlock()
	if fail {
		return errors.New("disk unavailable")
	}
	p.mu.Lock()
	p.raw = append(p.raw[:0], raw...)
	p.mu.Unlock()
	select {
	case p.saved <- struct{}{}:
	default:
	}
	return nil
}

func (p *flakyThrottlePersist) LoadThrottle(context.Context) ([]byte, error) { return nil, nil }

func (p *flakyThrottlePersist) stats() (calls int, raw []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]byte(nil), p.raw...)
}

// TestThrottleHeaderPersistenceRetriesAfterFailure proves a sink failure is
// logged by the shared persist owner, retried with backoff, and never rejects
// the inference request or loses the dirty policy.
func TestThrottleHeaderPersistenceRetriesAfterFailure(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	sink := &flakyThrottlePersist{remain: 1, saved: make(chan struct{}, 1)}
	p.attachThrottlePersist(sink)
	p.throttle.debounce = 5 * time.Millisecond
	p.throttle.retryMin = 5 * time.Millisecond
	p.throttle.retryMax = 20 * time.Millisecond
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	applyThrottleHeader(t, p, "provider.example", hdrLimitConcurrency, "2")
	select {
	case <-sink.saved:
	case <-time.After(3 * time.Second):
		t.Fatal("failed write was not retried")
	}
	calls, raw := sink.stats()
	if calls < 2 {
		t.Fatalf("persistence calls = %d, want a retry after the first failure", calls)
	}
	items := decodeThrottles(raw)
	if len(items) != 1 || items[0].Provider != "provider.example" || items[0].Concurrency != 2 {
		t.Fatalf("retried document = %+v", items)
	}
}

// TestThrottleHeaderPersistenceFlushRecoversAfterFailure proves a failed
// background write leaves the policy dirty, so a later flush persists it.
func TestThrottleHeaderPersistenceFlushRecoversAfterFailure(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	sink := &flakyThrottlePersist{remain: 1, saved: make(chan struct{}, 1)}
	p.attachThrottlePersist(sink)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	applyThrottleHeader(t, p, "provider.example", hdrLimitConcurrency, "3")
	if err := p.FlushThrottles(context.Background()); err == nil {
		t.Fatal("flush succeeded against a failing sink")
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatalf("flush after the sink recovered: %v", err)
	}
	_, raw := sink.stats()
	items := decodeThrottles(raw)
	if len(items) != 1 || items[0].Concurrency != 3 {
		t.Fatalf("recovered flush document = %+v", items)
	}
}

// blockingCtxPersist honors cancellation so the close bound is deterministic
// without waiting for a production storage timeout.
type blockingCtxPersist struct {
	started chan struct{}
}

func (p *blockingCtxPersist) SaveThrottle(ctx context.Context, _ []byte) error {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (*blockingCtxPersist) LoadThrottle(context.Context) ([]byte, error) { return nil, nil }

func TestThrottlePersisterCloseBounded(t *testing.T) {
	cfg := config.Default()
	cfg.StorageQueryTimeout = 20 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	sink := &blockingCtxPersist{started: make(chan struct{}, 1)}
	p.attachThrottlePersist(sink)
	deferThrottleWriter(p)

	applyThrottleHeader(t, p, "provider.example", hdrLimitConcurrency, "1")
	start := time.Now()
	err := p.CloseThrottlePersist(context.Background())
	if err == nil {
		t.Fatal("close reported success while the sink was blocked")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded close took %s", elapsed)
	}
	select {
	case <-p.throttle.done:
	default:
		t.Fatal("persister goroutine still running after bounded close")
	}
	// A second close is safe and equally bounded; the dirty generation is
	// still present because the failed write never advanced it.
	start = time.Now()
	if err := p.CloseThrottlePersist(context.Background()); err == nil {
		t.Fatal("second close reported success while the sink was blocked")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second bounded close took %s", elapsed)
	}
}

func TestThrottlePersisterCloseIdempotent(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)

	applyThrottleHeader(t, p, "provider.example", hdrLimitConcurrency, "4")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if err := p.CloseThrottlePersist(context.Background()); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
	wg.Wait()
	select {
	case <-p.throttle.done:
	default:
		t.Fatal("persister goroutine still running after close")
	}
	saves, raw, _, _ := store.stats()
	if saves != 1 {
		t.Fatalf("concurrent closes wrote %d documents, want 1", saves)
	}
	items := decodeThrottles(raw)
	if len(items) != 1 || items[0].Concurrency != 4 {
		t.Fatalf("closed policy = %+v", items)
	}
	if err := p.CloseThrottlePersist(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if saves, _, _, _ := store.stats(); saves != 1 {
		t.Fatalf("second close wrote again: %d saves", saves)
	}
}

func TestThrottleProviderLabelBound(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	normal := strings.Repeat("a", throttleProviderLabelMaxBytes)
	applyThrottleHeader(t, p, normal, hdrLimitConcurrency, "1")

	overlong := strings.Repeat("b", throttleProviderLabelMaxBytes+1)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set(hdrLimitConcurrency, "1")
	if err := p.applyThrottleHeaders(r, overlong, "client"); err == nil {
		t.Fatal("overlong header provider label was accepted")
	}
	if err := p.applyThrottle(scheduler.Throttle{Provider: overlong, Limit: scheduler.Limit{Concurrency: 1}}); err == nil {
		t.Fatal("overlong operator provider label was accepted")
	}
	rr := postThrottle(t, p, `{"provider":"`+overlong+`","concurrency":1}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("overlong operator POST = %d, want 400", rr.Code)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, raw, _, max := store.stats()
	if max > throttlePersistDocMaxBytes {
		t.Fatalf("persisted document exceeded cap: %d", max)
	}
	items := decodeThrottles(raw)
	if len(items) != 1 || items[0].Provider != normal {
		t.Fatalf("overlong label changed the persisted document: %+v", items)
	}
}
