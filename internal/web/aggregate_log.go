package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// jsonBufferPool reuses the encoder's scratch buffer: chart bundles and the
// bootstrap payload run to hundreds of kilobytes, and a fresh right-sized
// slice per poll is pure garbage. Oversized buffers are dropped instead of
// pinned in the pool.
var jsonBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const jsonBufferRetain = 1 << 20

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Every aggregate carries live request metadata; never let a browser or
	// intermediary retain a stale copy of request previews or filter results.
	w.Header().Set("Cache-Control", "no-store")
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	// application/json is never parsed as HTML by a browser: escaping <, > and
	// & only inflates URLs, query strings and previews with \u003c sequences.
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	if err != nil {
		releaseJSONBuffer(buf)
		log.Printf("agg encode: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(adminjson.ErrorBody("aggregate cannot be represented"))
		return
	}
	data := buf.Bytes()
	if _, err := w.Write(data); err != nil {
		// The response is already streaming; nothing to repair.
		_ = err
	}
	releaseJSONBuffer(buf)
}

// releaseJSONBuffer clears the pooled encoder scratch before returning it.
// Reset only shortens the slice; the backing array keeps the previous
// dashboard JSON, including request previews and filter values, until the next
// encode happens to overwrite it or the pool entry is collected. Clearing it
// costs one pass over a buffer this request just wrote. The retention bound
// lives here so both the success and encode-error paths agree: an oversized
// buffer is dropped instead of pinned in the pool, and the clear never scans
// more than jsonBufferRetain bytes.
func releaseJSONBuffer(buf *bytes.Buffer) {
	if buf.Cap() > jsonBufferRetain {
		return
	}
	buf.Reset()
	scratch := buf.Bytes()
	clear(scratch[:cap(scratch)])
	jsonBufferPool.Put(buf)
}

// Live-log page: GET /metrics/agg/log?before_ms=&limit=&s=&f=…
// Returns older-than-cursor records matching the SAME scope the request
// log uses (status + explorer filters), newest-first. Safety guardrails
// (not tunables): page size is config.DashLogRowsMin..Max, scan at most logScanMax rows per call.
const (
	logScanBatch = 512
	logScanMax   = 4096
)

// logPagePlan is the single classification owner for the live-log scope:
// exact stored predicates SQL enforces before decode, plus the residual
// Go-owned scope the authoritative matcher must still evaluate. allPushed
// reports that every active predicate is represented exactly, so each
// candidate is expected to be an output row.
type logPagePlan struct {
	filter    storage.LogFilter
	goFilters []scopeFilter
	goStatus  string
	allPushed bool
}

