package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func cacheChartRequests(p chartPayload) int64 {
	var n int64
	for _, b := range p.Buckets {
		n += b.Req
	}
	return n
}

func cacheRecord(id string, at time.Time) *metrics.Record {
	r := mkRec(id, at, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
	r.Provider, r.Model = "old.example", "MODEL-A"
	return r
}

func TestAggregateMemoReusesIdenticalResults(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	fs := []scopeFilter{{dim: "provider", id: "old.example"}, {dim: "status", id: "2xx"}}
	reversed := []scopeFilter{fs[1], fs[0]}
	p, err := api.chart(t.Context(), 0, fs, "", now.UnixMilli())
	if err != nil || cacheChartRequests(p) != 1 {
		t.Fatalf("chart = %+v, %v", p, err)
	}
	chartEntry := api.chartMemo.entry
	p2, err := api.chart(t.Context(), 0, reversed, "", now.Add(time.Second).UnixMilli())
	if err != nil || api.chartMemo.entry != chartEntry {
		t.Fatalf("identical chart refolded: %v", err)
	}
	if p2.NowMs == p.NowMs || chartEntry.value.payload.NowMs != p.NowMs || !reflect.DeepEqual(p.Buckets, p2.Buckets) {
		t.Fatal("chart hit must update only its copied clock metadata")
	}
	e, err := api.explorer(t.Context(), "provider", fs, "")
	if err != nil || e.Total != 1 {
		t.Fatalf("explorer = %+v, %v", e, err)
	}
	explorerEntry := api.explorerMemo.entry
	e2, err := api.explorer(t.Context(), "provider", reversed, "")
	if err != nil || api.explorerMemo.entry != explorerEntry || !reflect.DeepEqual(e, e2) {
		t.Fatalf("identical explorer refolded: %v", err)
	}
	if _, err := api.chart(t.Context(), 60, fs, "", now.UnixMilli()); err != nil || api.chartMemo.entry == chartEntry {
		t.Fatalf("different window reused old entry: %v", err)
	}
	if _, err := api.explorer(t.Context(), "model", fs, ""); err != nil || api.explorerMemo.entry == explorerEntry {
		t.Fatalf("different dimension reused old entry: %v", err)
	}
}

func TestAggregateMemoInvalidatesBufferAndPending(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	buf := metrics.NewBuffer(1)
	buf.Record(cacheRecord("a", now))
	api := NewAggAPI(buf, nil, time.Second)
	refresh := func(wantFinal, wantAll int64) {
		t.Helper()
		p, err := api.chart(t.Context(), 60, nil, "", now.Add(time.Minute).UnixMilli())
		if err != nil || cacheChartRequests(p) != wantFinal {
			t.Fatalf("chart requests = %d, %v; want %d", cacheChartRequests(p), err, wantFinal)
		}
		e, err := api.explorer(t.Context(), "status", nil, "")
		if err != nil || e.Total != wantAll {
			t.Fatalf("explorer requests = %d, %v; want %d", e.Total, err, wantAll)
		}
	}
	refresh(1, 1)
	chartEntry, explorerEntry := api.chartMemo.entry, api.explorerMemo.entry
	for _, phase := range []string{"begin", "update"} {
		r := cacheRecord("pending", now)
		r.Stream, r.Paused = true, phase == "update"
		buf.PublishLive(phase, r)
		refresh(1, 2)
		if api.chartMemo.entry != chartEntry || api.explorerMemo.entry == explorerEntry {
			t.Fatalf("%s must invalidate explorer but not finalized chart", phase)
		}
		explorerEntry = api.explorerMemo.entry
	}
	buf.Record(cacheRecord("pending", now)) // completion retires pending atomically
	refresh(1, 1)
	if api.chartMemo.entry == chartEntry || api.explorerMemo.entry == explorerEntry {
		t.Fatal("completion must invalidate both finalized and pending views")
	}
	chartEntry = api.chartMemo.entry
	buf.Record(cacheRecord("b", now)) // replacement keeps count, changes revision
	refresh(1, 1)
	if api.chartMemo.entry == chartEntry {
		t.Fatal("ring eviction reused old chart")
	}
	buf.RemoveWhere(func(*metrics.Record) bool { return true })
	refresh(0, 0)
	buf.Backfill([]*metrics.Record{cacheRecord("c", now)})
	refresh(1, 1)
	buf.Reset()
	refresh(0, 0)
}

func TestAggregateMemoInvalidatesDatabaseMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	d := config.Default()
	s, err := storage.Open(path, storage.Options{WriteChanCap: d.StorageWriteChanCap,
		BatchCap: d.StorageBatchCap, FlushInterval: d.StorageFlushInterval, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.Record(cacheRecord("stored", now.Add(-time.Minute)))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(8), s, time.Second)
	refresh := func(n int64, provider string) {
		t.Helper()
		p, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli())
		if err != nil || cacheChartRequests(p) != n {
			t.Fatalf("chart = %d, %v; want %d", cacheChartRequests(p), err, n)
		}
		e, err := api.explorer(t.Context(), "provider", nil, "")
		if err != nil || e.Total != n || (n > 0 && e.Groups[0].Name != provider) {
			t.Fatalf("explorer = %+v, %v", e, err)
		}
	}
	refresh(1, "old.example")
	first := api.chartMemo.entry
	if _, err := s.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"}); err != nil {
		t.Fatal(err)
	}
	refresh(1, "new.example")
	if api.chartMemo.entry == first {
		t.Fatal("provider rename retained chart cache")
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec("UPDATE requests SET cost = ? WHERE id = ?", 0.75, "stored"); err != nil {
		t.Fatal(err)
	}
	refresh(1, "new.example")
	if cost := api.explorerMemo.entry.value.Groups[0].Cost; cost != 0.75 {
		t.Fatalf("external writer invisible to cache: cost %v, want 0.75", cost)
	}
	if _, err := s.PurgeWhere(t.Context(), storage.PurgeFilter{Provider: "new.example"}); err != nil {
		t.Fatal(err)
	}
	refresh(0, "")
	s.Record(cacheRecord("new", now.Add(-time.Minute)))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	refresh(1, "old.example")
	if err := s.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	refresh(0, "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err == nil {
		t.Fatal("closed database reused cached chart")
	}
	if _, err := api.explorer(t.Context(), "provider", nil, ""); err == nil {
		t.Fatal("closed database reused cached explorer")
	}
}

