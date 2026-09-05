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
}

type historyProjection struct {
	mu   sync.RWMutex
	data projectionData
}

// PreloadHistory moves the one initial durable decode ahead of HTTP readiness.
// It preloads the shared data source, not hardcoded default dashboard views.
// A failed preload publishes nothing; later reads can retry the same builder.
func (a *AggAPI) PreloadHistory(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return a.withProjection(ctx, func(*projectionData) error { return nil })
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
		if c.id != "" {
			data.ids[c.id] = struct{}{}
		}
	}
	if full {
		data.rows = incoming
	} else {
		data.rows = append(data.rows, incoming...)
	}
	data.metrics.append(data.rows, previousRows)
	data.cursor, data.version = next, version
	// Keep the PRE-scan version. If a commit landed during materialization,
	// the next read catches it up instead of falsely stamping old rows fresh.
	p.data = data
	return nil
}
