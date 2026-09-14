package storage

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ErrStorageDisabled is the one identity behind every "no durable store"
// refusal, whatever local wording a surface wraps it with: the nil-store
// snapshot/merge helpers, the backup/restore senders in cmd/proxy, the
// 503 query endpoint, and the 404 debug-capture fetch. Each surface keeps
// its own status code; errors.Is must work at every value-returning one.
func TestStorageDisabledSentinelIdentity(t *testing.T) {
	var s *Store
	if _, err := s.Snapshot(t.Context()); !errors.Is(err, ErrStorageDisabled) {
		t.Errorf("Snapshot on nil store: %v, want ErrStorageDisabled", err)
	}
	if _, err := s.SnapshotOverlap(t.Context(), nil); !errors.Is(err, ErrStorageDisabled) {
		t.Errorf("SnapshotOverlap on nil store: %v, want ErrStorageDisabled", err)
	}
	if _, _, err := s.MergeSnapshot(t.Context(), nil); !errors.Is(err, ErrStorageDisabled) {
		t.Errorf("MergeSnapshot on nil store: %v, want ErrStorageDisabled", err)
	}

	// The read-only SQL endpoint keeps its 503 and answers with the
	// sentinel's canonical message even on the body-only surface.
	w := httptest.NewRecorder()
	s.HandleQuery(w, httptest.NewRequest(http.MethodGet, "/metrics/query?q=SELECT+1", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("query without storage: status=%d, want 503", w.Code)
	}
	if !errors.Is(ErrStorageDisabled, ErrStorageDisabled) || w.Body.String() != "{\"error\":\"durable storage is disabled\"}\n" {
		t.Fatalf("query without storage: body=%q, want the sentinel's canonical message", w.Body.String())
	}
}
