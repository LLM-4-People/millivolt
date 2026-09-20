package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func logRoutesForTest(t *testing.T, durable bool) (*http.ServeMux, *metrics.Buffer, *storage.Store) {
	t.Helper()
	b := metrics.NewBuffer(16)
	var s *storage.Store
	if durable {
		c := config.Default()
		var err error
		s, err = storage.Open(filepath.Join(t.TempDir(), "log.db"), storage.Options{
			WriteChanCap: c.StorageWriteChanCap, BatchCap: c.StorageBatchCap,
			FlushInterval: c.StorageFlushInterval, QueryTimeout: c.StorageQueryTimeout,
			WriteTrackCap: 32,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
	}
	mux := http.NewServeMux()
	registerLogRoutes(mux, b, s)
	return mux, b, s
}

func TestQueryRouteWithoutStorageNeverFallsThroughToInference(t *testing.T) {
	mux, _, _ := logRoutesForTest(t, false)
	forwarded := 0
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		forwarded++
		w.WriteHeader(http.StatusNoContent)
	})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(method, "/metrics/query?q=SELECT+1", nil))
		want := http.StatusMethodNotAllowed
		if method == http.MethodGet {
			want = http.StatusServiceUnavailable
		} else if response.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: missing query method contract", method)
		}
		if response.Code != want || forwarded != 0 || !json.Valid(response.Body.Bytes()) {
			t.Errorf("%s: status=%d forwarded=%d body=%s", method, response.Code, forwarded, response.Body.String())
		}
	}
}

func TestLogFilterRejectsMalformedAndNullConstraints(t *testing.T) {
	mux, b, s := logRoutesForTest(t, true)
	for _, provider := range []string{"target", "unrelated"} {
		s.Recorder(b).Record(&metrics.Record{ID: provider, Provider: provider, StatusCode: 500, Start: time.Now()})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/admin/purge", "/admin/purge/count"} {
		for _, body := range []string{
			` `, `null`, `[]`, `{"provder":"target"}`, `{"provider":"target"} {}`,
			`{"provider":"target","Provider":"unrelated"}`, `{"status_code":-1}`,
			`{"provider":null,"has_error":true}`, `{"provider":"target","before_ms":null}`,
			`{"before_ms":1,"after_ms":2}`, `{"debug":"true"}`,
		} {
			t.Run(route+body, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
				req.ContentLength = 0 // must not turn a nonempty body into a full wipe
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if n, err := s.CountWhere(t.Context(), storage.PurgeFilter{}); err != nil || n != 2 || b.Len() != 2 {
					t.Fatalf("rejected request mutated history: n=%d ring=%d err=%v", n, b.Len(), err)
				}
			})
		}
	}
	for _, body := range []string{`{}`, `{"provider":""}`, `{"has_error":false}`} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("empty filtered purge accepted: %s", body)
		}
	}
}

