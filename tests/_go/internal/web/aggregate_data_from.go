package web

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// dataFromOf reads data_from_ms from one decoded payload.
func dataFromOf(t *testing.T, p map[string]any, surface string) int64 {
	t.Helper()
	v, ok := p["data_from_ms"]
	if !ok {
		t.Fatalf("%s: data_from_ms missing", surface)
	}
	return int64(v.(float64))
}

// TestDataFromMsPayloads pins the stored-history extent on both payloads: the
// earliest stored request start, one projection-owned value on the bootstrap
// and chart surfaces alike, unscaled by any chart scope filter.
func TestDataFromMsPayloads(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	oldest := mkRec("extent-oldest", now.Add(-3*time.Hour), 200, "", nil, 10, 100, 1, 1, 0, 0)
	oldest.Client = "old-client"
	newest := mkRec("extent-newest", now.Add(-time.Minute), 200, "", nil, 10, 100, 1, 1, 0, 0)
	newest.Client = "new-client"
	s.Record(oldest)
	s.Record(newest)
	waitTotals(t, s, 2)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	want := oldest.Start.UnixMilli()

	bootstrap := get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := dataFromOf(t, bootstrap, "bootstrap"); got != want {
		t.Fatalf("bootstrap data_from_ms = %d, want the stored minimum %d", got, want)
	}

	// The chart value stays unscoped: a filter matching only the newest row
	// must not move the stored-history extent.
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60&f=client:new-client")
	if got := dataFromOf(t, p, "scoped chart"); got != want {
		t.Fatalf("scoped chart data_from_ms = %d, want the unscoped minimum %d", got, want)
	}

	// A range request publishes the same extent.
	p = get(t, http.HandlerFunc(api.HandleAggChart), fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d",
		now.Add(-2*time.Hour).UnixMilli(), now.Add(-30*time.Minute).UnixMilli()))
	if got := dataFromOf(t, p, "range chart"); got != want {
		t.Fatalf("range chart data_from_ms = %d, want %d", got, want)
	}
}

// TestDataFromMsEmptyStoreAndRingOnly pins the zero reading: an empty durable
// store reports 0 on both surfaces, and without a store the field stays 0 even
// while the ring holds records - it reports stored history, not the live tail.
func TestDataFromMsEmptyStoreAndRingOnly(t *testing.T) {
	s := testStore(t)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	if got := dataFromOf(t, get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap"), "empty bootstrap"); got != 0 {
		t.Fatalf("empty-store bootstrap data_from_ms = %d, want 0", got)
	}
	if got := dataFromOf(t, get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=15"), "empty chart"); got != 0 {
		t.Fatalf("empty-store chart data_from_ms = %d, want 0", got)
	}

	buf := metrics.NewBuffer(4)
	buf.Record(mkRec("ring", time.Now().Add(-time.Hour), 200, "", nil, 10, 100, 1, 1, 0, 0))
	ring := NewAggAPI(buf, nil, time.Second)
	if got := dataFromOf(t, get(t, http.HandlerFunc(ring.HandleBootstrap), "/metrics/bootstrap"), "ring bootstrap"); got != 0 {
		t.Fatalf("ring-only bootstrap data_from_ms = %d, want 0", got)
	}
	if got := dataFromOf(t, get(t, http.HandlerFunc(ring.HandleAggChart), "/metrics/agg/chart?window=all"), "ring chart"); got != 0 {
		t.Fatalf("ring-only chart data_from_ms = %d, want 0", got)
	}
}

// TestDataFromMsFollowsClearFence pins the destructive-clear behaviour: a
// filtered clear leaves the surviving minimum, a full clear returns to 0, and
// the bootstrap surface reflects it on its NEXT response without an
// intervening chart or explorer read.
func TestDataFromMsFollowsClearFence(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	keep := mkRec("extent-keep", now.Add(-2*time.Hour), 200, "", nil, 10, 100, 1, 1, 0, 0)
	keep.Provider = "keep.example"
	drop := mkRec("extent-drop", now.Add(-3*time.Hour), 200, "", nil, 10, 100, 1, 1, 0, 0)
	drop.Provider = "drop.example"
	s.Record(keep)
	s.Record(drop)
	waitTotals(t, s, 2)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)

	bootstrap := get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := dataFromOf(t, bootstrap, "baseline"); got != drop.Start.UnixMilli() {
		t.Fatalf("baseline data_from_ms = %d, want %d", got, drop.Start.UnixMilli())
	}

	// Filtered clear: the oldest row is removed, the surviving minimum stays.
	if _, err := s.Clear(t.Context(), &storage.PurgeFilter{Provider: "drop.example"}, nil); err != nil {
		t.Fatal(err)
	}
	bootstrap = get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := dataFromOf(t, bootstrap, "post-filtered-clear"); got != keep.Start.UnixMilli() {
		t.Fatalf("post-filtered-clear data_from_ms = %d, want %d", got, keep.Start.UnixMilli())
	}
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=1440")
	if got := dataFromOf(t, p, "post-filtered-clear chart"); got != keep.Start.UnixMilli() {
		t.Fatalf("post-filtered-clear chart data_from_ms = %d, want %d", got, keep.Start.UnixMilli())
	}

	// Full clear: no stored history remains.
	if _, err := s.Clear(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	bootstrap = get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := dataFromOf(t, bootstrap, "post-clear"); got != 0 {
		t.Fatalf("post-clear data_from_ms = %d, want 0", got)
	}
	p = get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=1440")
	if got := dataFromOf(t, p, "post-clear chart"); got != 0 {
		t.Fatalf("post-clear chart data_from_ms = %d, want 0", got)
	}
}
