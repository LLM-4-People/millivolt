package proxy

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRetainedTextBounds pins the record-retention caps: every real identifier
// passes unchanged, an oversized value is cut at a UTF-8 boundary and marked,
// and the result never exceeds the requested bound.
func TestRetainedTextBounds(t *testing.T) {
	if got := retainedText("claude-sonnet-4", retainedLabelMax); got != "claude-sonnet-4" {
		t.Fatalf("short label changed: %q", got)
	}
	long := strings.Repeat("é", retainedLabelMax)
	got := retainedText(long, retainedLabelMax)
	if len(got) > retainedLabelMax || !utf8.ValidString(got) || !strings.HasSuffix(got, retainedTruncationSuffix) {
		t.Fatalf("bounded text len=%d valid=%v suffix=%v", len(got), utf8.ValidString(got), strings.HasSuffix(got, retainedTruncationSuffix))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, retainedTruncationSuffix)) {
		t.Fatal("truncation did not preserve a prefix")
	}
}

// TestCaptureHeadersBoundsHostileValues pins the per-record header bound: a
// hostile megabyte header cannot be retained whole, the per-name value count
// is bounded, and credential redaction still wins.
func TestCaptureHeadersBoundsHostileValues(t *testing.T) {
	many := make([]string, retainedHeaderValuesMax+4)
	for i := range many {
		many[i] = "v"
	}
	h := http.Header{
		"X-Oversized":   {strings.Repeat("a", 1<<20)},
		"X-Many":        many,
		"Authorization": {"Bearer secret"},
	}
	out := captureHeaders(h, "Authorization")
	if got := len(out["X-Oversized"][0]); got > retainedHeaderValueMax {
		t.Fatalf("oversized header retained %d bytes", got)
	}
	if got := len(out["X-Many"]); got != retainedHeaderValuesMax {
		t.Fatalf("retained %d values, want %d", got, retainedHeaderValuesMax)
	}
	if got := out["Authorization"]; len(got) != 1 || got[0] != "[REDACTED]" {
		t.Fatalf("credential redaction lost: %v", got)
	}
	// The whole retained map stays under one total budget, so many oversized
	// names cannot multiply the per-value cap.
	total := 0
	for name, values := range out {
		for _, v := range values {
			total += len(name) + len(v)
		}
	}
	if total > retainedHeadersMaxBytes {
		t.Fatalf("retained header bytes = %d, want <= %d", total, retainedHeadersMaxBytes)
	}
}
