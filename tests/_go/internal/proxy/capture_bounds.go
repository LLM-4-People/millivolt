package proxy

import (
	"fmt"
	"net/http"
	"reflect"
	"runtime"
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

// TestClipClientMetaRetainsValidUTF8 pins the client-meta owner's use of the
// shared rune-safe truncator: every cut stays valid UTF-8 and within the
// 64-byte bound, invalid input bytes are coerced first, and short/normal values
// remain byte-identical.
func TestClipClientMetaRetainsValidUTF8(t *testing.T) {
	for _, want := range []string{"Linux", "x64", "CPython", "3.12.8", strings.Repeat("a", clientMetaMaxLen)} {
		if got := clipClientMeta(want); got != want {
			t.Fatalf("clipClientMeta(%q) = %q, want it unchanged", want, got)
		}
	}
	if got := clipClientMeta("  Linux  "); got != "Linux" {
		t.Fatalf("trimmed meta = %q, want Linux", got)
	}
	for _, in := range []string{
		"a" + strings.Repeat("é", 40), // 81 bytes: byte 64 splits a rune
		strings.Repeat("\xffa", 100),  // invalid runs expand when coerced
		"a" + strings.Repeat("é", 40) + "\xff",
	} {
		got := clipClientMeta(in)
		if len(got) > clientMetaMaxLen {
			t.Fatalf("in=%d bytes: retained %d bytes, want <= %d", len(in), len(got), clientMetaMaxLen)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("in=%d bytes: retained invalid UTF-8 % x", len(in), []byte(got[len(got)-4:]))
		}
		coerced := strings.ToValidUTF8(strings.TrimSpace(in), "\uFFFD")
		if len(coerced) > clientMetaMaxLen && !strings.HasSuffix(got, retainedTruncationSuffix) {
			t.Fatalf("in=%d bytes: retained value does not end with the truncation marker", len(in))
		}
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
	tokenName := "X-" + strings.Repeat("t", 1<<20) + "-token"
	ordinaryName := "X-" + strings.Repeat("n", 1<<20)
	h := http.Header{
		"X-Oversized":   {strings.Repeat("a", 1<<20)},
		"X-Many":        many,
		"Authorization": {"Bearer secret"},
		tokenName:       {"hostile credential name"},
		ordinaryName:    {"hostile ordinary name"},
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
	// An overlong name is skipped, never truncated into a colliding map key.
	for _, name := range []string{tokenName, ordinaryName} {
		if _, ok := out[name]; ok {
			t.Fatalf("overlong header name of %d bytes was retained", len(name))
		}
	}
	// The whole retained map stays under one total budget, so many oversized
	// names cannot multiply the per-value cap. Names and the fixed redaction
	// marker are charged like ordinary values.
	total := 0
	for name, values := range out {
		if len(name) > retainedHeaderNameMax {
			t.Fatalf("retained header name of %d bytes, want <= %d", len(name), retainedHeaderNameMax)
		}
		for _, v := range values {
			total += len(name) + len(v)
		}
	}
	if total > retainedHeadersMaxBytes {
		t.Fatalf("retained header bytes = %d, want <= %d", total, retainedHeadersMaxBytes)
	}
}

// TestCaptureHeadersBoundsEntryCount pins the retained-entry cap separately
// from the text budget: an upstream can send tens of thousands of short unique
// names that all fit the 16 KiB text budget, so without an entry cap the
// retained map (and its bucket array) would scale with the upstream header
// count. Selection stays deterministic (sorted names), ordinary and redacted
// headers keep their exact bytes, and the retained heap stays bounded.
func TestCaptureHeadersBoundsEntryCount(t *testing.T) {
	normal := http.Header{
		"Content-Type":  {"application/json"},
		"X-Request-Id":  {"abc-123"},
		"Authorization": {"Bearer secret"},
	}
	want := http.Header{
		"Content-Type":  {"application/json"},
		"X-Request-Id":  {"abc-123"},
		"Authorization": {"[REDACTED]"},
	}
	if got := captureHeaders(normal, "Authorization"); !reflect.DeepEqual(got, want) {
		t.Fatalf("normal/redacted headers changed: got %#v, want %#v", got, want)
	}

	build := func(n int) http.Header {
		h := make(http.Header, n)
		for i := 0; i < n; i++ {
			h[fmt.Sprintf("x%05d", i)] = []string{"v"}
		}
		return h
	}
	atCap := captureHeaders(build(retainedHeaderEntriesMax), "Authorization")
	if len(atCap) != retainedHeaderEntriesMax {
		t.Fatalf("retained entries = %d, want the full cap %d", len(atCap), retainedHeaderEntriesMax)
	}

	huge := build(60000)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := captureHeaders(huge, "Authorization")
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(huge)
	runtime.KeepAlive(out)
	if len(out) != retainedHeaderEntriesMax {
		t.Fatalf("60k-name header map retained %d entries, want the cap %d", len(out), retainedHeaderEntriesMax)
	}
	for _, name := range []string{"x00000", "x00255"} {
		if _, ok := out[name]; !ok {
			t.Fatalf("sorted selection did not retain %s", name)
		}
	}
	if _, ok := out["x00256"]; ok {
		t.Fatal("retained an entry beyond the sorted cap")
	}
	// A map sized from the 60k input used about 6.1 MiB of retained buckets
	// for the same text budget; the cap keeps the whole retained map well
	// under this bound. The signed delta tolerates GC releasing unrelated
	// benchmark allocations (HeapAlloc is unsigned).
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("60k-name header map retained %d entries in %d heap bytes", len(out), heap)
	if heap > 512<<10 {
		t.Fatalf("60k-name retained heap = %d bytes, want <= %d", heap, 512<<10)
	}
}

// TestCaptureHeadersRedactedNamesShareBudget pins the redacted branch's budget:
// a flood of credential-bearing names keeps the presence marker for a bounded
// subset, and the name plus marker cost never exceeds the map total.
func TestCaptureHeadersRedactedNamesShareBudget(t *testing.T) {
	const count = 40
	h := make(http.Header, count)
	for i := range count {
		name := fmt.Sprintf("X-%s-token-%02d", strings.Repeat("a", 480), i)
		h[name] = []string{"credential"}
	}
	out := captureHeaders(h, "Authorization")
	if len(out) == 0 || len(out) == count {
		t.Fatalf("redacted headers retained = %d, want a bounded subset of %d", len(out), count)
	}
	total := 0
	for name, values := range out {
		if len(values) != 1 || values[0] != "[REDACTED]" {
			t.Fatalf("%s = %v, want the redaction marker", name, values)
		}
		total += len(name) + len(values[0])
	}
	if total > retainedHeadersMaxBytes {
		t.Fatalf("retained redacted header bytes = %d, want <= %d", total, retainedHeadersMaxBytes)
	}
}