// planLogPage derives one request's pushdown and residual scope. Exact stored
// dimensions become SQL predicates; everything the projection derives (status
// classes, errors, tools, time buckets, canonical models) stays in the
// authoritative Go matcher and is matched against the narrow candidate read.
func planLogPage(fs []scopeFilter, statusCode string) logPagePlan {
	plan := logPagePlan{allPushed: true}
	// Group by dimension first: OR within one dimension's values, AND across
	// groups. A group that cannot be represented exactly in SQL (canonical
	// models, status classes, tools, errors, time buckets) stays wholly
	// Go-owned; pushing a subset as AND would narrow the candidate set below
	// the group's union.
	var done uint32
	for i := range fs {
		bit := fs[i].bit
		if bit == 0 {
			bit = scopeDimBit(fs[i].dim)
		}
		if done&bit != 0 {
			continue
		}
		done |= bit
		group := make([]scopeFilter, 0, 1)
		for j := i; j < len(fs); j++ {
			if fs[j].dim == fs[i].dim {
				group = append(group, fs[j])
			}
		}
		var field storage.LogField
		pushable := true
		switch fs[i].dim {
		case "client":
			field = storage.LogClient
		case "provider":
			field = storage.LogProvider
		case "key":
			field = storage.LogKeyHash
		case "conversation":
			field = storage.LogConversation
		case "model":
			// The canonical matcher trims the stored spelling even with no
			// rules, so raw SQL equality would silently drop a stored
			// " gpt-4 " that canonically equals "gpt-4". Keep every model
			// filter Go-owned; a growing scan is the correct cost.
			pushable = false
		default:
			pushable = false
		}
		if !pushable {
			plan.goFilters = append(plan.goFilters, group...)
			plan.allPushed = false
			continue
		}
		if len(group) == 1 {
			plan.filter.All = append(plan.filter.All, storage.LogPredicate{Field: field, Value: group[0].id})
			continue
		}
		// One dimension's repeated values stay one alternative group. Separate
		// dimensions must remain separate groups so the SQL planner intersects
		// them instead of flattening every value into one broad OR.
		predicates := make([]storage.LogPredicate, 0, len(group))
		for _, f := range group {
			predicates = append(predicates, storage.LogPredicate{Field: field, Value: f.id})
		}
		plan.filter.Any = append(plan.filter.Any, predicates)
	}
	if statusCode != "" {
		if canonical, ok := canonicalStatusSelector(statusCode); ok && !liveStatusFilter(canonical) {
			plan.filter.All = append(plan.filter.All, storage.LogPredicate{Field: storage.LogStatus, Value: canonical})
		} else {
			plan.goStatus = statusCode
			plan.allPushed = false
		}
	}
	return plan
}

// logPageQuery preserves the original classification contract for callers and
// tests that only need the SQL filter and the all-pushed verdict.
func logPageQuery(fs []scopeFilter, statusCode string) (filter storage.LogFilter, allPushed bool) {
	plan := planLogPage(fs, statusCode)
	return plan.filter, plan.allPushed
}

// logCandidateMatch evaluates the residual Go-owned scope against the narrow
// candidate projection. Exact predicates already enforced by the candidate
// query are deliberately absent from the plan, so every field this matcher
// reads comes from the scanned row itself.
func logCandidateMatch(c *storage.LogCandidate, goStatus string, fs []scopeFilter, mcz *modelCanonizer) bool {
	r := metrics.Record{
		StatusCode: c.Status, ErrorType: c.ErrorType, ErrorCode: c.ErrorCode,
		ErrorMsg: c.ErrorMsg, Attempts: c.Attempts,
		Start: time.UnixMilli(c.Cursor.StartMs),
	}
	cc := contrib{
		id: c.Cursor.ID, start: c.Cursor.StartMs, status: c.Status,
		labels: &contribLabels{model: mcz.model(c.Model)},
		toolsL: c.ToolNames,
		ent:    errorEntries(&r),
	}
	return scopedMatch(&cc, goStatus, fs)
}

