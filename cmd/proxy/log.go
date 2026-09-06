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

func logHTTPError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (w *exportWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func registerLogRoutes(mux *http.ServeMux, buffer *metrics.Buffer, store *storage.Store) {
	// Reserve the read-only SQL endpoint even when storage is disabled, so
	// requests cannot fall through to inference. HandleQuery owns both cases.
	mux.HandleFunc("/metrics/query", store.HandleQuery)
	mux.HandleFunc("/metrics/export", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodGet) {
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			logHTTPError(w, "invalid query", http.StatusBadRequest)
			return
		}
		f, err := exportLogFilter(q)
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="millivolt-logs-`+time.Now().Format("20060102-150405")+`.json"`)
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			rows := buffer.Snapshot()
			out := make([]*metrics.Record, 0, len(rows))
			for _, row := range rows {
				if f.MatchRecord(row) {
					out = append(out, row)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		ew := &exportWriter{ResponseWriter: w}
		if _, err := store.ExportWhere(r.Context(), f, ew); err != nil {
			if !ew.started {
				w.Header().Del("Content-Disposition")
				logHTTPError(w, "export failed", http.StatusInternalServerError)
			} else {
				log.Printf("metrics export failed: %v", err)
				// Do not complete a successful 200 download after a read failure.
				// net/http aborts the connection/stream without a panic stack.
				panic(http.ErrAbortHandler)
			}
		}
	})
	mux.HandleFunc("/metrics/purge", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		f, hasBody, err := decodeLogFilter(w, r)
		if err == nil && hasBody && f.IsEmpty() {
			err = errors.New("empty purge filter; send no body to delete everything")
		}
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		var filter *storage.PurgeFilter
		if hasBody {
			filter = &f
		}
		var result storage.PurgeResult
		if store != nil {
			result, err = store.Clear(r.Context(), filter, buffer)
		} else if err = r.Context().Err(); err == nil {
			if filter == nil {
				buffer.Reset()
			} else {
				result.BufferRemoved = buffer.RemoveWhere(filter.MatchRecord)
			}
		}
		if err != nil {
			logHTTPError(w, "purge failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			OK bool `json:"ok"`
			storage.PurgeResult
		}{true, result})
	})
	mux.HandleFunc("/metrics/purge/count", func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnless(w, r, http.MethodPost) {
			return
		}
		f, _, err := decodeLogFilter(w, r)
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
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
			logHTTPError(w, "count failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"count": n})
	})
}
