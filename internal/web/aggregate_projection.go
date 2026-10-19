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
	// lineage is the resolved conversation topology for these rows. It is
	// rebuilt on a destructive replacement and updated incrementally on
	// append; requests only mark their own flags on it.
	lineage conversationTopology
	// modelCache retains canonical model IDs for the current build. Appends
	// extend its prefix; a replacement publishes a fresh cache.
	modelCache *modelProjectionCache
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
	// building is non-nil while exactly one builder materializes rows off the
	// lock. Concurrent readers wait on it instead of repeating the same scan;
	// readers holding the previous published snapshot keep serving.
	building chan struct{}
}

// rowsGrowChunk bounds the rows slice's spare capacity. Growth rounds the new
// length up to a multiple of this chunk: small delta catch-ups amortize their
// copies (one copy per ~chunk/batch refreshes) and retained spare capacity
// stays below one chunk (~1.4 MB at the current row width) no matter how long
// the history grows, instead of append doubling's up-to-2x live slack.
// Internal implementation constant, not a tunable.
const rowsGrowChunk = 4096

// ctxCheckRows is the cancellation cadence for projection folds and ring
// visitors. One check per block keeps the hot path allocation-free while
// bounding abandoned-request work to a few thousand rows.
const ctxCheckRows = 4096

// checkFoldContext reports the caller's cancellation error at each
// ctxCheckRows-row boundary. Fold and ring loops share this cadence owner.
func checkFoldContext(ctx context.Context, row int) error {
	if row%ctxCheckRows != 0 {
		return nil
	}
	return ctx.Err()
}

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
	if err := a.withProjectionBudget(ctx, projectionMaintenance, func(*projectionData) error { return nil }); err != nil {
		return err
	}
	debug.FreeOSMemory()
	return nil
}

// projectionBudget selects which storage budget bounds one materialization.
// The startup preload is maintenance work; every dashboard read stays on the
// interactive query timeout.
type projectionBudget int

const (
	projectionInteractive projectionBudget = iota
	projectionMaintenance
)

// withProjection holds one read lease for metadata preparation AND iteration.
// Borrowed maps/slices must not escape this callback. Catch-up never takes the
// ring/writer hot-path locks; concurrent views share one materialization.
func (a *AggAPI) withProjection(ctx context.Context, fn func(*projectionData) error) error {
	return a.withProjectionBudget(ctx, projectionInteractive, fn)
}

