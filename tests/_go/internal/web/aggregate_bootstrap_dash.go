package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// payloadMapIdentity returns the runtime identity of the memoized payload map;
// reuse is asserted by identity, not by content equality.
func payloadMapIdentity(m map[string]any) uintptr { return reflect.ValueOf(m).Pointer() }

// The bootstrap config payload is exported once per live *config.Config and
// reused by every 5 s poll of every open tab; main wires config.PayloadMemo
// behind AggAPI.Dash. These tests drive the real handler with the memo as the
// provider and prove both halves of the contract: consecutive polls reuse one
// payload, and a reload (a NEW *config.Config pointer, exactly what
// reloadConfigLocked publishes) is served on the very next poll.
func TestBootstrapConfigPayloadMemo(t *testing.T) {
	// live is main's liveCfg stand-in; mu is liveMu.
	var mu sync.Mutex
	live := config.Default()
	var memo config.PayloadMemo

	// served records the payload the provider handed the handler, so identity
	// can be asserted across polls (the HTTP body is re-decoded per response).
	var served []map[string]any
	agg := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	agg.Dash = func() any {
		mu.Lock()
		cfg := live
		mu.Unlock()
		payload := memo.Map(cfg) // exactly main's provider
		served = append(served, payload)
		return payload
	}

	poll := func() map[string]any {
		t.Helper()
		doc := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
		dash, ok := doc["dash"].(map[string]any)
		if !ok {
			t.Fatalf("bootstrap dash = %v, want the exported config payload", doc["dash"])
		}
		return dash
	}

	first := poll()
	mu.Lock()
	wantFirst := live.Map()
	mu.Unlock()
	wantJSON, err := json.Marshal(wantFirst)
	if err != nil {
		t.Fatal(err)
	}
	var wantDecoded map[string]any
	if err := json.Unmarshal(wantJSON, &wantDecoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, wantDecoded) {
		t.Fatalf("bootstrap dash differs from config.Map\n got: %v\nwant: %v", first, wantDecoded)
	}

	// Consecutive polls without a reload: one export, reused. Pointer identity
	// of the provider's map is the proof; a content comparison could pass even
	// when Map() ran again.
	second := poll()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("consecutive polls served different dash payloads without a reload")
	}
	if len(served) != 2 {
		t.Fatalf("dash provider ran %d times, want 2", len(served))
	}
	if payloadMapIdentity(served[0]) != payloadMapIdentity(served[1]) {
		t.Fatal("consecutive polls without a reload did not reuse the cached payload")
	}

	// A reload publishes a fresh *config.Config (main.go liveCfg = fresh.Clone()).
	// The next poll must serve its values, not the cached ones.
	const reloadedRows = 123
	mu.Lock()
	live = live.Clone()
	live.DashLogRows = reloadedRows
	mu.Unlock()

	third := poll()
	if got := third["dash_log_rows"]; got != float64(reloadedRows) {
		t.Fatalf("dash_log_rows after reload = %v, want %d", got, reloadedRows)
	}
	if payloadMapIdentity(served[1]) == payloadMapIdentity(served[2]) {
		t.Fatal("a reloaded config pointer reused the previous payload")
	}

	// Encoding must not mutate the shared payload: the pre-reload map still
	// equals a fresh export of the same (unchanged) config values.
	if !reflect.DeepEqual(served[0], wantFirst) {
		t.Fatal("the cached payload was mutated after being served")
	}
	// The second and third payloads are independent maps, so a reload cannot
	// rewrite history for an in-flight encoder.
	if payloadMapIdentity(served[1]) == payloadMapIdentity(served[2]) {
		t.Fatal("reload aliased the previous payload map")
	}
}

// BenchmarkBootstrapConfigPayload measures the real bootstrap handler with the
// pre-memo provider (Clone+Map on every poll) against the memoized provider.
// The delta is one schema walk per poll; the shared remainder is the ring
// snapshot, KPI and the 217 KB payload encode, which this change does not own.
func BenchmarkBootstrapConfigPayload(b *testing.B) {
	live := config.Default()
	var memo config.PayloadMemo
	_ = memo.Map(live) // warm the one entry
	for _, tc := range []struct {
		name string
		dash func() any
	}{
		{name: "direct", dash: func() any { return live.Clone().Map() }},
		{name: "memo", dash: func() any { return memo.Map(live) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			agg := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
			agg.Dash = tc.dash
			h := http.HandlerFunc(agg.HandleBootstrap)
			for b.Loop() {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil))
				if rec.Code != http.StatusOK {
					b.Fatalf("bootstrap status %d", rec.Code)
				}
			}
		})
	}
}
