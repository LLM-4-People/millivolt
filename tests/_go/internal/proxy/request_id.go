package proxy

import (
	"sync"
	"testing"
	"time"
)

// TestRequestIDIsTimeOrderedAndUnique pins the storage-locality contract: the
// same 26-character opaque shape, a leading time prefix that advances
// lexicographically, and uniqueness under concurrency.
func TestRequestIDIsTimeOrderedAndUnique(t *testing.T) {
	first := requestID()
	if len(first) != 26 {
		t.Fatalf("id length = %d, want 26", len(first))
	}
	time.Sleep(2 * time.Millisecond)
	second := requestID()
	if second <= first {
		t.Fatalf("id order did not advance with time: %q then %q", first, second)
	}
	const n = 4096
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ids[i] = requestID() })
	}
	wg.Wait()
	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if len(id) != 26 {
			t.Fatalf("id %q length = %d", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}
