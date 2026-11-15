package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/mcp"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// logFilterRequest is the one wire document of both purge routes, decoded by
// decodeLogFilter for the count and the delete alike. The filter keeps its own
// strict field grammar (PurgeFilter.UnmarshalJSON) and gains no field;
// reviewed_count is the optional row count the caller previewed. It is absent
// for a plain preview, for a bodyless full deletion, and for callers that do
// not preview first.
type logFilterRequest struct {
	Filter        storage.PurgeFilter `json:"filter"`
	ReviewedCount *int64              `json:"reviewed_count"`
}

// decodeLogFilter distinguishes an explicit, genuinely empty full-history
// command from a malformed/nonempty document. Content-Length is never authority.
func decodeLogFilter(w http.ResponseWriter, r *http.Request) (logFilterRequest, bool, error) {
	var req logFilterRequest
	err := adminjson.Decode(w, r, &req)
	if errors.Is(err, io.EOF) {
		return req, false, nil
	}
	if err != nil {
		return req, true, err
	}
	if req.ReviewedCount != nil && *req.ReviewedCount < 0 {
		return req, true, errors.New("reviewed_count must be zero or greater")
	}
	return req, true, req.Filter.Validate()
}

// rejectPurgeQuery denies every query key on the body-only purge routes before
// their body is decoded. The documented purge predicate travels only in the
// JSON body, so an ignored query key would select the default, broader
// action: POST /admin/purge?provider=alpha with no body would delete all
// history instead of the named provider. The shared strict parse runs first,
// so a malformed pair the lenient read would silently drop cannot turn a
// requested filter into that same full delete. UnknownQueryKey then owns the
// closed-set rule and its "unknown key <name>" wording; these routes consume
// no keys, so duplicate and case-variant spellings are unlisted keys too and
// fail the same way.
func rejectPurgeQuery(w http.ResponseWriter, r *http.Request) bool {
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid query")
		return false
	}
	if err := adminjson.UnknownQueryKey(q); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// exportLogFilter exposes exactly the same predicate fields as JSON count and
// clear. Unknown/duplicate keys and invalid booleans fail closed, never broaden.
func exportLogFilter(q url.Values) (storage.PurgeFilter, error) {
	var f storage.PurgeFilter
	for key, values := range q {
		if err := adminjson.DuplicateQueryKey(q, key); err != nil {
			return f, err
		}
		v := values[0]
		var err error
		switch key {
		case "provider":
			f.Provider = v
		case "model":
			f.Model = v
		case "client":
			f.Client = v
		case "conversation_id":
			f.ConversationID = v
		case "error_type":
			f.ErrorType = v
		case "status_code":
			f.StatusCode, err = strconv.Atoi(v)
		case "before_ms":
			f.BeforeMs, err = strconv.ParseInt(v, 10, 64)
		case "after_ms":
			f.AfterMs, err = strconv.ParseInt(v, 10, 64)
		case "has_error", "debug":
			if v != "0" && v != "1" {
				err = errors.New("expected 0 or 1")
			}
			if key == "has_error" {
				f.HasError = v == "1"
			} else {
				f.HasDebug = v == "1"
			}
		default:
			return f, fmt.Errorf("unknown filter %s", key)
		}
		if err != nil {
			return f, fmt.Errorf("invalid %s", key)
		}
	}
	return f, f.Validate()
}

// purgeResponse is the /admin/purge success document: the store-level
// PurgeResult on the wire so an operator sees what the deletion removed.
type purgeResponse struct {
	Deleted       int64 `json:"deleted"`
	BufferRemoved int   `json:"buffer_removed"`
}

type exportWriter struct {
	http.ResponseWriter
	started bool
}

func (w *exportWriter) Write(p []byte) (int, error) {
	w.started = true
	// Export streaming writes a gzip artifact, not HTML.
	return w.ResponseWriter.Write(p)
}

