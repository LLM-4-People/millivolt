package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Record-retention contract. The bounds are internal guardrails, not config:
// they are pinned here independently of the implementation constants so a
// silent widening or narrowing fails the behavior tests.
const (
	retentionContractPathMax          = 1024
	retentionContractMethodMax        = 32
	retentionContractProviderTextMax  = 512
	retentionContractToolNameMax      = 128
	retentionContractToolNamesMax     = 64
	retentionContractDebugSessionMax  = 128
	retentionContractTruncationMarker = "…"
)

type retentionTextField struct {
	name string
	max  int
	set  func(*Record, string)
	get  func(*Record) string
}

func retentionTextFields() []retentionTextField {
	return []retentionTextField{
		{"path", retentionContractPathMax, func(r *Record, v string) { r.Path = v }, func(r *Record) string { return r.Path }},
		{"method", retentionContractMethodMax, func(r *Record, v string) { r.Method = v }, func(r *Record) string { return r.Method }},
		{"model", retentionContractProviderTextMax, func(r *Record, v string) { r.Model = v }, func(r *Record) string { return r.Model }},
		{"provider model", retentionContractProviderTextMax, func(r *Record, v string) { r.ProviderModel = v }, func(r *Record) string { return r.ProviderModel }},
		{"provider request id", retentionContractProviderTextMax, func(r *Record, v string) { r.ProviderRequestID = v }, func(r *Record) string { return r.ProviderRequestID }},
		{"provider server", retentionContractProviderTextMax, func(r *Record, v string) { r.ProviderServer = v }, func(r *Record) string { return r.ProviderServer }},
		{"finish reason", retentionContractProviderTextMax, func(r *Record, v string) { r.FinishReason = v }, func(r *Record) string { return r.FinishReason }},
		{"error type", retentionContractProviderTextMax, func(r *Record, v string) { r.ErrorType = v }, func(r *Record) string { return r.ErrorType }},
		{"error code", retentionContractProviderTextMax, func(r *Record, v string) { r.ErrorCode = v }, func(r *Record) string { return r.ErrorCode }},
		{"debug session id", retentionContractDebugSessionMax, func(r *Record, v string) { r.DebugSessionID = v }, func(r *Record) string { return r.DebugSessionID }},
	}
}

func assertRetentionBound(t *testing.T, field, got string, max int) {
	t.Helper()
	if len(got) > max {
		t.Fatalf("%s retained %d bytes, want <= %d", field, len(got), max)
	}
	if !strings.HasSuffix(got, retentionContractTruncationMarker) {
		t.Fatalf("%s of %d bytes does not end with the retention marker", field, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("%s of %d bytes is not valid UTF-8", field, len(got))
	}
}

// TestRecordRetentionTextFields pins every record text bound at max-1, max and
// max+1 through the real finalized-record entry point. Values at or below the
// bound stay byte-identical; an overlong value is cut exactly to the bound and
// carries the shared marker.
func TestRecordRetentionTextFields(t *testing.T) {
	for _, tc := range retentionTextFields() {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{tc.max - 1, tc.max, tc.max + 1} {
				value := strings.Repeat("a", size)
				rec := &Record{ID: "field", Provider: "fixture"}
				tc.set(rec, value)
				b := NewBuffer(1)
				b.Record(rec)
				got := tc.get(b.Snapshot()[0])
				if size <= tc.max {
					if got != value {
						t.Fatalf("%d-byte value changed: got %d bytes", size, len(got))
					}
					continue
				}
				assertRetentionBound(t, tc.name, got, tc.max)
				want := strings.Repeat("a", tc.max-len(retentionContractTruncationMarker)) + retentionContractTruncationMarker
				if got != want {
					t.Fatalf("overlong %s = %d bytes, want the exact %d-byte bound", tc.name, len(got), tc.max)
				}
			}
		})
	}
}

// TestRecordRetentionUTF8Boundary proves a cut that lands inside a rune backs
// off to the prior boundary; the result stays valid UTF-8 and within the bound.
func TestRecordRetentionUTF8Boundary(t *testing.T) {
	value := strings.Repeat("é", 1000) // 2000 bytes: the 1021-byte cut splits a rune
	rec := &Record{ID: "utf8", Path: value}
	b := NewBuffer(1)
	b.Record(rec)
	got := b.Snapshot()[0].Path
	assertRetentionBound(t, "path", got, retentionContractPathMax)
	want := strings.Repeat("é", (retentionContractPathMax-len(retentionContractTruncationMarker))/2) + retentionContractTruncationMarker
	if got != want {
		t.Fatalf("UTF-8 cut = %d bytes, want %d", len(got), len(want))
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("UTF-8 cut introduced a replacement rune")
	}
}

