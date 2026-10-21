package metrics

import (
	"testing"
	"time"
)

// TestTTFTIsPerAttempt pins the one TTFT definition: from the adopted upstream
// attempt's send start to the first content token. Start (upload/queue/parse)
// never moves the anchor, so a retry has its own TTFT.
func TestTTFTIsPerAttempt(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	r := &Record{
		Start:          base,
		FinalAttemptAt: base.Add(2 * time.Second), // queued/parsed before this
		FirstTokenAt:   base.Add(2300 * time.Millisecond),
	}
	if got := r.TTFT(); got != 300*time.Millisecond {
		t.Errorf("TTFT = %v, want 300ms from the attempt send, not 2300ms from Start", got)
	}

	// A non-streamed response has no first-token observation.
	if got := (&Record{Start: base, FinalAttemptAt: base}).TTFT(); got != 0 {
		t.Errorf("TTFT = %v, want 0 with no first token", got)
	}
	// No upstream attempt was sent: absent, never a Start-anchored fallback.
	if got := (&Record{Start: base, FirstTokenAt: base.Add(time.Second)}).TTFT(); got != 0 {
		t.Errorf("TTFT = %v, want 0 with no attempt anchor", got)
	}
	// A first token before the adopted attempt is not a valid window.
	if got := (&Record{FinalAttemptAt: base.Add(time.Second), FirstTokenAt: base}).TTFT(); got != 0 {
		t.Errorf("TTFT = %v, want 0 for a negative window", got)
	}
}

// TestFinalizeTTFTSubMillisecond keeps the presence signal: a positive TTFT
// below 1ms rounds up to 1ms, while an absent TTFT stays 0.
func TestFinalizeTTFTSubMillisecond(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Record{
		Start:          base,
		FinalAttemptAt: base,
		FirstTokenAt:   base.Add(400 * time.Microsecond),
		GenTokens:      2,
		LastTokenAt:    base.Add(time.Millisecond),
	}
	FinalizeRecord(r)
	if r.TTFTMs != 1 {
		t.Errorf("TTFTMs = %d, want 1 for a positive sub-ms TTFT", r.TTFTMs)
	}

	r2 := &Record{Start: base, FinalAttemptAt: base}
	FinalizeRecord(r2)
	if r2.TTFTMs != 0 {
		t.Errorf("TTFTMs = %d, want 0 when no token arrived", r2.TTFTMs)
	}
}
