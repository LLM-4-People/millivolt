package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCountDebugSessionIgnoresExpired pins the operator-visible count to the
// same expiry predicate as LoadDebugCapture. Rows kept for physical
// reclamation must not inflate a session's capture count after they are
// already unfetchable.
func TestCountDebugSessionIgnoresExpired(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "debug-count.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now()
	rows := []struct {
		id      string
		expires int64
	}{
		{"live", now.Add(time.Hour).UnixMilli()},
		{"expired", now.Add(-time.Minute).UnixMilli()},
		{"forever", 0},
	}
	for _, row := range rows {
		if err := s.SaveDebugCapture(t.Context(), row.id, "sess", now.UnixMilli(), row.expires, []byte("capture")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CountDebugSession(t.Context(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("CountDebugSession = %d, want 2 (expired row must not count)", got)
	}
	got, err = s.CountDebugSession(t.Context(), "")
	if err != nil || got != 0 {
		t.Fatalf("empty session count = %d, %v", got, err)
	}
}