func TestRecordRetentionToolNames(t *testing.T) {
	t.Run("normal list unchanged", func(t *testing.T) {
		names := []string{"read", "write", "search"}
		rec := &Record{ID: "normal-tools", ToolNames: names}
		b := NewBuffer(1)
		b.Record(rec)
		if got := b.Snapshot()[0].ToolNames; !slices.Equal(got, names) {
			t.Fatalf("ToolNames = %v, want %v", got, names)
		}
	})

	t.Run("exact count unchanged", func(t *testing.T) {
		names := make([]string, retentionContractToolNamesMax)
		for i := range names {
			names[i] = fmt.Sprintf("tool-%02d", i)
		}
		rec := &Record{ID: "exact-tools", ToolNames: names}
		b := NewBuffer(1)
		b.Record(rec)
		if got := b.Snapshot()[0].ToolNames; !slices.Equal(got, names) {
			t.Fatalf("ToolNames len = %d, want the exact %d-entry list unchanged", len(got), len(names))
		}
	})

	t.Run("name length bounds", func(t *testing.T) {
		for _, size := range []int{retentionContractToolNameMax - 1, retentionContractToolNameMax, retentionContractToolNameMax + 1} {
			name := strings.Repeat("n", size)
			rec := &Record{ID: "tool-name", ToolNames: []string{name}}
			b := NewBuffer(1)
			b.Record(rec)
			got := b.Snapshot()[0].ToolNames
			if len(got) != 1 {
				t.Fatalf("%d-byte tool name list = %d entries", size, len(got))
			}
			if size <= retentionContractToolNameMax {
				if got[0] != name {
					t.Fatalf("%d-byte tool name changed: got %d bytes", size, len(got[0]))
				}
				continue
			}
			assertRetentionBound(t, "tool name", got[0], retentionContractToolNameMax)
			if len(got[0]) != retentionContractToolNameMax {
				t.Fatalf("overlong tool name = %d bytes, want the exact %d-byte bound", len(got[0]), retentionContractToolNameMax)
			}
		}
	})

	t.Run("over-count keeps first 64 in order", func(t *testing.T) {
		names := make([]string, retentionContractToolNamesMax+6)
		for i := range names {
			names[i] = fmt.Sprintf("tool-%02d", i)
		}
		before := slices.Clone(names)
		rec := &Record{ID: "many-tools", ToolCalls: len(names), ToolNames: names}
		b := NewBuffer(1)
		b.Record(rec)
		got := b.Snapshot()[0].ToolNames
		if len(got) != retentionContractToolNamesMax {
			t.Fatalf("ToolNames len = %d, want %d", len(got), retentionContractToolNamesMax)
		}
		for i := range got {
			if got[i] != names[i] {
				t.Fatalf("ToolNames[%d] = %q, want %q", i, got[i], names[i])
			}
		}
		if !slices.Equal(names, before) {
			t.Fatal("caller-owned tool-name slice was mutated")
		}
		if b.Snapshot()[0].ToolCalls != len(names) {
			t.Fatal("tool-call count changed")
		}
	})

	t.Run("overlong name bounded without mutating caller", func(t *testing.T) {
		long := strings.Repeat("t", retentionContractToolNameMax+1)
		names := []string{long, "short"}
		before := slices.Clone(names)
		rec := &Record{ID: "long-tools", ToolNames: names}
		b := NewBuffer(1)
		b.Record(rec)
		got := b.Snapshot()[0].ToolNames
		if len(got) != 2 || got[1] != "short" {
			t.Fatalf("ToolNames = %v, want two ordered entries", got)
		}
		assertRetentionBound(t, "tool name", got[0], retentionContractToolNameMax)
		if !slices.Equal(names, before) {
			t.Fatal("caller-owned tool-name slice was mutated")
		}
	})
}

type attemptRetentionField struct {
	name string
	max  int
	set  func(*RetryAttempt, string)
	get  func(*RetryAttempt) string
}

func attemptRetentionFields() []attemptRetentionField {
	return []attemptRetentionField{
		{"provider model", retentionContractProviderTextMax, func(a *RetryAttempt, v string) { a.ProviderModel = v }, func(a *RetryAttempt) string { return a.ProviderModel }},
		{"provider request id", retentionContractProviderTextMax, func(a *RetryAttempt, v string) { a.ProviderRequestID = v }, func(a *RetryAttempt) string { return a.ProviderRequestID }},
		{"provider server", retentionContractProviderTextMax, func(a *RetryAttempt, v string) { a.ProviderServer = v }, func(a *RetryAttempt) string { return a.ProviderServer }},
		{"error type", retentionContractProviderTextMax, func(a *RetryAttempt, v string) { a.ErrorType = v }, func(a *RetryAttempt) string { return a.ErrorType }},
		{"error code", retentionContractProviderTextMax, func(a *RetryAttempt, v string) { a.ErrorCode = v }, func(a *RetryAttempt) string { return a.ErrorCode }},
	}
}