// HandleLogPage starts at durable newest, then pages below an exact (time,id)
// cursor. The ring is arrival-ordered and cannot supply a safe history cutoff.
func (a *AggAPI) HandleLogPage(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	// s is this surface's only single-occurrence scope key; the repeated f=
	// list keeps its grammar, and the before_ms/before_id/limit duplicates
	// stay behind their existing pairing gates below.
	if err := adminjson.DuplicateQueryKey(q, "s"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	fs, statusCode, ok := parseScope(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid scope")
		return
	}
	mcz := a.canonizer()
	var cursor *storage.LogCursor
	ms, hasMs := q["before_ms"]
	ids, hasID := q["before_id"]
	if hasMs != hasID || (hasMs && (len(ms) != 1 || len(ids) != 1)) {
		adminjson.WriteError(w, http.StatusBadRequest, "before_ms and before_id must appear together once")
		return
	}
	if hasMs {
		n, err := strconv.ParseInt(ms[0], 10, 64)
		if err != nil {
			adminjson.WriteError(w, http.StatusBadRequest, "invalid before_ms")
			return
		}
		cursor = &storage.LogCursor{StartMs: n, ID: ids[0]}
	}
	v := q.Get("limit")
	if v == "" || len(q["limit"]) != 1 {
		adminjson.WriteError(w, http.StatusBadRequest, "limit required")
		return
	}
	limit, err := strconv.Atoi(v)
	if err != nil || limit < config.DashLogRowsMin || limit > config.DashLogRowsMax {
		adminjson.WriteError(w, http.StatusBadRequest, fmt.Sprintf("limit must be %d..%d", config.DashLogRowsMin, config.DashLogRowsMax))
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	// A live-only status class can never match durable history (those classes
	// describe the in-memory pending set). Return the empty page without
	// decoding up to logScanMax rows that the Go matcher would reject.
	if liveStatusFilter(statusCode) {
		writeJSON(w, map[string]any{
			"records":     []metrics.ObservedRecord{},
			"model_canon": a.ObserveModels(false, nil),
			"more":        false,
			"cursor_ms":   int64(0),
			"cursor_id":   "",
		})
		return
	}
	// Push exact, SQL-safe dimensions into the page query. Model equality,
	// status classes, errors, tools and time buckets stay in the authoritative
	// Go matcher, which reads the narrow candidate projection instead of full
	// records.
	plan := planLogPage(fs, statusCode)
	accepted := make([]storage.LogCursor, 0, limit)
	scanned := 0
	hitEnd := a.store == nil
	batch := min(limit, logScanBatch)
	if a.store != nil {
		for scanned < logScanMax && len(accepted) < limit {
			if plan.allPushed {
				// Every scanned row is an expected output row, so ask for
				// exactly what the page still needs.
				batch = limit - len(accepted)
			}
			if remain := logScanMax - scanned; remain < batch {
				batch = remain
			}
			if batch <= 0 {
				break
			}
			candidates, err := a.store.QueryBeforeCandidates(ctx, cursor, &plan.filter, batch)
			if err != nil {
				log.Printf("agg log page: %v", err)
				adminjson.WriteError(w, http.StatusInternalServerError, "log page failed")
				return
			}
			if len(candidates) == 0 {
				hitEnd = true
				break
			}
			scanned += len(candidates)
			for i := range candidates {
				cand := &candidates[i]
				cursor = &storage.LogCursor{StartMs: cand.Cursor.StartMs, ID: cand.Cursor.ID}
				if !plan.allPushed && !logCandidateMatch(cand, plan.goStatus, plan.goFilters, mcz) {
					continue
				}
				accepted = append(accepted, cand.Cursor)
				if len(accepted) >= limit {
					break
				}
			}
			if len(accepted) >= limit {
				break
			}
			if len(candidates) < batch {
				hitEnd = true
				break
			}
			if !plan.allPushed {
				// Go-only predicates may reject most rows: grow from the page
				// size towards the per-call batch/scan caps instead of decoding
				// a full batch on the first request.
				batch = min(batch*2, logScanBatch)
			}
		}
	}
	// The full response rows are materialized once, for the accepted page
	// only: a rejected candidate scan never pays for prompts, previews,
	// headers, or request parameters. The storage call is bounded by the page
	// size and preserves the scan order.
	out := make([]*metrics.Record, 0, limit)
	if len(accepted) > 0 {
		out, err = a.store.LogPageRecords(ctx, accepted)
		if err != nil {
			log.Printf("agg log page: %v", err)
			adminjson.WriteError(w, http.StatusInternalServerError, "log page failed")
			return
		}
	}
	var cursorMs int64
	var cursorID string
	if cursor != nil {
		cursorMs, cursorID = cursor.StartMs, cursor.ID
	}
	writeJSON(w, map[string]any{
		"records":     metrics.ObserveRecords(out),
		"model_canon": a.ObserveModels(false, out),
		"more":        !hitEnd && a.store != nil,
		"cursor_ms":   cursorMs,
		"cursor_id":   cursorID,
	})
}
