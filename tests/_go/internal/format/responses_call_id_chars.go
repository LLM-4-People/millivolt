package format

// Round 7 H2 regression: Responses call ids are bounded by JSON Schema
// maxLength, which counts characters (RFC 8259 code points), not UTF-8 bytes.
// A 64-rune multibyte id is valid even when it exceeds 64 bytes.

import (
	"strings"
	"testing"
)

func TestResponsesCallIDCountsCharacters(t *testing.T) {
	for _, field := range []string{"tool call id", "tool result call id"} {
		for _, tc := range []struct {
			name string
			id   string
			ok   bool
		}{
			{"empty", "", false},
			{"64 ascii", strings.Repeat("a", 64), true},
			{"65 ascii", strings.Repeat("a", 65), false},
			{"64 two-byte runes", strings.Repeat("é", 64), true},
			{"65 two-byte runes", strings.Repeat("é", 65), false},
			{"64 four-byte runes", strings.Repeat("🙂", 64), true},
			{"65 four-byte runes", strings.Repeat("🙂", 65), false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				err := validateResponsesToolReference(field, tc.id)
				if tc.ok && err != nil {
					t.Fatalf("id with %d bytes and %d runes rejected: %v", len(tc.id), len([]rune(tc.id)), err)
				}
				if !tc.ok && err == nil {
					t.Fatalf("id with %d bytes and %d runes accepted", len(tc.id), len([]rune(tc.id)))
				}
				if err != nil && !strings.Contains(err.Error(), "1..64 characters") {
					t.Fatalf("error = %v, want the character bound", err)
				}
			})
		}
	}
}
