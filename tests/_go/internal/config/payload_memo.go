package config

import (
	"reflect"
	"sync"
	"testing"
)

// mapIdentity returns the shared runtime identity of a map value. PayloadMemo
// reuses one map between polls, so tests compare identity (not just content)
// with this helper.
func mapIdentity(m map[string]any) uintptr {
	return reflect.ValueOf(m).Pointer()
}

// TestPayloadMemoReusesOnePayloadPerConfig pins the bootstrap payload contract:
// one exported map per live *Config, reused by every poll until the live
// pointer changes. That is the whole point of the memo - Map() walks the schema
// through reflection on every call, and the dashboard polls bootstrap per open
// tab every few seconds.
func TestPayloadMemoReusesOnePayloadPerConfig(t *testing.T) {
	var memo PayloadMemo
	cfg := Default()

	first := memo.Map(cfg)
	if want := cfg.Map(); !reflect.DeepEqual(first, want) {
		t.Fatalf("PayloadMemo.Map = %d keys, want the full config.Map payload (%d keys)", len(first), len(want))
	}
	second := memo.Map(cfg)
	if mapIdentity(first) != mapIdentity(second) {
		t.Fatal("two polls of one config returned different payload maps; the memo did not reuse its entry")
	}
}

// TestPayloadMemoInvalidatesOnPointerNotContent: invalidation is pointer
// identity, never content equality. A reload publishes a fresh *Config even
// when every value is identical, and that reload must still re-export (a
// content comparison would cost a full reflect.DeepEqual per poll and could
// pin a stale payload if a future config field were excluded from the map).
func TestPayloadMemoInvalidatesOnPointerNotContent(t *testing.T) {
	var memo PayloadMemo
	cfg := Default()
	first := memo.Map(cfg)

	// A distinct pointer with byte-identical content is a distinct identity.
	same := cfg.Clone()
	second := memo.Map(same)
	if mapIdentity(first) == mapIdentity(second) {
		t.Fatal("a new *Config with identical content reused the previous payload; invalidation must be by pointer")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical configs exported different payloads")
	}
}

// TestPayloadMemoServesReloadedValues: the next poll after a reload serves the
// new values, and the previously returned payload is left untouched for any
// encoder still holding it. The old map is the previous config's export, not a
// view the memo rewrites in place.
func TestPayloadMemoServesReloadedValues(t *testing.T) {
	var memo PayloadMemo
	before := Default()
	beforePayload := memo.Map(before)
	beforeRows := beforePayload["dash_log_rows"]

	after := before.Clone()
	after.DashLogRows++ // any schema field; Validate is not the memo's concern
	afterPayload := memo.Map(after)

	if afterPayload["dash_log_rows"] != beforeRows.(int)+1 {
		t.Fatalf("payload after reload = %v, want %v", afterPayload["dash_log_rows"], beforeRows.(int)+1)
	}
	if beforePayload["dash_log_rows"] != beforeRows {
		t.Fatalf("the pre-reload payload was mutated in place: %v, want %v", beforePayload["dash_log_rows"], beforeRows)
	}
	if mapIdentity(beforePayload) == mapIdentity(afterPayload) {
		t.Fatal("reload reused the previous payload map")
	}
}

// TestPayloadMemoConcurrentPollsShareOnePayload: a fresh tab burst serializes
// on the one computation and every poll observes the same immutable map.
func TestPayloadMemoConcurrentPollsShareOnePayload(t *testing.T) {
	var memo PayloadMemo
	cfg := Default()
	const polls = 8
	ids := make([]uintptr, polls)
	var wg sync.WaitGroup
	for i := range polls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = mapIdentity(memo.Map(cfg))
		}()
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("poll %d saw a different payload map than poll 0; concurrent first polls must share one export", i)
		}
	}
}

// BenchmarkPayloadMemo measures the bootstrap config payload with and without
// the memo. "map" is the per-poll cost the bootstrap used to pay through
// liveCfg.Clone().Map(); "memo_hit" is what a poll pays until the next reload.
func BenchmarkPayloadMemo(b *testing.B) {
	cfg := Default()
	b.Run("map", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = cfg.Clone().Map()
		}
	})
	var memo PayloadMemo
	_ = memo.Map(cfg) // warm the one entry
	b.Run("memo_hit", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = memo.Map(cfg)
		}
	})
}