func (a *AggAPI) withProjectionBudget(ctx context.Context, budget projectionBudget, fn func(*projectionData) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.store == nil {
		return fn(nil)
	}
	if err := a.projection.refresh(ctx, a.store, budget); err != nil {
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
// bounded chart window never touches stale history row by row. Cancellation is
// observed once per block, so an abandoned fold stops within ctxCheckRows rows.
func (d *projectionData) visitSince(ctx context.Context, since int64, fn func(*contrib) error) error {
	if since <= 0 || len(d.chunkMaxStart) == 0 {
		return d.visitRange(ctx, 0, len(d.rows), 0, fn)
	}
	for chunk, maxStart := range d.chunkMaxStart {
		if maxStart < since {
			continue
		}
		start := chunk * rowsGrowChunk
		end := min(start+rowsGrowChunk, len(d.rows))
		if err := d.visitRange(ctx, start, end, since, fn); err != nil {
			return err
		}
	}
	return nil
}

// visitRange walks rows[start:end], skipping starts below since when since > 0.
// It re-checks the caller's context every ctxCheckRows rows.
func (d *projectionData) visitRange(ctx context.Context, start, end int, since int64, fn func(*contrib) error) error {
	for start < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		stop := min(start+ctxCheckRows, end)
		for i := start; i < stop; i++ {
			if since > 0 && d.rows[i].start < since {
				continue
			}
			if err := fn(&d.rows[i]); err != nil {
				return err
			}
		}
		start = stop
	}
	return nil
}

// projectionDataVersion reads the freshness witness under the selected budget.
// An interactive read must keep the storage query timeout; only the startup
// preload selects maintenance.
func projectionDataVersion(ctx context.Context, store *storage.Store, budget projectionBudget) (int64, error) {
	if budget == projectionMaintenance {
		return store.DataVersionMaintenance(ctx)
	}
	return store.DataVersion(ctx)
}

// projectionStream streams the projection under the selected budget, keeping
// the interactive timeout on every dashboard read path.
func projectionStream(ctx context.Context, store *storage.Store, budget projectionBudget, cursor storage.ProjectionCursor, fn func(*sql.Rows) error) (storage.ProjectionCursor, bool, error) {
	if budget == projectionMaintenance {
		return store.StreamProjectionMaintenance(ctx, rowColumns, cursor, fn)
	}
	return store.StreamProjection(ctx, rowColumns, cursor, fn)
}

func (p *historyProjection) refresh(ctx context.Context, store *storage.Store, budget projectionBudget) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		version, err := projectionDataVersion(ctx, store, budget)
		if err != nil {
			return err // never serve an old projection when freshness is unknowable
		}
		p.mu.RLock()
		current := p.data.cursor.Ready && p.data.version == version
		wait := p.building
		p.mu.RUnlock()
		if current {
			return nil
		}
		if wait != nil {
			// Single-flight: one builder owns the scan. Waiting readers keep
			// their own cancellation and re-check the version after it ends.
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		p.mu.Lock()
		if p.building != nil {
			wait := p.building
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Re-read the version after acquiring the build token: an older caller
		// cannot roll the projection's version back, and a commit that landed
		// while we waited is included in this scan.
		version, err = projectionDataVersion(ctx, store, budget)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		if p.data.cursor.Ready && p.data.version == version {
			p.mu.Unlock()
			return nil
		}
		base := p.data // snapshot metadata; underlying rows stay immutable until apply
		done := make(chan struct{})
		p.building = done
		p.mu.Unlock()

		// Materialize off the lock. The version was read before the scan: a
		// commit landing during it leaves the projection stamped older, so the
		// next read catches up instead of falsely declaring the rows fresh.
		var incoming []contrib
		scanned := 0
		next, full, err := projectionStream(ctx, store, budget, base.cursor, func(rows *sql.Rows) error {
			c, err := scanContrib(rows, nil) // raw spelling; query rules apply later
			if err != nil {
				return err
			}
			incoming = append(incoming, c)
			scanned++
			// Check cancellation periodically so an abandoned dashboard request
			// cannot keep a full-history decode running to completion.
			return checkFoldContext(ctx, scanned)
		})
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			p.finishBuild(done)
			return err
		}

		p.mu.Lock()
		if p.data.version != base.version || p.data.cursor != base.cursor {
			// Defensive: the build token makes this unreachable today. Never
			// apply a delta onto a different base; release and retry instead.
			p.finishBuildLocked(done)
			p.mu.Unlock()
			continue
		}
		if full {
			data, err := buildProjection(incoming, next, version)
			if err != nil {
				p.finishBuildLocked(done)
				p.mu.Unlock()
				return err
			}
			p.data = data
			p.finishBuildLocked(done)
			p.mu.Unlock()
			return nil
		}
		data := p.data
		// Compact metric-order indices are uint32; reject before narrowing or
		// mutating a published dictionary (an arithmetic guard, not a history cap).
		if uint64(len(data.rows))+uint64(len(incoming)) > math.MaxUint32 {
			p.finishBuildLocked(done)
			p.mu.Unlock()
			return errors.New("analytical row index capacity exceeded")
		}
		for _, c := range incoming {
			if c.id != "" {
				if _, exists := data.ids[c.id]; exists {
					p.finishBuildLocked(done)
					p.mu.Unlock()
					return errors.New("analytical append unexpectedly replaced an existing request")
				}
			}
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
		// growRows covers the delta append with one owner; spare capacity never
		// exceeds the incoming batch.
		data.rows = growRows(data.rows, incoming)
		if data.lineage.appendRows(incoming) {
			data.lineage.resolve()
		}
		data.metrics.append(data.rows, previousRows)
		data.cursor, data.version = next, version
		p.data = data
		p.finishBuildLocked(done)
		p.mu.Unlock()
		return nil
	}
}

// buildProjection constructs a complete replacement off the lock (destructive
// invalidation, purge or external rewrite). The caller swaps it under the
// write lock, so readers never observe a partially built table.
func buildProjection(incoming []contrib, next storage.ProjectionCursor, version int64) (projectionData, error) {
	// Compact metric-order indices are uint32; reject before narrowing.
	if uint64(len(incoming)) > math.MaxUint32 {
		return projectionData{}, errors.New("analytical row index capacity exceeded")
	}
	data := projectionData{
		ids:        make(map[string]struct{}, len(incoming)),
		cursor:     next,
		version:    version,
		rows:       make([]contrib, len(incoming)),
		hasStart:   len(incoming) > 0,
		modelCache: newModelProjectionCache(),
	}
	for i := range incoming {
		c := incoming[i]
		c.rowIndex = uint32(i + 1)
		data.dimensions.intern(&c)
		if i == 0 || c.start < data.minStart {
			data.minStart = c.start
		}
		data.noteChunkStart(c.rowIndex, c.start)
		if c.id != "" {
			data.ids[c.id] = struct{}{}
		}
		data.rows[i] = c
	}
	data.metrics.append(data.rows, 0)
	data.lineage = buildConversationTopology(data.rows)
	return data, nil
}

func (p *historyProjection) finishBuild(done chan struct{}) {
	p.mu.Lock()
	p.finishBuildLocked(done)
	p.mu.Unlock()
}

func (p *historyProjection) finishBuildLocked(done chan struct{}) {
	if p.building == done {
		p.building = nil
		close(done)
	}
}
