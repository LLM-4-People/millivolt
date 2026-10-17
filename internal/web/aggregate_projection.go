package web

// The durable analytical projection is the one decoded source shared by all
// chart/explorer scopes. SQLite remains authoritative. It stores only metric
// contributions and dimension dictionaries, never request/response bodies or
// per-view copies; memory grows with durable analytical history, not visits.

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"runtime/debug"
	"sync"

	"github.com/LLM-4-People/millivolt/internal/storage"
)

type projectionData struct {
	rows       []contrib
	ids        map[string]struct{}
	dimensions contribDimensions
	metrics    projectionMetrics
	minStart   int64
	hasStart   bool
	cursor     storage.ProjectionCursor
	version    int64
	// chunkMaxStart holds the maximum contribution start per rowsGrowChunk
	// block, so a bounded window can skip whole blocks whose newest row is
	// older than the window. Late completions only raise their own block's
	// maximum, which never makes the skip unsound.
	chunkMaxStart []int64
}

type historyProjection struct {
	mu   sync.RWMutex
	data projectionData
}

// rowsGrowChunk bounds the rows slice's spare capacity. Growth rounds the new
// length up to a multiple of this chunk: small delta catch-ups amortize their
// copies (one copy per ~chunk/batch refreshes) and retained spare capacity
// stays below one chunk (~1.4 MB at the current row width) no matter how long
// the history grows, instead of append doubling's up-to-2x live slack.
// Internal implementation constant, not a tunable.
const rowsGrowChunk = 4096

// growRows appends with chunk-bounded spare capacity. Growth copies the whole
// retained rows block, so it must stay amortized for the small per-refresh
// deltas of the query path while never leaving large live slack: batches
// larger than one chunk grow exactly, batches that fit share one rounded
// chunk of headroom.
func growRows(dst, add []contrib) []contrib {
	if cap(dst)-len(dst) < len(add) {
		size := len(dst) + len(add)
		if r := size % rowsGrowChunk; r != 0 && len(add) < rowsGrowChunk {
			size += rowsGrowChunk - r
		}
		grown := make([]contrib, size)
		dst = grown[:copy(grown, dst)]
	}
	return append(dst, add...)
}

// PreloadHistory moves the one initial durable decode ahead of HTTP readiness.
// It preloads the shared data source, not hardcoded default dashboard views.
// A failed preload publishes nothing; later reads can retry the same builder.
// The startup decode allocates several times the retained projection (per-row
// SQL strings, attempts and tool-name JSON become garbage after interning);
// one forced collection and scavenge returns those burst pages to the OS
// immediately instead of leaving them resident until the background scavenger
// gets there. Later full rebuilds (destructive changes) must NOT do this:
// they run on the query path, where a forced GC would add a stop-the-world
// pause to a live request.
func (a *AggAPI) PreloadHistory(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if err := a.withProjection(ctx, func(*projectionData) error { return nil }); err != nil {
		return err
	}
	debug.FreeOSMemory()
	return nil
}

// withProjection holds one read lease for metadata preparation AND iteration.
// Borrowed maps/slices must not escape this callback. Catch-up never takes the
// ring/writer hot-path locks; concurrent views share one materialization.
func (a *AggAPI) withProjection(ctx context.Context, fn func(*projectionData) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.store == nil {
		return fn(nil)
	}
	if err := a.projection.refresh(ctx, a.store); err != nil {
		return err
	}
	a.projection.mu.RLock()
	defer a.projection.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(&a.projection.data)
}

// noteChunkStart records a contribution's start in its block's maximum.
// rowIndex is 1-based, matching contrib.rowIndex.
func (d *projectionData) noteChunkStart(rowIndex uint32, start int64) {
	chunk := int(rowIndex-1) / rowsGrowChunk
	for len(d.chunkMaxStart) <= chunk {
		d.chunkMaxStart = append(d.chunkMaxStart, start)
	}
	if start > d.chunkMaxStart[chunk] {
		d.chunkMaxStart[chunk] = start
	}
}

// visitSince calls fn for every contribution whose start is at or after since
// (since <= 0 means all). Whole blocks older than the window are skipped, so a
// bounded chart window never touches stale history row by row.
func (d *projectionData) visitSince(since int64, fn func(*contrib) error) error {
	if since <= 0 || len(d.chunkMaxStart) == 0 {
		for i := range d.rows {
			if err := fn(&d.rows[i]); err != nil {
				return err
			}
		}
		return nil
	}
	for chunk, maxStart := range d.chunkMaxStart {
		if maxStart < since {
			continue
		}
		start := chunk * rowsGrowChunk
		end := min(start+rowsGrowChunk, len(d.rows))
		for i := start; i < end; i++ {
			if d.rows[i].start < since {
				continue
			}
			if err := fn(&d.rows[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *historyProjection) refresh(ctx context.Context, store *storage.Store) error {
	version, err := store.DataVersion(ctx)
	if err != nil {
		return err // never serve an old projection when freshness is unknowable
	}
	p.mu.RLock()
	current := p.data.cursor.Ready && p.data.version == version
	p.mu.RUnlock()
	if current {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Another reader may have caught up while this caller waited. Re-read the
	// version after acquiring the build lock; an older caller cannot roll the
	// projection's version back after a newer caller already published it.
	version, err = store.DataVersion(ctx)
	if err != nil {
		return err
	}
	if p.data.cursor.Ready && p.data.version == version {
		return nil
	}
	var incoming []contrib
	next, full, err := store.StreamProjection(ctx, rowColumns, p.data.cursor, func(rows *sql.Rows) error {
		c, err := scanContrib(rows, nil) // raw spelling; query rules apply later
		if err == nil {
			incoming = append(incoming, c)
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data := p.data
	if full {
		data = projectionData{ids: make(map[string]struct{}, len(incoming))}
	} else {
		for _, c := range incoming {
			if c.id != "" {
				if _, exists := data.ids[c.id]; exists {
					return errors.New("analytical append unexpectedly replaced an existing request")
				}
			}
		}
	}
	// Compact metric-order indices are uint32; reject before narrowing or
	// mutating a published dictionary (an arithmetic guard, not a history cap).
	if uint64(len(data.rows))+uint64(len(incoming)) > math.MaxUint32 {
		return errors.New("analytical row index capacity exceeded")
	}
	previousRows := len(data.rows)
	for i := range incoming {
		c := &incoming[i]
		c.rowIndex = uint32(previousRows + i + 1)
		data.dimensions.intern(c)
		if !data.hasStart || c.start < data.minStart {
			data.minStart, data.hasStart = c.start, true
		}
		data.noteChunkStart(c.rowIndex, c.start)
		if c.id != "" {
			data.ids[c.id] = struct{}{}
		}
	}
	// growRows covers the full rebuild (dst empty) and the delta append with
	// one owner; spare capacity never exceeds the incoming batch.
	data.rows = growRows(data.rows, incoming)
	data.metrics.append(data.rows, previousRows)
	data.cursor, data.version = next, version
	// Keep the PRE-scan version. If a commit landed during materialization,
	// the next read catches it up instead of falsely stamping old rows fresh.
	p.data = data
	return nil
}
