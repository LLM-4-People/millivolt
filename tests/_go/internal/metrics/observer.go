package metrics

import (
	"sync"
	"testing"
	"time"
)

// The startup-wired observer is immutable metadata: concurrent SetModelObserver
// calls must be safe against SSE record/end serialization, and every read must
// return one of the installed providers (never a torn/nil result once set).
func TestModelObserverConcurrentSetAndObserve(t *testing.T) {
	buf := NewBuffer(4)
	first := func(bool, ...[]*Record) any { return "first" }
	second := func(bool, ...[]*Record) any { return "second" }
	buf.SetModelObserver(first)
	rec := &Record{ID: "concurrent", Model: "MODEL", Start: time.UnixMilli(1_800_000_000_000)}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				if (i+j)%2 == 0 {
					buf.SetModelObserver(first)
				} else {
					buf.SetModelObserver(second)
				}
			}
		})
		wg.Go(func() {
			for range 200 {
				got := buf.observeRecord(rec)
				if got.ModelCanon != "first" && got.ModelCanon != "second" {
					t.Errorf("concurrent observer result = %v, want an installed provider", got.ModelCanon)
					return
				}
			}
		})
	}
	wg.Wait()
	buf.SetModelObserver(first)
	if got := buf.observerModels(false, []*Record{rec}); got != "first" {
		t.Fatalf("final observer = %v, want first", got)
	}
}

// observerModels and the SSE serialization helpers must not take the ring
// lock: a concurrent Record writing to the ring may hold it while every
// dashboard event is being encoded. This regression holds the write lock and
// requires the observer path to complete; the pre-atomic implementation blocks
// on RLock until the timeout.
func TestModelObserverDoesNotAcquireRingLock(t *testing.T) {
	buf := NewBuffer(4)
	buf.SetModelObserver(func(bool, ...[]*Record) any { return "installed" })
	rec := &Record{ID: "locked", Model: "MODEL", Start: time.UnixMilli(1_800_000_000_000)}

	buf.mu.Lock()
	defer buf.mu.Unlock()
	done := make(chan ObservedRecord, 1)
	go func() { done <- buf.observeRecord(rec) }()
	select {
	case got := <-done:
		if got.ModelCanon != "installed" {
			t.Fatalf("observer while ring locked = %v, want installed", got.ModelCanon)
		}
	case <-time.After(time.Second):
		t.Fatal("observerModels blocked on the ring write lock")
	}
}

func TestModelObserverNilResult(t *testing.T) {
	buf := NewBuffer(1)
	if got := buf.observerModels(false, nil); got != nil {
		t.Fatalf("unset observer = %v, want nil", got)
	}
	buf.SetModelObserver(func(bool, ...[]*Record) any { return "installed" })
	buf.SetModelObserver(nil)
	if got := buf.observerModels(false, nil); got != nil {
		t.Fatalf("cleared observer = %v, want nil", got)
	}
}

func BenchmarkBufferObserverEvent(b *testing.B) {
	buf := NewBuffer(1)
	buf.SetModelObserver(func(bool, ...[]*Record) any { return "installed" })
	rec := &Record{ID: "record", Model: "MODEL", Start: time.UnixMilli(1_800_000_000_000), StatusCode: 200}
	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = buf.observeRecord(rec)
		}
	})
	// Proves the event path is independent of ring mutation: a Record holding
	// the write lock can never block observer serialization.
	b.Run("ring_write_locked", func(b *testing.B) {
		buf.mu.Lock()
		defer buf.mu.Unlock()
		b.ReportAllocs()
		for b.Loop() {
			_ = buf.observeRecord(rec)
		}
	})
}
