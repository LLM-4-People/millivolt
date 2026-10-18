package storage

import (
	"strings"
	"testing"
	"time"
)

// TestDebugCaptureStoreCap pins the total sidecar budget for live rows:
// captures are refused before exceeding the cap, a smaller replacement frees
// room, and a later capture can then fit. Expired-row reclamation at the cap is
// covered separately by TestSaveDebugCaptureReclaimsExpiredBytesAtCap.
func TestDebugCaptureStoreCap(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	s, err := Open(t.TempDir()+"/debug-cap.db", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	big := make([]byte, 600<<10)
	if err := s.SaveDebugCapture(t.Context(), "a", "sess", now.UnixMilli(), 0, big); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDebugCapture(t.Context(), "b", "sess", now.UnixMilli(), 0, big); err == nil {
		t.Fatal("second capture exceeded the cap but was accepted")
	} else if !strings.Contains(err.Error(), "store limit") {
		t.Fatalf("cap error = %v", err)
	}
	// Replacing an existing row only charges the delta, so it can free room.
	if err := s.SaveDebugCapture(t.Context(), "a", "sess", now.UnixMilli(), 0, make([]byte, 100<<10)); err != nil {
		t.Fatalf("smaller replacement: %v", err)
	}
	if err := s.SaveDebugCapture(t.Context(), "b", "sess", now.UnixMilli(), 0, big); err != nil {
		t.Fatalf("capture after freeing room: %v", err)
	}
	if err := s.SaveDebugCapture(t.Context(), "c", "sess", now.UnixMilli(), 0, big); err == nil {
		t.Fatal("capture over the cap after two live rows was accepted")
	}
}
