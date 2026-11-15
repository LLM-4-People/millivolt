package proxy

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// errFakeBusy is a transient storage failure: the class that must keep the
// retry ladder alive.
var errFakeBusy = errors.New("database is locked")

// failingThrottlePersist answers every write with a fixed error and counts the
// attempts, so a test can tell a retry ladder from a stop.
type failingThrottlePersist struct {
	err   error
	mu    sync.Mutex
	saves int
}

func (p *failingThrottlePersist) SaveThrottle(context.Context, []byte) error {
	p.mu.Lock()
	p.saves++
	p.mu.Unlock()
	return p.err
}

func (p *failingThrottlePersist) LoadThrottle(context.Context) ([]byte, error) { return nil, nil }

func (p *failingThrottlePersist) saveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saves
}

func fastThrottleRetries(p *Server) {
	p.throttle.debounce = time.Millisecond
	p.throttle.retryMin = time.Millisecond
	p.throttle.retryMax = 2 * time.Millisecond
}

func dirtyTestThrottle(t *testing.T, p *Server) {
	t.Helper()
	th := scheduler.Throttle{
		Scope:     scheduler.LimitScope{Providers: []string{"closed.example"}},
		Limit:     scheduler.Limit{Concurrency: 2},
		Source:    scheduler.ThrottleSourceUI,
		UpdatedBy: "test",
	}
	// markThrottleDirty is the non-blocking header path: it bumps the
	// generation and starts the one background writer. A synchronous operator
	// edit would fail before the writer ever runs.
	p.scheduler.SetPolicy(th)
	p.markThrottleDirty()
}

// TestThrottlePersisterStopsOnClosedStore pins the closed-store class: once the
// database is closed no later attempt can succeed, so the writer leaves the
// generation dirty and stops instead of hammering the closed handle until
// shutdown.
func TestThrottlePersisterStopsOnClosedStore(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &failingThrottlePersist{err: errors.New("sql: database is closed")}
	p.attachThrottlePersist(store)
	fastThrottleRetries(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	dirtyTestThrottle(t, p)
	select {
	case <-p.throttle.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("persister kept retrying a closed store (%d attempts)", store.saveCount())
	}
	p.throttle.mu.Lock()
	dirty := p.throttle.persistGen != p.throttle.persistedGen
	p.throttle.mu.Unlock()
	if !dirty {
		t.Fatal("a closed-store failure claimed the generation was persisted")
	}
}

// TestThrottlePersisterRetriesBusyStore is the other half: a busy store is
// transient, so the ladder keeps trying well past a single attempt.
func TestThrottlePersisterRetriesBusyStore(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &failingThrottlePersist{err: errFakeBusy}
	p.attachThrottlePersist(store)
	fastThrottleRetries(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	dirtyTestThrottle(t, p)
	deadline := time.Now().Add(5 * time.Second)
	for store.saveCount() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("busy store attempts = %d, want the retry ladder", store.saveCount())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-p.throttle.done:
		t.Fatal("busy store stopped the persister")
	default:
	}
}

// TestThrottlePersisterStopsOnRealClosedStore is the same stop driven by a real
// *storage.Store: a closed *sql.DB answers with database/sql's own
// "database is closed" error, not sql.ErrConnDone, so this case fails if the
// classifier or the fake drifts from the driver.
func TestThrottlePersisterStopsOnRealClosedStore(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "closed-sink.db"))
	p := New(config.Default(), metrics.Noop{})
	p.attachThrottlePersist(store)
	fastThrottleRetries(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dirtyTestThrottle(t, p)
	select {
	case <-p.throttle.done:
	case <-time.After(5 * time.Second):
		t.Fatal("persister kept retrying a really closed store")
	}
}
