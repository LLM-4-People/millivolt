package proxy

import "testing"

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