func TestLogCountExportAndClearSharePredicate(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			for i := range 3 {
				recorder.Record(&metrics.Record{ID: fmt.Sprint(i), Provider: "target", ConversationID: "conversation", ErrorType: "upstream", StatusCode: 500, Start: time.UnixMilli(int64(i+1) * 1000)})
			}
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			body := `{"provider":"target","conversation_id":"conversation","error_type":"upstream","after_ms":2000,"before_ms":3000}`
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge/count", strings.NewReader(body)))
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"count":1}` {
				t.Fatalf("count=%d %s", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/export?provider=target&conversation_id=conversation&error_type=upstream&after_ms=2000&before_ms=3000", nil))
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatalf("export is not a gzip artifact: %v (%s)", err, w.Body.String())
			}
			var rows []*metrics.Record
			if err := json.NewDecoder(zr).Decode(&rows); err != nil || w.Code != 200 || len(rows) != 1 || rows[0].ID != "1" {
				t.Fatalf("export=%d %s err=%v", w.Code, w.Body.String(), err)
			}
			w = httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body))
			req.ContentLength = -1
			mux.ServeHTTP(w, req)
			if w.Code != 200 || b.Len() != 2 {
				t.Fatalf("clear=%d %s ring=%d", w.Code, w.Body.String(), b.Len())
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", nil))
			if w.Code != 200 || b.Len() != 0 {
				t.Fatalf("full clear=%d ring=%d", w.Code, b.Len())
			}
		})
	}
}

// TestLogExportServesGzipArtifact pins the compression gate: the export is
// always a saved gzip artifact (application/gzip, a millivolt-logs-*.json.gz
// attachment named by the UTC stamp, no-store), and the payload gunzips to
// the exact row set the plain export produced - on both the ring-only path
// and durable storage.
func TestLogExportServesGzipArtifact(t *testing.T) {
	dispoShape := regexp.MustCompile(`^attachment; filename="millivolt-logs-(\d{8}-\d{6})\.json\.gz"$`)
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			for i, id := range []string{"gz-a", "gz-b"} {
				recorder.Record(&metrics.Record{ID: id, Provider: "neutral.example", StatusCode: 200, Start: time.UnixMilli(int64(i+1) * 1000)})
			}
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/export", nil))
			if w.Code != 200 {
				t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
				t.Fatalf("Content-Type = %q, want application/gzip", ct)
			}
			dispo := w.Header().Get("Content-Disposition")
			m := dispoShape.FindStringSubmatch(dispo)
			if m == nil {
				t.Fatalf("Content-Disposition = %q, want a millivolt-logs-<stamp>.json.gz attachment", dispo)
			}
			// The export's filename clock is UTC, one policy with the backup
			// and capture artifacts: the stamp must sit within seconds of now
			// UTC (a local-clock filename reddens here whenever local time
			// differs).
			stamp, err := time.ParseInLocation("20060102-150405", m[1], time.UTC)
			if err != nil {
				t.Fatalf("attachment stamp %q does not parse: %v", m[1], err)
			}
			if skew := time.Since(stamp); skew < -time.Minute || skew > time.Minute {
				t.Fatalf("attachment stamp %s sits %s from now UTC - the artifact filename clock must be UTC", m[1], skew)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", cc)
			}
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatalf("export is not a gzip artifact: %v", err)
			}
			var rows []*metrics.Record
			if err := json.NewDecoder(zr).Decode(&rows); err != nil {
				t.Fatalf("gunzipped export is not a JSON array: %v", err)
			}
			got := make([]string, len(rows))
			for i, r := range rows {
				got[i] = r.ID
			}
			if strings.Join(got, ",") != "gz-a,gz-b" {
				t.Fatalf("gunzipped row set = %v, want [gz-a gz-b] oldest-first", got)
			}
		})
	}
}

// TestLogExportDebugOnlyShapeSharesCompressionGate pins the structural half
// of the compression gate: the gzip artifact layer wraps the export at the
// one choke point every shape crosses (the shared writer composition in the
// route handler), so the debug-only export, the shape that embeds capture
// sidecars and grows past a gigabyte, is a gzip round-trip on the ring-only
// path and on durable storage alike. A per-branch hand-wrap that bypasses
// the choke point leaves one path uncompressed (or double-compressed) and
// reddens here on the gunzip step.
func TestLogExportDebugOnlyShapeSharesCompressionGate(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			recorder.Record(&metrics.Record{ID: "plain-row", Provider: "neutral.example", StatusCode: 200, Start: time.UnixMilli(1000)})
			recorder.Record(&metrics.Record{ID: "debug-row", Provider: "neutral.example", StatusCode: 200, Start: time.UnixMilli(2000), Debug: true})
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
				payload := []byte(`{"schema":"millivolt.debug/v1","id":"debug-row"}`)
				if err := s.SaveDebugCapture(t.Context(), "debug-row", "debug-row", time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), payload); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/export?debug=1", nil))
			if w.Code != 200 {
				t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
				t.Fatalf("Content-Type = %q, want application/gzip", ct)
			}
			if _, ok := w.Header()["Content-Length"]; ok {
				t.Fatal("streamed export must not pin Content-Length")
			}
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatalf("debug-only export is not a gzip artifact: %v", err)
			}
			raw, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("gunzip failed: %v", err)
			}
			var rows []*metrics.Record
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatalf("gunzipped export is not a JSON array: %v", err)
			}
			if len(rows) != 1 || rows[0].ID != "debug-row" {
				t.Fatalf("gunzipped rows = %v, want only debug-row", rows)
			}
			if s != nil && !strings.Contains(string(raw), `"debug_capture"`) {
				t.Fatal("durable debug-only export lost the capture sidecar")
			}
		})
	}
}

func TestLogExportRejectsInvalidQueryAndReportsDatabaseFailure(t *testing.T) {
	mux, b, s := logRoutesForTest(t, true)
	for _, q := range []string{"unknown=x", "provider=a&provider=b", "has_error=true", "debug=2", "status_code=1000", "before_ms=-1", "after_ms=2&before_ms=1", "bad=%zz"} {
		req := httptest.NewRequest(http.MethodGet, "/metrics/export", nil)
		req.URL.RawQuery = q
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("query %s accepted: %d", q, w.Code)
		}
	}
	b.Record(&metrics.Record{ID: "ring", StatusCode: 200})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/metrics/export", "/admin/purge", "/admin/purge/count"} {
		method := http.MethodPost
		if path == "/metrics/export" {
			method = http.MethodGet
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 500 || b.Len() != 1 {
			t.Fatalf("failed %s status=%d ring=%d", path, w.Code, b.Len())
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", nil).WithContext(ctx))
	if w.Code != 500 || b.Len() != 1 {
		t.Fatal("canceled purge changed live ring")
	}
}

// TestPurgeResponseWireKeysStrict pins the /admin/purge response to exactly
// {"ok": true}. The W15 drop removed the wire-only deleted and
// buffer_removed counts (the store-level PurgeResult keeps them); the
// count-focused tests never read the response body, so a re-added key must
// redden on this strict decode instead of passing unnoticed.
func TestPurgeResponseWireKeysStrict(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			recorder.Record(&metrics.Record{ID: "purge-strict", Provider: "neutral.example", StatusCode: 200, Start: time.Now()})
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			for _, body := range []string{`{"provider":"neutral.example"}`, ""} {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("purge %q = %d %s", body, w.Code, w.Body.String())
				}
				var purge struct {
					OK bool `json:"ok"`
				}
				dec := json.NewDecoder(w.Body)
				dec.DisallowUnknownFields()
				if err := dec.Decode(&purge); err != nil {
					t.Fatalf("strict decode of the purge response: %v (%s)", err, w.Body.String())
				}
				if !purge.OK {
					t.Fatalf("purge response ok = false: %s", w.Body.String())
				}
			}
			if b.Len() != 0 {
				t.Fatalf("ring = %d, want both purges to clear it", b.Len())
			}
		})
	}
}

type failingLogExportWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *failingLogExportWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

func TestLogExportAbortsIncompleteHTTPDownload(t *testing.T) {
	mux, b, s := logRoutesForTest(t, true)
	s.Recorder(b).Record(&metrics.Record{ID: "export", Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	w := &failingLogExportWriter{ResponseRecorder: httptest.NewRecorder()}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("partial export did not abort HTTP: %v", got)
		}
		if json.Valid(w.Body.Bytes()) {
			t.Error("partial export looks complete")
		}
		if w.writes != 2 {
			t.Errorf("write failure was not exercised: %d writes", w.writes)
		}
	}()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/export", nil))
}
