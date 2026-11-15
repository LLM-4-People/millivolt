package proxy

// W2 regression: the O(1) data-head scan used to accept any non-whitespace
// scalar as a complete document, so two plain-text data lines ("hello" then
// "world") got a synthetic separator and were split into two events. The scan
// must answer exactly like json.Valid, which only a JSON starter byte can
// satisfy, and a plain-text payload must stay one event.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHeadScanMatchesJSONValid differs the scanner verdict against json.Valid:
// the scan exists only to replace the per-line json.Valid rescan, so the two
// must agree on every head. The [DONE] sentinel is excluded because
// separateBefore owns that branch, which the last case pins.
func TestHeadScanMatchesJSONValid(t *testing.T) {
	corpus := []struct{ name, head string }{
		{"plain word", "hello"},
		{"two plain words", "hello\nworld"},
		{"comma words", "a,b"},
		{"integer", "123"},
		{"negative float", "-1.5"},
		{"string", `"x"`},
		{"true", "true"},
		{"null", "null"},
		{"empty object", "{}"},
		{"empty array", "[]"},
		{"pretty object", "{\n  \"a\": 1\n}"},
		{"padded scalar", "  123  "},
		{"crlf tail", "{\"a\":1}\r\n"},
		{"member fragment", `{"a":1,`},
		{"open object", `{"a":`},
		{"unterminated string", `{"a":"x`},
	}
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			var h headScan
			h.add([]byte(tc.head))
			if got, want := h.complete(), json.Valid([]byte(tc.head)); got != want {
				t.Fatalf("headScan.complete(%q) = %v, json.Valid = %v", tc.head, got, want)
			}
		})
	}
	// A bare [DONE] is not JSON but must still prove complete: the sentinel
	// always takes its own separator.
	var h headScan
	h.add([]byte("[DONE]"))
	if !h.complete() {
		t.Fatal("headScan.complete([DONE]) = false, want the sentinel to separate")
	}
}

// TestStreamPlainTextEventStaysOneEvent pins the wire: the data lines of one
// plain-text event are joined, and the [DONE] sentinel still separates from a
// scalar that precedes it.
func TestStreamPlainTextEventStaysOneEvent(t *testing.T) {
	body := []byte("data: hello\ndata: world\n\n")
	got, _ := streamPrettyBodyTime(t, body)
	if strings.Contains(got, "data: hello\n\ndata: world") {
		t.Fatalf("wire = %q, want the two plain-text lines in one event", got)
	}
	if !strings.Contains(got, "data: hello\ndata: world\n") {
		t.Fatalf("wire = %q, want the two plain-text lines joined", got)
	}
}

func TestStreamDoneSeparatesFromAScalar(t *testing.T) {
	body := []byte("data: 123\ndata: [DONE]\n\n")
	got, _ := streamPrettyBodyTime(t, body)
	if strings.Contains(got, "data: 123\ndata: [DONE]") {
		t.Fatalf("wire = %q, want a separator before [DONE]", got)
	}
	if !strings.Contains(got, "\n\ndata: [DONE]") {
		t.Fatalf("wire = %q, want [DONE] in its own event", got)
	}
}
