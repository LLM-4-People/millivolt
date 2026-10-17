package proxy

import "testing"

func TestInflightLimiterBoundsAndReloads(t *testing.T) {
	var l inflightLimiter
	l.set(2)
	if !l.acquire() || !l.acquire() {
		t.Fatal("admission below the bound failed")
	}
	if l.acquire() {
		t.Fatal("admission above the bound succeeded")
	}
	l.release()
	if !l.acquire() {
		t.Fatal("released slot not reusable")
	}
	l.set(0) // unlimited: a live reload must not strand capacity
	if !l.acquire() || !l.acquire() {
		t.Fatal("unlimited limiter refused")
	}
	l.release()
	l.release()
	l.release()
	l.release()
	if got := l.cur.Load(); got != 0 {
		t.Fatalf("counter = %d, want 0", got)
	}
}