func TestAggregateMemoClockGeometry(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	for _, win := range []int{0, 60} {
		p, err := api.chart(t.Context(), win, nil, "", now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		entry := api.chartMemo.entry
		after := now.Add(time.Duration(p.BucketMs) * time.Millisecond)
		p2, err := api.chart(t.Context(), win, nil, "", after.UnixMilli())
		if err != nil || api.chartMemo.entry == entry || cacheChartRequests(p2) != 1 {
			t.Fatalf("window %d missed clock boundary invalidation: %v", win, err)
		}
	}
	buf.Reset()
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p, err := api.chart(t.Context(), 0, nil, "", now.Add(3*time.Hour).UnixMilli())
	if err != nil || len(p.Buckets) != 1 || p.FromMs < now.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("empty All range kept an obsolete minimum: %+v, %v", p, err)
	}
	buf.Record(cacheRecord("future", now.Add(time.Hour)))
	entry := api.chartMemo.entry
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err != nil || api.chartMemo.entry != entry {
		t.Fatalf("future-clamped All window must not become a cache entry: %v", err)
	}
}

func TestAggregateMemoSnapshotRulesAndMidScanMutation(t *testing.T) {
	for _, surface := range []string{"chart", "explorer"} {
		t.Run(surface, func(t *testing.T) {
			now := time.Now()
			buf := metrics.NewBuffer(8)
			buf.Record(cacheRecord("first", now.Add(-time.Minute)))
			api := NewAggAPI(buf, nil, time.Second)
			calls := 0
			api.ModelCanon = func() config.ModelCanon {
				calls++
				if calls == 2 {
					buf.Record(cacheRecord("racing", now.Add(-time.Minute)))
				}
				return config.ModelCanon{}
			}
			if surface == "chart" {
				p, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli())
				if err != nil || cacheChartRequests(p) != 1 || api.chartMemo.entry != nil {
					t.Fatalf("racing chart was memoized: %+v, %v", p, err)
				}
			} else {
				p, err := api.explorer(t.Context(), "model", nil, "")
				if err != nil || p.Total != 1 || api.explorerMemo.entry != nil {
					t.Fatalf("racing explorer was memoized: %+v, %v", p, err)
				}
			}
		})
	}
	now := time.Now()
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now))
	buf.PublishLive("begin", cacheRecord("pending", now))
	api := NewAggAPI(buf, nil, time.Second)
	calls := 0
	api.ModelCanon = func() config.ModelCanon {
		calls++
		if calls == 1 {
			return config.ModelCanon{}
		}
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	e, err := api.explorer(t.Context(), "model", nil, "")
	if err != nil || len(e.Groups) != 1 || e.Groups[0].Name != "MODEL-A" || e.Total != 2 || api.explorerMemo.entry != nil {
		t.Fatalf("one fold mixed canonicalization snapshots: %+v, %v", e, err)
	}
	e, err = api.explorer(t.Context(), "model", nil, "")
	if err != nil || len(e.Groups) != 1 || e.Groups[0].Name != "model-a" || api.explorerMemo.entry == nil {
		t.Fatalf("new canonicalization was not applied: %+v, %v", e, err)
	}
}

func TestAggregateMemoCancellationBoundAndConcurrentReaders(t *testing.T) {
	now := time.Now()
	buf := metrics.NewBuffer(64)
	for i := range 48 {
		r := cacheRecord(fmt.Sprint(i), now.Add(-time.Minute))
		r.Provider = fmt.Sprintf("group-%d.example", i)
		buf.Record(r)
	}
	api := NewAggAPI(buf, nil, time.Second)
	e, err := api.explorer(t.Context(), "provider", nil, "")
	if err != nil || len(e.Groups) != xpNodeCap || cap(e.Groups) != xpNodeCap {
		t.Fatalf("cached groups retain discarded history: len/cap=%d/%d, %v", len(e.Groups), cap(e.Groups), err)
	}
	if _, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := api.explorer(ctx, "provider", nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled explorer request = %v", err)
	}
	if _, err := api.chart(ctx, 60, nil, "", now.UnixMilli()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled chart request = %v", err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 8 {
				p, err := api.chart(t.Context(), 60, nil, "", now.Add(time.Duration(i)*time.Millisecond).UnixMilli())
				if err != nil || cacheChartRequests(p) != 48 {
					t.Errorf("concurrent chart = %d, %v", cacheChartRequests(p), err)
				}
				e, err := api.explorer(t.Context(), "provider", nil, "")
				if err != nil || e.Total != 48 {
					t.Errorf("concurrent explorer = %d, %v", e.Total, err)
				}
			}
		})
	}
	wg.Wait()
}
