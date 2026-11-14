package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// heldThrottlePersist parks the FIRST SaveThrottle so a test can force a
// background write to be captured and in flight while a newer policy lands.
// Every later write records itself and returns immediately.
type heldThrottlePersist struct {
	mu      sync.Mutex
	doc     []byte
	writes  []string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldThrottlePersist) SaveThrottle(_ context.Context, raw []byte) error {
	items, _ := decodeThrottles(raw)
	providers := make([]string, 0, len(items))
	for _, it := range items {
		providers = append(providers, it.Provider)
	}
	first := false
	h.once.Do(func() { first = true })
	if first {
		// The stale snapshot is captured; park here so the edit below can
		// complete while this write is still open.
		close(h.entered)
		<-h.release
	}
	h.mu.Lock()
	h.writes = append(h.writes, providers...)
	h.doc = append(h.doc[:0], raw...)
	h.mu.Unlock()
	return nil
}

func (h *heldThrottlePersist) LoadThrottle(context.Context) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]byte(nil), h.doc...), nil
}

func (h *heldThrottlePersist) finalDocument(t *testing.T) map[string]bool {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	items, degraded := decodeThrottles(h.doc)
	if degraded {
		t.Fatalf("durable document could not be decoded: %q", h.doc)
	}
	names := make(map[string]bool, len(items))
	for _, it := range items {
		names[it.Provider] = true
	}
	return names
}

// TestStaleThrottleWriteCannotEraseANewerEdit pins write ORDER, not just the
// generation bookkeeping. A background snapshot captured before an operator
// edit must never land after that edit's document: the store keeps one value
// per key, so the older document wins the last-write race and the edit is gone,
// while persistedGen still names the newer generation and therefore no retry
// and no shutdown flush rewrites it.
//
// Regression: the background writer held only the shared side of persistMu, so
// its parked snapshot overtook a newer operator write. Measured on that shape:
// the edit completed first, the stale write landed second, and the durable
// document ended with one provider. The writer now takes the exclusive side, so
// the edit waits and the last document written includes both policies.
func TestStaleThrottleWriteCannotEraseANewerEdit(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &heldThrottlePersist{entered: make(chan struct{}), release: make(chan struct{})}
	p.attachThrottlePersist(store)
	p.throttle.debounce = 0
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	// One header cap starts the background writer, whose first write parks
	// inside SaveThrottle with the older snapshot already captured.
	applyThrottleHeader(t, p, "alpha.example", hdrLimitConcurrency, "1")
	select {
	case <-store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the background writer never started its first write")
	}

	// The operator edit lands while that stale write is parked, then the write
	// is released. A correct owner serializes the two, so the document that is
	// written last must carry both policies.
	edited := make(chan struct{})
	go func() {
		defer close(edited)
		if rr := postThrottle(t, p, `{"provider":"beta.example","concurrency":2}`); rr.Code != 200 {
			t.Errorf("operator edit: %d %s", rr.Code, rr.Body.String())
		}
	}()
	// Let the edit reach the persistence owner before the stale write is
	// released; the race under test is which document lands last.
	select {
	case <-edited:
	case <-time.After(750 * time.Millisecond):
		// The exclusive owner blocks the edit here until the write finishes,
		// which is the correct behaviour; the ordering assertion below is what
		// decides the outcome either way.
	}
	close(store.release)
	select {
	case <-edited:
	case <-time.After(10 * time.Second):
		t.Fatal("the operator edit never completed")
	}

	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := store.finalDocument(t)
	if !names["alpha.example"] || !names["beta.example"] {
		t.Fatalf("durable document lost a policy: holds %v, want alpha.example and beta.example", names)
	}
}
