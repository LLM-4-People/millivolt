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
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// decodeLogFilter distinguishes an explicit, genuinely empty full-history
// command from a malformed/nonempty document. Content-Length is never authority.
func decodeLogFilter(w http.ResponseWriter, r *http.Request) (storage.PurgeFilter, bool, error) {
	var f storage.PurgeFilter
	err := adminjson.Decode(w, r, &f)
	if errors.Is(err, io.EOF) {
		return f, false, nil
	}
	if err != nil {
		return f, true, err
	}
	return f, true, f.Validate()
}

// exportLogFilter exposes exactly the same predicate fields as JSON count and
// clear. Unknown/duplicate keys and invalid booleans fail closed, never broaden.
func exportLogFilter(q url.Values) (storage.PurgeFilter, error) {
	var f storage.PurgeFilter
	for key, values := range q {
		if len(values) != 1 {
			return f, fmt.Errorf("duplicate %s", key)
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

type exportWriter struct {
	http.ResponseWriter
	started bool
}

func (w *exportWriter) Write(p []byte) (int, error) {
	w.started = true
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
	mux.HandleFunc("/metrics/query", store.HandleQuery)
	mux.HandleFunc("/metrics/export", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodGet) {
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
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
	mux.HandleFunc("/admin/purge", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		f, hasBody, err := decodeLogFilter(w, r)
		if err == nil && hasBody && f.IsEmpty() {
			err = errors.New("empty purge filter; send no body to delete everything")
		}
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		var filter *storage.PurgeFilter
		if hasBody {
			filter = &f
		}
		if store != nil {
			// The deletion counts are storage.PurgeResult, the store-level
			// contract; no wire consumer reads them (the dashboard confirms
			// from the count preview and bootstraps after), so the response
			// carries ok only.
			_, err = store.Clear(r.Context(), filter, buffer)
		} else if err = r.Context().Err(); err == nil {
			if filter == nil {
				buffer.Reset()
			} else {
				buffer.RemoveWhere(filter.MatchRecord)
			}
		}
		if err != nil {
			log.Printf("purge failed: %v", err)
			adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/admin/purge/count", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		f, _, err := decodeLogFilter(w, r)
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