// exportFailed answers a failed export read after the artifact headers were
// staged. The not-started branch is reachable only for failures before the
// first write - the store's filter Validate and SQL open failures - so
// nothing reached the client yet: retract the attachment name and answer
// the store error. A failure after a write leaves a partial artifact on the
// wire that must not complete as a successful 200 download; net/http aborts
// the connection or stream without a panic stack.
func exportFailed(w http.ResponseWriter, ew *exportWriter, err error) {
	if !ew.started {
		w.Header().Del("Content-Disposition")
		adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	panic(http.ErrAbortHandler)
}

func registerLogRoutes(mux *http.ServeMux, buffer *metrics.Buffer, store *storage.Store) {
	// Reserve the read-only SQL endpoint even when storage is disabled, so
	// requests cannot fall through to inference (HandleQuery owns both
	// cases). It is operator-gated like every /metrics route: gatedPath
	// gates the whole namespace, read surfaces included.
	mux.HandleFunc(string(mcp.RouteQuery), store.HandleQuery)
	mux.HandleFunc("/metrics/export", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodGet) {
			return
		}
		q, err := adminjson.StrictQuery(r)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid query")
			return
		}
		f, err := exportLogFilter(q)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		// The export is always a saved gzip artifact: a browser decompresses
		// Content-Encoding before saving, so transit compression can never
		// shrink what lands on disk. No Content-Length: the payload streams
		// row-by-row and chunked framing is correct. The one artifact-header
		// owner composes the name (millivolt-logs-<UTC stamp>.json.gz),
		// disposition, content type and no-store.
		proxy.WriteArtifactHeaders(w, "logs", "json.gz", "application/gzip", time.Now(), "")
		// The compression gate is the one choke point every export shape
		// crosses: the ring snapshot and the durable stream below both write
		// through this single gzip artifact writer, so matching, all and
		// debug-only exports inherit the same compression on either backend.
		// The gzip layer cannot touch the socket before the first payload
		// write (the 10-byte header is emitted lazily with the first row,
		// never at construction), so a store that fails before producing a
		// row still leaves the socket untouched and exportFailed's 500
		// retraction stays reachable.
		ew := &exportWriter{ResponseWriter: w}
		zw := proxy.NewArtifactGzipWriter(ew)
		if store == nil {
			rows := buffer.Snapshot()
			out := make([]*metrics.Record, 0, len(rows))
			for _, row := range rows {
				if f.MatchRecord(row) {
					out = append(out, row)
				}
			}
			err = json.NewEncoder(zw).Encode(out)
		} else {
			_, err = store.ExportWhere(r.Context(), f, zw)
		}
		if err == nil {
			// Close runs on the normal path so the deflate trailer flushes; a
			// trailer that cannot reach the client is the same incomplete
			// download.
			err = zw.Close()
		}
		if err != nil {
			log.Printf("metrics export failed: %v", err)
			exportFailed(w, ew, err)
		}
	})
	// Destructive purge stays in the /admin operator action plane, never the
	// /metrics namespace. The operator credential gates both planes alike
	// (gatedPath gates /metrics too), so the split is action vs read, never
	// open-read vs gated.
	mux.HandleFunc(string(mcp.RoutePurge), func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		if !rejectPurgeQuery(w, r) {
			return
		}
		req, hasBody, err := decodeLogFilter(w, r)
		if err == nil && hasBody && req.Filter.IsEmpty() {
			err = errors.New("empty purge filter; send no body to delete everything")
		}
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		var filter *storage.PurgeFilter
		var reviewed *int64
		if hasBody {
			filter = &req.Filter
			reviewed = req.ReviewedCount
		}
		var result storage.PurgeResult
		if store != nil {
			// The store counts the reviewed row set inside the delete
			// transaction and refuses a moved population with a distinct
			// error, so a preview from this route can never authorize a
			// broader deletion than the operator saw.
			result, err = store.Clear(r.Context(), filter, reviewed, buffer)
		} else if err = r.Context().Err(); err == nil {
			// Without a store there is no transaction to count in. The ring
			// is the only history and it cannot grow while the handler runs,
			// so the check is exact - but it must run before the removal, or a
			// refused deletion would still have emptied it.
			if reviewed != nil {
				live := int64(buffer.Len())
				if filter != nil {
					live = 0
					for _, row := range buffer.Snapshot() {
						if filter.MatchRecord(row) {
							live++
						}
					}
				}
				if live != *reviewed {
					err = storage.ErrPurgeCountChanged
				}
			}
			if err == nil {
				if filter == nil {
					result.BufferRemoved = buffer.Len()
					buffer.Reset()
				} else {
					result.BufferRemoved = buffer.RemoveWhere(filter.MatchRecord)
				}
			}
		}
		if errors.Is(err, storage.ErrPurgeCountChanged) {
			auditLog("purge", "outcome", "refused", "filter", describeFilter(filter),
				"reviewed", reviewedText(reviewed), "error", err.Error())
			adminjson.WriteErrorJSON(w, http.StatusConflict,
				"the row set changed since the preview; preview again and confirm the new count")
			return
		}
		if err != nil {
			// The failure line is written before the response, so a lost or
			// truncated response is still diagnosable and a retry can be
			// compared against what was actually attempted.
			auditLog("purge", "outcome", "failed", "filter", describeFilter(filter),
				"reviewed", reviewedText(reviewed), "error", err.Error())
			adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		// One line, same wording family as the failure path, so a purge is
		// reconstructable after the response is gone.
		auditLog("purge", "outcome", "ok", "filter", describeFilter(filter),
			"reviewed", reviewedText(reviewed), "deleted", result.Deleted,
			"buffer_removed", result.BufferRemoved)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(purgeResponse{Deleted: result.Deleted, BufferRemoved: result.BufferRemoved})
	})
	mux.HandleFunc(string(mcp.RoutePurgeCount), func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		if !rejectPurgeQuery(w, r) {
			return
		}
		req, hasBody, err := decodeLogFilter(w, r)
		if err == nil && hasBody && req.Filter.IsEmpty() {
			// Same guard as the delete: "{}" reads as a filter body but the
			// delete route only accepts a bodyless call as the full set.
			err = errors.New("empty purge filter; send no body to delete everything")
		}
		f := req.Filter
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		var n int64
		if store != nil {
			n, err = store.CountWhere(r.Context(), f)
		} else {
			for _, row := range buffer.Snapshot() {
				if f.MatchRecord(row) {
					n++
				}
			}
		}
		if err != nil {
			log.Printf("count failed: %v", err)
			adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"count": n})
	})
}
