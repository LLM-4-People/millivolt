package metrics

import (
	"encoding/json"
	"runtime"
	"testing"
)

// contentAllocationMax is the hard byte ceiling for one bounded content
// decode. A typed slice of a million empty parts costs hundreds of MiB; the
// lexical owner must stay within the body's own footprint.
const contentAllocationMax = 16 << 20

// metricsContentParts builds one JSON content array of n empty objects.
func metricsContentParts(n int) []byte {
	body := make([]byte, 0, n*3+1)
	body = append(body, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '{', '}')
	}
	body = append(body, ']')
	return body
}

// contentAllocDelta measures bytes allocated by fn without timing it.
func contentAllocDelta(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestContentPartsWithinLimit pins the lexical bound: ordinary and boundary
// arrays pass, malformed arrays do not, and the over-bound case is rejected.
func TestContentPartsWithinLimit(t *testing.T) {
	atCap := metricsContentParts(ContentPartMax)
	overCap := metricsContentParts(ContentPartMax + 1)
	for _, tc := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{"plain string is not a parts array", []byte(`"hello"`), true},
		{"null is not a parts array", []byte(`null`), true},
		{"empty array", []byte(`[]`), true},
		{"nested object and whitespace", []byte(" \n[ { \"type\": \"text\", \"text\": \"a]b\" } ]\t"), true},
		{"escaped quotes", []byte(`[{"type":"text","text":"a\"b"}]`), true},
		{"scalar members count", []byte(`[1,2,3]`), true},
		{"array at the cap", atCap, true},
		{"array over the cap", overCap, false},
		{"unterminated array", []byte(`[{"type":"text"`), false},
		{"trailing comma", []byte(`[{"type":"text"},]`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContentPartsWithinLimit(tc.raw); got != tc.want {
				t.Fatalf("ContentPartsWithinLimit(%d bytes) = %v, want %v", len(tc.raw), got, tc.want)
			}
		})
	}

	small := []byte(`[1,2,3]`)
	if !ArrayWithinLimit(small, 3) || ArrayWithinLimit(small, 2) {
		t.Fatal("ArrayWithinLimit did not honor a custom bound")
	}

	heavy := metricsContentParts(1_000_000)
	var within bool
	alloc := contentAllocDelta(func() { within = ContentPartsWithinLimit(heavy) })
	if within {
		t.Fatalf("million-part array reported within the %d-part limit", ContentPartMax)
	}
	if alloc > contentAllocationMax {
		t.Fatalf("ContentPartsWithinLimit allocated %d bytes for a %d-byte array, want <= %d",
			alloc, len(heavy), contentAllocationMax)
	}
	t.Logf("ContentPartsWithinLimit over-bound: input=%d alloc=%d", len(heavy), alloc)
}

// TestFlattenContentBoundsPartArrays preserves the normal flatten contract and
// proves an over-bound array is skipped without materializing its parts.
func TestFlattenContentBoundsPartArrays(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"bare string", `"plain"`, "plain", true},
		{"text parts join", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab", true},
		{"mixed keeps text parts", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]`, "a", true},
		{"null decodes empty", `null`, "", true},
		{"empty array", `[]`, "", true},
		{"object is not a parts array", `{"text":"nope"}`, "", false},
		{"non-string part text fails", `[{"text":123}]`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FlattenContent(json.RawMessage(tc.raw))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("FlattenContent(%s) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}

	heavy := metricsContentParts(1_000_000)
	var got string
	var ok bool
	alloc := contentAllocDelta(func() { got, ok = FlattenContent(heavy) })
	if got != "" || ok {
		t.Fatalf("over-bound FlattenContent() = (%q, %v), want empty best-effort skip", got, ok)
	}
	if alloc > contentAllocationMax {
		t.Fatalf("FlattenContent allocated %d bytes for a %d-byte over-bound array, want <= %d",
			alloc, len(heavy), contentAllocationMax)
	}
	t.Logf("FlattenContent over-bound: input=%d alloc=%d", len(heavy), alloc)
}

// TestContentPresentBoundsPartArrays keeps the presence semantics (any member
// is content) while checking presence lexically instead of materializing every
// member of a hostile answer array.
func TestContentPresentBoundsPartArrays(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"non-empty string", `"x"`, true},
		{"empty string", `""`, false},
		{"null", `null`, false},
		{"empty array", `[]`, false},
		{"one part", `[{"type":"text","text":"a"}]`, true},
		{"many scalar parts", `[1,2,3]`, true},
		{"unterminated array", `[{"type":"text"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentPresent(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("contentPresent(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}

	heavy := metricsContentParts(1_000_000)
	var present bool
	alloc := contentAllocDelta(func() { present = contentPresent(heavy) })
	if !present {
		t.Fatal("over-bound non-empty array lost content presence")
	}
	if alloc > contentAllocationMax {
		t.Fatalf("contentPresent allocated %d bytes for a %d-byte array, want <= %d",
			alloc, len(heavy), contentAllocationMax)
	}
	t.Logf("contentPresent over-bound: input=%d alloc=%d", len(heavy), alloc)
}