// TestRecordRetentionAttemptFields bounds every attempt provider/error field
// at the same max-1/max/max+1 contract as the record's own fields.
func TestRecordRetentionAttemptFields(t *testing.T) {
	for _, tc := range attemptRetentionFields() {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{tc.max - 1, tc.max, tc.max + 1} {
				value := strings.Repeat("a", size)
				attempt := RetryAttempt{StatusCode: 500}
				tc.set(&attempt, value)
				rec := &Record{ID: "attempt-field", Attempts: []RetryAttempt{attempt}}
				b := NewBuffer(1)
				b.Record(rec)
				got := tc.get(&b.Snapshot()[0].Attempts[0])
				if size <= tc.max {
					if got != value {
						t.Fatalf("%d-byte attempt value changed: got %d bytes", size, len(got))
					}
					continue
				}
				assertRetentionBound(t, tc.name, got, tc.max)
				want := strings.Repeat("a", tc.max-len(retentionContractTruncationMarker)) + retentionContractTruncationMarker
				if got != want {
					t.Fatalf("overlong %s = %d bytes, want the exact %d-byte bound", tc.name, len(got), tc.max)
				}
			}
		})
	}
}

// TestRecordRetentionAttemptOrderAndMessage pins attempt order and the
// already-bounded message semantics: the record pass never reorders attempts
// and leaves TruncatePreview's output alone.
func TestRecordRetentionAttemptOrderAndMessage(t *testing.T) {
	hostile := strings.Repeat("x", 1<<20)
	message := TruncatePreview(strings.Repeat("m", PreviewMaxBytes+50))
	rec := &Record{ID: "attempt-order", Attempts: []RetryAttempt{
		{StatusCode: 429, ErrorType: "rate_limit_error", ErrorMsg: message, ProviderRequestID: "first"},
		{
			StatusCode: 503, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "upstream unavailable",
			ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
		},
	}}
	b := NewBuffer(1)
	b.Record(rec)
	attempts := b.Snapshot()[0].Attempts
	if len(attempts) != 2 || attempts[0].StatusCode != 429 || attempts[1].StatusCode != 503 {
		t.Fatalf("attempt order = %+v, want [429 503]", attempts)
	}
	if attempts[0].ErrorMsg != message {
		t.Fatalf("bounded attempt message changed: %d bytes", len(attempts[0].ErrorMsg))
	}
	if attempts[1].ErrorMsg != "upstream unavailable" {
		t.Fatalf("attempt message = %q", attempts[1].ErrorMsg)
	}
	if attempts[0].ProviderRequestID != "first" {
		t.Fatalf("short attempt metadata changed: %q", attempts[0].ProviderRequestID)
	}
	for _, field := range attemptRetentionFields() {
		assertRetentionBound(t, field.name, field.get(&attempts[1]), field.max)
	}
}

// TestPublishLiveRecordRetentionBoundsSnapshot proves a pending begin/update
// snapshot is bounded without mutating the live record the request is still
// streaming into.
func TestPublishLiveRecordRetentionBoundsSnapshot(t *testing.T) {
	hostile := strings.Repeat("h", 1<<20)
	toolNames := make([]string, retentionContractToolNamesMax+1)
	for i := range toolNames {
		toolNames[i] = "tool"
	}
	toolNames[0] = hostile
	live := &Record{
		ID: "pending", Path: hostile, Method: hostile, Model: hostile,
		ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
		FinishReason: hostile, ErrorType: hostile, ErrorCode: hostile,
		DebugSessionID: hostile, ToolNames: toolNames,
	}
	b := NewBuffer(2)
	b.PublishLive("begin", live)
	pending := b.PendingRecords()
	if len(pending) != 1 {
		t.Fatalf("pending records = %d, want 1", len(pending))
	}
	for _, field := range retentionTextFields() {
		assertRetentionBound(t, field.name, field.get(pending[0]), field.max)
	}
	if got := pending[0].ToolNames; len(got) != retentionContractToolNamesMax {
		t.Fatalf("pending ToolNames len = %d, want %d", len(got), retentionContractToolNamesMax)
	} else {
		assertRetentionBound(t, "pending tool name", got[0], retentionContractToolNameMax)
		for i := 1; i < len(got); i++ {
			if got[i] != "tool" {
				t.Fatalf("pending ToolNames[%d] = %q, want the original order", i, got[i])
			}
		}
	}
	if live.Path != hostile || live.Method != hostile || live.Model != hostile ||
		live.ProviderModel != hostile || live.ProviderRequestID != hostile ||
		live.ProviderServer != hostile || live.FinishReason != hostile ||
		live.ErrorType != hostile || live.ErrorCode != hostile ||
		live.DebugSessionID != hostile {
		t.Fatal("begin snapshot mutated the live record")
	}
	if len(live.ToolNames) != retentionContractToolNamesMax+1 || live.ToolNames[0] != hostile {
		t.Fatal("begin snapshot mutated the live tool-name slice")
	}

	live.Attempts = []RetryAttempt{{
		StatusCode: 500, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "attempt failed",
		ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
	}}
	b.PublishLive("update", live)
	pending = b.PendingRecords()
	if len(pending) != 1 || len(pending[0].Attempts) != 1 {
		t.Fatalf("pending update attempts = %+v", pending)
	}
	for _, field := range attemptRetentionFields() {
		assertRetentionBound(t, field.name, field.get(&pending[0].Attempts[0]), field.max)
	}
	if pending[0].Attempts[0].ErrorMsg != "attempt failed" {
		t.Fatal("attempt message changed in the pending snapshot")
	}
	if live.Attempts[0].ErrorType != hostile || live.Attempts[0].ProviderModel != hostile ||
		live.Attempts[0].ProviderRequestID != hostile || live.Attempts[0].ProviderServer != hostile ||
		live.Attempts[0].ErrorCode != hostile {
		t.Fatal("update snapshot mutated the live attempt slice")
	}

	data, err := json.Marshal(b.SnapshotSince(0))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(hostile)) {
		t.Fatal("pending JSON retained a hostile 1 MiB value")
	}
}

