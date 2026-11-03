package scheduler

// Regression tests for operator queue draining: a drain releases the parked
// admission waiters that match its scope, leaves non-matching waiters queued,
// and never touches an in-flight request.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainQueueReleasesMatchingWaiters(t *testing.T) {
	s := New(Options{MaxQueueSize: 100})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	holder, err := s.AcquireWith(context.Background(), "p|holder", 0, WaiterHooks{Provider: "p", Client: "c0", Model: "m0"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder(0)

	type result struct {
		err error
	}
	wait := func(client, model string) chan result {
		ch := make(chan result, 1)
		go func() {
			_, err := s.AcquireWith(context.Background(), "p|"+client, 0,
				WaiterHooks{Provider: "p", Client: client, Model: model})
			ch <- result{err: err}
		}()
		return ch
	}
	matching := wait("c1", "m1")
	other := wait("c2", "m2")
	waitFor(t, func() bool { return s.Stats().Queued == 2 }, "two waiters queued")

	// Drain only c1: it must return ErrQueueDrained while c2 stays queued.
	s.DrainQueue(DrainScope{Client: "c1"})
	select {
	case got := <-matching:
		if !errors.Is(got.err, ErrQueueDrained) {
			t.Fatalf("drained waiter error = %v, want ErrQueueDrained", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching waiter was not released by the drain")
	}
	select {
	case got := <-other:
		t.Fatalf("non-matching waiter returned early: %v", got.err)
	case <-time.After(50 * time.Millisecond):
	}
	if queued := s.Stats().Queued; queued != 1 {
		t.Fatalf("queued after drain = %d, want 1", queued)
	}

	// Draining all releases the remaining waiter too.
	s.DrainQueue(DrainScope{})
	select {
	case got := <-other:
		if !errors.Is(got.err, ErrQueueDrained) {
			t.Fatalf("drained waiter error = %v, want ErrQueueDrained", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain-all did not release the remaining waiter")
	}
}

func TestDrainQueueMatchesProviderAndModel(t *testing.T) {
	s := New(Options{MaxQueueSize: 100})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	s.SetThrottle(Throttle{Provider: "q", Limit: Limit{Concurrency: 1}})
	holder, err := s.AcquireWith(context.Background(), "p|holder", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder(0)
	holderQ, err := s.AcquireWith(context.Background(), "q|holder", 0, WaiterHooks{Provider: "q"})
	if err != nil {
		t.Fatal(err)
	}
	defer holderQ(0)

	byModel := make(chan error, 1)
	go func() {
		_, err := s.AcquireWith(context.Background(), "p|m", 0, WaiterHooks{Provider: "p", Client: "c", Model: "target"})
		byModel <- err
	}()
	byProvider := make(chan error, 1)
	go func() {
		_, err := s.AcquireWith(context.Background(), "q|k", 0, WaiterHooks{Provider: "q", Client: "c", Model: "other"})
		byProvider <- err
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 2 }, "waiters queued")

	// A provider-scoped drain releases only the provider's waiter.
	s.DrainQueue(DrainScope{Provider: "p"})
	select {
	case err := <-byModel:
		if !errors.Is(err, ErrQueueDrained) {
			t.Fatalf("provider drain error = %v, want ErrQueueDrained", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider-scoped drain did not release the provider waiter")
	}
	select {
	case err := <-byProvider:
		t.Fatalf("other provider released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.DrainQueue(DrainScope{})
	select {
	case err := <-byProvider:
		if !errors.Is(err, ErrQueueDrained) {
			t.Fatalf("drain-all error = %v, want ErrQueueDrained", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain-all did not release the remaining provider waiter")
	}
}
