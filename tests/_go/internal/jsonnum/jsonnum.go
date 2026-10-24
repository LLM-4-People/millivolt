package jsonnum

// Round 7 verifier D1: one owner decides which JSON number spellings are
// integral. These cases pin the exact grammar boundaries shared by the proxy
// trust boundary and the three wire decoders.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseIntegralSpellings(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int
	}{
		{"0", 0},
		{"-0", 0},
		{"0.0", 0},
		{"0e999", 0},
		{"1", 1},
		{"-1", -1},
		{"1000000", 1000000},
		{"1e6", 1000000},
		{"1E+6", 1000000},
		{"1000000.0", 1000000},
		{"1000000.000", 1000000},
		{"10000e2", 1000000},
		{"-1000000", -1000000},
		{"1000000000000000000", 1000000000000000000},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			got, err := Parse([]byte(tc.literal))
			if err != nil {
				t.Fatalf("Parse(%s) error = %v", tc.literal, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%s) = %d, want %d", tc.literal, got, tc.want)
			}
		})
	}
}

func TestParseRejectsNonIntegralValues(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    error
	}{
		{"", ErrNotNumber},
		{"null", ErrNotNumber},
		{"true", ErrNotNumber},
		{`"1000000"`, ErrNotNumber},
		{"1e", ErrNotNumber},
		{"1.", ErrNotNumber},
		{".5", ErrNotNumber},
		{"1.5", ErrNotInteger},
		{"0.5", ErrNotInteger},
		{"1e-1", ErrNotInteger},
		{"1.0000001", ErrNotInteger},
		{"99999999999999999999", ErrOutOfRange},
		{"1e99", ErrOutOfRange},
		{"1e999999999999999999999", ErrOutOfRange},
		{"-99999999999999999999", ErrOutOfRange},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			if _, err := Parse([]byte(tc.literal)); !errors.Is(err, tc.want) {
				t.Fatalf("Parse(%s) error = %v, want %v", tc.literal, err, tc.want)
			}
		})
	}
}

func TestOptionalIntPresence(t *testing.T) {
	if got, err := OptionalInt(nil, "max_tokens"); err != nil || got != nil {
		t.Fatalf("absent member = %v, %v; want nil, nil", got, err)
	}
	if got, err := OptionalInt(json.RawMessage("null"), "max_tokens"); err != nil || got != nil {
		t.Fatalf("null member = %v, %v; want nil, nil", got, err)
	}
	got, err := OptionalInt(json.RawMessage("1e6"), "max_tokens")
	if err != nil || got == nil || *got != 1000000 {
		t.Fatalf("1e6 member = %v, %v; want 1000000", got, err)
	}
	if _, err := OptionalInt(json.RawMessage("1.5"), "max_tokens"); err == nil ||
		!strings.Contains(err.Error(), "max_tokens") || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("fractional member error = %v, want the field-named rejection", err)
	}
}