// TestBackfillRecordRetentionBoundsRestoredRecord proves old oversized rows
// loaded at startup are bounded before they enter the ring, while a normal
// restored record keeps byte-identical JSON.
func TestBackfillRecordRetentionBoundsRestoredRecord(t *testing.T) {
	normal := &Record{
		ID: "normal", Provider: "fixture", Model: "model-a", Path: "/v1/chat/completions",
		Method: "POST", FinishReason: "stop", ToolNames: []string{"read"}, DebugSessionID: "debug-1",
	}
	normalJSON, err := json.Marshal(normal)
	if err != nil {
		t.Fatal(err)
	}
	hostile := strings.Repeat("o", 1<<20)
	old := &Record{
		ID: "old", Path: hostile, Method: hostile, Model: hostile, ProviderModel: hostile,
		ProviderRequestID: hostile, ProviderServer: hostile, FinishReason: hostile,
		ErrorType: hostile, ErrorCode: hostile, DebugSessionID: hostile,
		Attempts: []RetryAttempt{{
			StatusCode: 500, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "old failure",
			ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
		}},
	}
	b := NewBuffer(2)
	b.Backfill([]*Record{normal, old})
	ring := b.Snapshot()
	restoredNormal, err := json.Marshal(ring[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(normalJSON, restoredNormal) {
		t.Fatalf("normal restored record changed:\nbefore=%s\nafter=%s", normalJSON, restoredNormal)
	}
	for _, field := range retentionTextFields() {
		assertRetentionBound(t, field.name, field.get(ring[1]), field.max)
	}
	if len(ring[1].Attempts) != 1 || ring[1].Attempts[0].ErrorMsg != "old failure" {
		t.Fatalf("restored attempts = %+v, want the original message", ring[1].Attempts)
	}
	for _, field := range attemptRetentionFields() {
		assertRetentionBound(t, field.name, field.get(&ring[1].Attempts[0]), field.max)
	}
}

// TestRecordRetentionPreservesNormalRecordBytes pins the common path: a record
// whose values already fit is encoded byte-for-byte as it was before entry.
func TestRecordRetentionPreservesNormalRecordBytes(t *testing.T) {
	rec := &Record{
		ID: "normal", Provider: "fixture", Model: "model-a", Path: "/v1/chat/completions",
		Method: "POST", Client: "fixture-client", ClientIP: "127.0.0.1",
		StatusCode: 200, Start: time.Unix(1700000000, 0).UTC(), End: time.Unix(1700000001, 0).UTC(),
		FinishReason: "stop", ToolNames: []string{"read", "write"}, DebugSessionID: "debug-1",
		Attempts: []RetryAttempt{{
			StatusCode: 429, ErrorType: "rate_limit_error", ErrorMsg: "slow down",
			ProviderModel: "model-a", ProviderRequestID: "req-1", ProviderServer: "fixture",
		}},
	}
	before, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBuffer(1)
	b.Record(rec)
	after, err := json.Marshal(b.Snapshot()[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("normal record bytes changed:\nbefore=%s\nafter=%s", before, after)
	}
	if !reflect.DeepEqual(rec.ToolNames, []string{"read", "write"}) {
		t.Fatalf("normal tool names changed: %v", rec.ToolNames)
	}
}
