package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func projectionAPI(t *testing.T) (*AggAPI, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projection.db")
	d := config.Default()
	s, err := storage.Open(path, storage.Options{WriteChanCap: d.StorageWriteChanCap,
		BatchCap: d.StorageBatchCap, FlushInterval: d.StorageFlushInterval,
		QueryTimeout: time.Second, WriteTrackCap: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	return NewAggAPI(metrics.NewBuffer(64), s, time.Second), other
}

func TestProjectionPreloadAndRawModelReuse(t *testing.T) {
	api, _ := projectionAPI(t)
	now := time.Now()
	r := cacheRecord("stored", now.Add(-time.Minute))
	api.store.Record(r)
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	api.buf.Backfill([]*metrics.Record{r})
	api.store.MarkWritten([]string{r.ID})
	if err := api.PreloadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	var first *contrib
	if err := api.withProjection(t.Context(), func(p *projectionData) error {
		if !p.cursor.Ready || len(p.rows) != 1 || len(p.ids) != 1 || p.rows[0].model != "MODEL-A" || !p.hasStart || p.minStart != r.Start.UnixMilli() {
			t.Fatalf("incorrect raw preload: %+v", p)
		}
		first = &p.rows[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, rules := range [][]config.ModelRule{nil, {{Mode: config.ModelRuleLower}}} {
		api.ModelCanon = func() config.ModelCanon { return config.ModelCanon{Rules: rules} }
		e, err := api.explorer(t.Context(), "model", nil, "")
		want := "MODEL-A"
		if rules != nil {
			want = "model-a"
		}
		if err != nil || e.Total != 1 || len(e.Groups) != 1 || e.Groups[0].Name != want {
			t.Fatalf("rules must remap one raw projection: %+v, %v", e, err)
		}
		if err := api.withProjection(t.Context(), func(p *projectionData) error {
			if &p.rows[0] != first || p.rows[0].model != "MODEL-A" {
				t.Fatal("view/rule change rebuilt or mutated raw history")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectionExternalDeleteNeverResurrectsRing(t *testing.T) {
	api, other := projectionAPI(t)
	now := time.Now()
	r := cacheRecord("deleted", now.Add(-time.Hour))
	api.buf.Record(r)
	api.store.Record(r)
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := api.PreloadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec("DELETE FROM requests WHERE id = ?", r.ID); err != nil {
		t.Fatal(err)
	}
	// The durable deletion never removed the historical row from this
	// process's ring. Only genuinely unflushed rows may overlay the store.
	api.buf.Record(cacheRecord("unflushed", now.Add(-time.Minute)))
	p, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli())
	if err != nil || cacheChartRequests(p) != 1 || p.FromMs < now.Add(-2*time.Minute).UnixMilli() {
		t.Fatalf("chart resurrected deleted ring row: %+v, %v", p, err)
	}
	e, err := api.explorer(t.Context(), "provider", nil, "")
	if err != nil || e.Total != 1 {
		t.Fatalf("explorer resurrected deleted ring row: %+v, %v", e, err)
	}
}

func TestProjectionCommitAfterRingCaptureCountsExactlyOnce(t *testing.T) {
	api, _ := projectionAPI(t)
	now := time.Now()
	r := cacheRecord("racing", now.Add(-time.Minute))
	api.buf.Record(r)
	// Capture before the commit (the order enforced by streamWindow). Its
	// fresh durable projection must subsequently suppress this candidate.
	ring := api.ringSnapshot(0)
	if len(ring) != 1 {
		t.Fatalf("initial unflushed candidates = %d, want 1", len(ring))
	}
	api.store.Record(r)
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := api.withProjection(t.Context(), func(p *projectionData) error {
		count := len(p.rows)
		for _, rec := range ring {
			if _, duplicate := p.ids[rec.ID]; !duplicate {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("racing commit counted %d times", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionFailedRefreshDoesNotServeOldRows(t *testing.T) {
	api, other := projectionAPI(t)
	api.store.Record(cacheRecord("a", time.Now()))
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := api.PreloadHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	previous := api.projection.data.cursor
	if _, err := other.Exec("UPDATE requests SET ttft_ms = 'not-a-number' WHERE id = 'a'"); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := api.withProjection(t.Context(), func(*projectionData) error { called = true; return nil }); err == nil || called {
		t.Fatal("failed decode served an obsolete projection")
	}
	if api.projection.data.cursor != previous {
		t.Fatal("failed decode advanced projection cursor")
	}
	if _, err := other.Exec("UPDATE requests SET ttft_ms = 42 WHERE id = 'a'"); err != nil {
		t.Fatal(err)
	}
	if err := api.withProjection(t.Context(), func(p *projectionData) error {
		if len(p.rows) != 1 || p.rows[0].ttft != 42 {
			t.Fatalf("retry did not publish repaired snapshot: %+v", p.rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := api.PreloadHistory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preload = %v", err)
	}
}

func TestProjectionConcurrentReadersAndAppends(t *testing.T) {
	api, _ := projectionAPI(t)
	api.store.Record(cacheRecord("initial", time.Now()))
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 15 {
				err := api.withProjection(t.Context(), func(p *projectionData) error {
					if len(p.rows) != len(p.ids) || !p.cursor.Ready {
						t.Errorf("incoherent projection: rows=%d ids=%d ready=%v", len(p.rows), len(p.ids), p.cursor.Ready)
					}
					for _, c := range p.rows {
						if _, ok := p.ids[c.id]; !ok {
							t.Errorf("row %s has no dedupe entry", c.id)
						}
					}
					return nil
				})
				if err != nil {
					t.Errorf("concurrent projection: %v", err)
				}
			}
		})
	}
	for i := range 10 {
		api.store.Record(cacheRecord(fmt.Sprint(i), time.Now()))
		if err := api.store.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if err := api.withProjection(t.Context(), func(p *projectionData) error {
		if len(p.rows) != 11 {
			t.Fatalf("append catch-up retained %d rows, want 11", len(p.rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGrowRowsBoundedSpareCapacity pins the rows growth policy: small delta
// catch-ups amortize their copies (the 3*rowsGrowChunk singles below cause
// only chunk-sized growths, where doubling would have passed two whole
// blocks), batches of a chunk or more grow exactly, and the live spare
// capacity on the projection's largest retained block never exceeds one
// chunk.
func TestGrowRowsBoundedSpareCapacity(t *testing.T) {
	var rows []contrib
	for range 3 * rowsGrowChunk {
		rows = growRows(rows, []contrib{{status: 200}})
	}
	if len(rows) != 3*rowsGrowChunk || cap(rows) != 3*rowsGrowChunk {
		t.Fatalf("singles grew to len=%d cap=%d, want len=cap=%d (amortized chunk growth)",
			len(rows), cap(rows), 3*rowsGrowChunk)
	}
	rows = growRows(rows, make([]contrib, rowsGrowChunk))
	if len(rows) != 4*rowsGrowChunk || cap(rows) != 4*rowsGrowChunk {
		t.Fatalf("chunk batch grew to len=%d cap=%d, want exact %d",
			len(rows), cap(rows), 4*rowsGrowChunk)
	}
	rows = growRows(rows, []contrib{{status: 201}})
	if cap(rows) != 5*rowsGrowChunk {
		t.Fatalf("rounded growth cap=%d, want one chunk of headroom (%d)", cap(rows), 5*rowsGrowChunk)
	}
	spare := cap(rows) - len(rows)
	rows = growRows(rows, []contrib{{status: 202}})
	if cap(rows) != 5*rowsGrowChunk || cap(rows)-len(rows) != spare-1 {
		t.Fatal("an in-capacity batch reallocated instead of reusing headroom")
	}
	if spare > rowsGrowChunk {
		t.Fatalf("live spare capacity = %d rows, want <= one chunk (%d)", spare, rowsGrowChunk)
	}
}

// TestProjectionVisitSinceChunks pins the chunk-max window skip: blocks whose
// newest row is older than the window are never visited, while a late
// completion that lands in an old block is still visited because it raises that
// block's maximum.
func TestProjectionVisitSinceChunks(t *testing.T) {
	var p projectionData
	for i := range 3 * rowsGrowChunk {
		start := int64(100000 - i)
		p.rows = append(p.rows, contrib{start: start})
		p.noteChunkStart(uint32(i+1), start)
	}
	// Late completion appended into a new block with a much newer start: the
	// block's maximum must include it even though every earlier block is old.
	lateStart := int64(999999)
	p.rows = append(p.rows, contrib{start: lateStart})
	p.noteChunkStart(uint32(len(p.rows)), lateStart)

	visited := 0
	if err := p.visitSince(95000, func(c *contrib) error {
		if c.start < 95000 {
			t.Fatalf("visited out-of-window start %d", c.start)
		}
		visited++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := range p.rows {
		if p.rows[i].start >= 95000 {
			want++
		}
	}
	if visited != want {
		t.Fatalf("visited %d rows, want %d", visited, want)
	}

	// Unbounded (since <= 0) still visits every row.
	visited = 0
	if err := p.visitSince(0, func(*contrib) error { visited++; return nil }); err != nil {
		t.Fatal(err)
	}
	if visited != len(p.rows) {
		t.Fatalf("unbounded visited %d rows, want %d", visited, len(p.rows))
	}

	// A window ahead of every row visits nothing.
	visited = 0
	if err := p.visitSince(lateStart+1, func(*contrib) error { visited++; return nil }); err != nil {
		t.Fatal(err)
	}
	if visited != 0 {
		t.Fatalf("empty window visited %d rows", visited)
	}
}
