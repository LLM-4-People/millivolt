package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
			` `, `null`, `[]`, `{"filter":{"provder":"target"}}`, `{"filter":{"provider":"target"}} {}`,
			`{"filter":{"provider":"target","Provider":"unrelated"}}`, `{"filter":{"status_code":-1}}`,
			`{"filter":{"provider":null,"has_error":true}}`, `{"filter":{"provider":"target","before_ms":null}}`,
			`{"filter":{"before_ms":1,"after_ms":2}}`, `{"filter":{"debug":"true"}}`,
			`{"provider":"target"}`, `{"filter":null}`, `{"filter":{"provider":"target"},"reviewed_count":-1}`,
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
	for _, body := range []string{`{}`, `{"filter":{}}`, `{"filter":{"provider":""}}`, `{"filter":{"has_error":false}}`, `{"reviewed_count":1}`} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("empty filtered purge accepted: %s", body)
		}
	}
}

// TestPurgeRoutesRejectQueryFilters pins the body-only purge grammar: the
// documented predicate never travels in the query string, and an ignored key
// would run the default, broader action (?provider=alpha with no body deleted
// all history). Every query key, including repeated and case-variant
// spellings, is denied before the body is read, and a malformed pair is a 400
// rather than a dropped query; the documented empty-body and body-filter
// commands keep working afterward.
func TestPurgeRoutesRejectQueryFilters(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			for _, provider := range []string{"target", "unrelated"} {
				recorder.Record(&metrics.Record{ID: provider, Provider: provider, StatusCode: 500, Start: time.Now()})
			}
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			untouched := func(t *testing.T, label string) {
				t.Helper()
				if b.Len() != 2 {
					t.Fatalf("%s: ring=%d, want 2", label, b.Len())
				}
				if s != nil {
					n, err := s.CountWhere(t.Context(), storage.PurgeFilter{})
					if err != nil || n != 2 {
						t.Fatalf("%s: durable=%d err=%v, want 2", label, n, err)
					}
				}
			}
			for _, tc := range []struct {
				name    string
				query   string
				body    string
				wantErr string
			}{
				{"filter key only", "provider=alpha", "", "unknown key provider"},
				{"repeated filter key", "provider=alpha&provider=target", "", "unknown key provider"},
				{"case variant", "Provider=alpha", "", "unknown key Provider"},
				{"bare key", "provider", "", "unknown key provider"},
				{"malformed pair", "provider=%zz", "", "invalid query"},
				{"unknown key with filter body", "inspect=1", `{"filter":{"provider":"target"}}`, "unknown key inspect"},
				{"unknown key before a malformed body", "provider=alpha", "not json", "unknown key provider"},
			} {
				for _, route := range []string{"/admin/purge", "/admin/purge/count"} {
					t.Run(tc.name+" "+route, func(t *testing.T) {
						var body io.Reader
						if tc.body != "" {
							body = strings.NewReader(tc.body)
						}
						w := httptest.NewRecorder()
						mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, route+"?"+tc.query, body))
						var got struct {
							Error string `json:"error"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusBadRequest || got.Error != tc.wantErr || w.Header().Get("Content-Type") != "application/json" {
							t.Fatalf("status=%d type=%q body=%q, want 400 application/json %q", w.Code, w.Header().Get("Content-Type"), w.Body.String(), tc.wantErr)
						}
						untouched(t, tc.name+" "+route)
					})
				}
			}
			// The documented body-filter command still deletes exactly the
			// matching rows, and a genuinely query-less, body-less request still
			// deletes everything.
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(`{"filter":{"provider":"target"}}`)))
			if w.Code != http.StatusOK || b.Len() != 1 {
				t.Fatalf("filtered purge: status=%d body=%q ring=%d, want 200 and one row", w.Code, w.Body.String(), b.Len())
			}
			if s != nil {
				n, err := s.CountWhere(t.Context(), storage.PurgeFilter{})
				if err != nil || n != 1 {
					t.Fatalf("filtered purge: durable=%d err=%v, want one row", n, err)
				}
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", nil))
			if w.Code != http.StatusOK || b.Len() != 0 {
				t.Fatalf("full purge: status=%d body=%q ring=%d, want 200 and no rows", w.Code, w.Body.String(), b.Len())
			}
			if s != nil {
				n, err := s.CountWhere(t.Context(), storage.PurgeFilter{})
				if err != nil || n != 0 {
					t.Fatalf("full purge: durable=%d err=%v, want no rows", n, err)
				}
			}
		})
	}
}

// TestPurgeRingRemovalStopsAtTheCountBoundary pins the ring half of R1-4:
// without a store the ring is the only history, so the removal boundary is
// taken before the count and a matching record that finalizes between the
// snapshot and the removal survives - it was never in the previewed count.
func TestPurgeRingRemovalStopsAtTheCountBoundary(t *testing.T) {
	b := metrics.NewBuffer(20000)
	mux := http.NewServeMux()
	registerLogRoutes(mux, b, nil)
	exaggerated, conflicts := 0, 0
	for range 200 {
		b.Reset()
		for i := range 19000 {
			b.Record(&metrics.Record{ID: fmt.Sprintf("seed-%d", i), Provider: "steady", StatusCode: 200})
		}
		reviewed := int64(b.Len())
		done := make(chan struct{})
		// One arrival per round: when it lands while the handler's ring
		// snapshot holds the read side, it queues as the next writer and lands
		// right after the count, before the removal.
		go func() {
			defer close(done)
			b.Record(&metrics.Record{ID: "late", Provider: "steady", StatusCode: 200})
		}()
		body := fmt.Sprintf(`{"filter":{"provider":"steady"},"reviewed_count":%d}`, reviewed)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body)))
		<-done
		if w.Code == http.StatusConflict {
			conflicts++
			continue
		}
		if w.Code != http.StatusOK {
			t.Fatalf("filtered purge = %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			BufferRemoved int `json:"buffer_removed"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if int64(resp.BufferRemoved) > reviewed {
			exaggerated++
		}
	}
	t.Logf("conflicts=%d exaggerated=%d", conflicts, exaggerated)
	if exaggerated > 0 {
		t.Fatalf("%d successful deletes removed more ring rows than the preview confirmed", exaggerated)
	}
}

// TestPurgeEmptyBodyVersusEmptyFilter pins one empty-filter guard across both
// body-only routes: "{}" reads as a filter body, while a bodyless post means
// the full set, so the preview must answer the same 400 as the delete instead
// of counting everything under an ambiguous body. A bodyless preview stays
// valid and counts the full set.
func TestPurgeEmptyBodyVersusEmptyFilter(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			recorder.Record(&metrics.Record{ID: "only", Provider: "target", StatusCode: 200, Start: time.Now()})
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{"/admin/purge", "/admin/purge/count"} {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "send no body") {
					t.Fatalf("%s with {} = %d %s, want the empty-filter 400", path, w.Code, w.Body.String())
				}
			}
			if b.Len() != 1 {
				t.Fatalf("rejected calls removed live rows: ring=%d, want 1", b.Len())
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge/count", nil))
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"count":1}` {
				t.Fatalf("bodyless count = %d %s, want the full count", w.Code, w.Body.String())
			}
		})
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
			body := `{"filter":{"provider":"target","conversation_id":"conversation","error_type":"upstream","after_ms":2000,"before_ms":3000}}`
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
	for _, q := range []string{"has_error=true", "debug=2", "status_code=1000", "before_ms=-1", "after_ms=2&before_ms=1", "bad=%zz"} {
		req := httptest.NewRequest(http.MethodGet, "/metrics/export", nil)
		req.URL.RawQuery = q
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("query %s accepted: %d", q, w.Code)
		}
	}
	// The duplicate wording, pinned through the route's JSON error
	// transport. The export owner delegates the repeated-key rule to the
	// shared adminjson owner, and this row sees the user-facing wording
	// through the real route.
	dupReq := httptest.NewRequest(http.MethodGet, "/metrics/export", nil)
	dupReq.URL.RawQuery = "provider=a&provider=b"
	dupW := httptest.NewRecorder()
	mux.ServeHTTP(dupW, dupReq)
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(dupW.Body.Bytes(), &body); err != nil || dupW.Code != 400 || body.Error != "duplicate provider" {
		t.Fatalf("repeated provider: status=%d body=%q, want 400 duplicate provider", dupW.Code, dupW.Body.String())
	}
	// The unknown-filter wording, pinned the same way (a freeze row: the
	// wording is already correct). The export owner answers unknown keys in
	// its own filter-grammar class ("unknown filter <name>", not the
	// mutating routes' "unknown key <name>"), and this row is the one that
	// sees a drift between the two wordings. The row's key is "unknown"
	// itself, so the owner's answer reads "unknown filter unknown".
	unkReq := httptest.NewRequest(http.MethodGet, "/metrics/export", nil)
	unkReq.URL.RawQuery = "unknown=x"
	unkW := httptest.NewRecorder()
	mux.ServeHTTP(unkW, unkReq)
	if err := json.Unmarshal(unkW.Body.Bytes(), &body); err != nil || unkW.Code != 400 || body.Error != "unknown filter unknown" {
		t.Fatalf("unknown key: status=%d body=%q, want 400 unknown filter unknown", unkW.Code, unkW.Body.String())
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

// TestExportFilterKeysMatchPurgeFilterTags pins the export query grammar to
// storage.PurgeFilter's JSON tags. Both the JSON count/clear body and the CSV
// export must expose the same predicate; a new filter field that reaches one
// surface only would make the export silently broader or narrower than the
// preview it mirrors. Every tag must parse through exportLogFilter, and the
// parser's closed default branch proves it accepts nothing else.
func TestExportFilterKeysMatchPurgeFilterTags(t *testing.T) {
	typ := reflect.TypeOf(storage.PurgeFilter{})
	fields := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		fields++
		raw := "x"
		switch field.Type.Kind() {
		case reflect.String:
		case reflect.Int, reflect.Int64:
			raw = "1"
		case reflect.Bool:
			raw = "0"
		default:
			t.Fatalf("PurgeFilter field %s has unsupported kind %s; extend the parity fixture", key, field.Type.Kind())
		}
		if _, err := exportLogFilter(url.Values{key: {raw}}); err != nil {
			t.Errorf("export filter rejects PurgeFilter tag %q: %v", key, err)
		}
	}
	if fields == 0 {
		t.Fatal("PurgeFilter declares no JSON tags; the parity walk is blind")
	}
}

// TestPurgeResponseWireKeysStrict pins the /admin/purge response to exactly
// deleted and buffer_removed, the store-level PurgeResult on the wire. A
// re-added or renamed key must redden on this strict decode instead of passing
// unnoticed by the count-focused tests, which never read the response body.
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
			for i, body := range []string{`{"filter":{"provider":"neutral.example"}}`, ""} {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("purge %q = %d %s", body, w.Code, w.Body.String())
				}
				var purge struct {
					Deleted       int64 `json:"deleted"`
					BufferRemoved int   `json:"buffer_removed"`
				}
				dec := json.NewDecoder(w.Body)
				dec.DisallowUnknownFields()
				if err := dec.Decode(&purge); err != nil {
					t.Fatalf("strict decode of the purge response: %v (%s)", err, w.Body.String())
				}
				// The first deletion removes the only row; the second finds
				// nothing left.
				want := int64(0)
				if durable && i == 0 {
					want = 1
				}
				if purge.Deleted != want || purge.BufferRemoved != 1-i {
					t.Fatalf("purge response deleted=%d buffer_removed=%d, want %d and %d: %s",
						purge.Deleted, purge.BufferRemoved, want, 1-i, w.Body.String())
				}
			}
			if b.Len() != 0 {
				t.Fatalf("ring = %d, want both purges to clear it", b.Len())
			}
		})
	}
}

// TestPurgeReviewedCountRefusesAMovedRowSet pins the preview binding: a
// reviewed count that no longer matches the row set refuses the deletion with
// 409 and deletes nothing, and the count is compared inside the delete
// transaction rather than by a separate query. The matching count deletes and
// reports both net counts.
func TestPurgeReviewedCountRefusesAMovedRowSet(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			mux, b, s := logRoutesForTest(t, durable)
			var recorder metrics.Recorder = b
			if s != nil {
				recorder = s.Recorder(b)
			}
			recorder.Record(&metrics.Record{ID: "reviewed-a", Provider: "target", StatusCode: 200, Start: time.Now()})
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			// Preview one row, then let a second matching row land.
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge/count",
				strings.NewReader(`{"filter":{"provider":"target"}}`)))
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"count":1}` {
				t.Fatalf("preview = %d %s", w.Code, w.Body.String())
			}
			recorder.Record(&metrics.Record{ID: "reviewed-b", Provider: "target", StatusCode: 200, Start: time.Now()})
			if s != nil {
				if err := s.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			stale := httptest.NewRecorder()
			mux.ServeHTTP(stale, httptest.NewRequest(http.MethodPost, "/admin/purge",
				strings.NewReader(`{"filter":{"provider":"target"},"reviewed_count":1}`)))
			if stale.Code != http.StatusConflict {
				t.Fatalf("stale reviewed count = %d %s, want 409", stale.Code, stale.Body.String())
			}
			if b.Len() != 2 {
				t.Fatalf("refused purge removed live rows: ring=%d, want 2", b.Len())
			}
			if s != nil {
				if n, err := s.CountWhere(t.Context(), storage.PurgeFilter{}); err != nil || n != 2 {
					t.Fatalf("refused purge removed durable rows: n=%d err=%v", n, err)
				}
			}
			ok := httptest.NewRecorder()
			mux.ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/admin/purge",
				strings.NewReader(`{"filter":{"provider":"target"},"reviewed_count":2}`)))
			if ok.Code != 200 || b.Len() != 0 {
				t.Fatalf("matching reviewed count = %d %s ring=%d, want 200 and an empty ring", ok.Code, ok.Body.String(), b.Len())
			}
			var deleted struct {
				Deleted       int64 `json:"deleted"`
				BufferRemoved int   `json:"buffer_removed"`
			}
			if err := json.Unmarshal(ok.Body.Bytes(), &deleted); err != nil {
				t.Fatalf("decode: %v (%s)", err, ok.Body.String())
			}
			wantDurable := int64(0)
			if durable {
				wantDurable = 2
			}
			if deleted.Deleted != wantDurable || deleted.BufferRemoved != 2 {
				t.Fatalf("deleted=%d buffer_removed=%d, want %d and 2", deleted.Deleted, deleted.BufferRemoved, wantDurable)
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

// TestPurgeWritesOneAuditLinePerOutcome pins the destructive-action audit: a
// purge reports its selection and committed counts on success, its selection
// and error on failure, and both before the response so a lost response is
// still diagnosable (H19). The stale-count refusal logs its own outcome.
func TestPurgeWritesOneAuditLinePerOutcome(t *testing.T) {
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	mux, b, s := logRoutesForTest(t, true)
	recorder := s.Recorder(b)
	recorder.Record(&metrics.Record{ID: "audit-a", Provider: "target", StatusCode: 200, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Stale reviewed count: refused, and the committed population is reported.
	stale := httptest.NewRecorder()
	mux.ServeHTTP(stale, httptest.NewRequest(http.MethodPost, "/admin/purge",
		strings.NewReader(`{"filter":{"provider":"target"},"reviewed_count":9}`)))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale purge = %d %s", stale.Code, stale.Body.String())
	}
	if got := strings.Count(logged.String(), `audit: purge outcome="refused" filter="provider=target" reviewed="9"`); got != 1 {
		t.Fatalf("refusal audit lines = %d in %q", got, logged.String())
	}
	// A matching purge reports the committed counts.
	ok := httptest.NewRecorder()
	mux.ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/admin/purge",
		strings.NewReader(`{"filter":{"provider":"target"},"reviewed_count":1}`)))
	if ok.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", ok.Code, ok.Body.String())
	}
	if !strings.Contains(logged.String(), `audit: purge outcome="ok" filter="provider=target" reviewed="1" deleted="1" buffer_removed="1"`) {
		t.Fatalf("success audit line missing from %q", logged.String())
	}
	// The bodyless full deletion names the selection "all".
	full := httptest.NewRecorder()
	mux.ServeHTTP(full, httptest.NewRequest(http.MethodPost, "/admin/purge", nil))
	if full.Code != http.StatusOK {
		t.Fatalf("full purge = %d %s", full.Code, full.Body.String())
	}
	if !strings.Contains(logged.String(), `audit: purge outcome="ok" filter="all"`) {
		t.Fatalf("full-delete audit line missing from %q", logged.String())
	}
}

// TestPurgeReportsTheCommittedCount pins H19: the wire count is the rows the
// delete transaction committed, not the population the filter scanned or the
// previewed number, and the same count reaches the audit line before the
// response body so a lost response is still diagnosable.
func TestPurgeReportsTheCommittedCount(t *testing.T) {
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	mux, b, s := logRoutesForTest(t, true)
	recorder := s.Recorder(b)
	for _, row := range []struct{ id, provider string }{
		{"committed-a", "alpha"}, {"committed-b", "alpha"}, {"kept-c", "beta"},
	} {
		recorder.Record(&metrics.Record{ID: row.id, Provider: row.provider, StatusCode: 200, Start: time.Now()})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/purge",
		strings.NewReader(`{"filter":{"provider":"alpha"},"reviewed_count":2}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Deleted != 2 {
		t.Fatalf("deleted = %d, want the 2 committed rows", body.Deleted)
	}
	if !strings.Contains(logged.String(), `deleted="2"`) {
		t.Fatalf("audit line does not carry the committed count: %q", logged.String())
	}
	if n, err := s.CountWhere(t.Context(), storage.PurgeFilter{Provider: "beta"}); err != nil || n != 1 {
		t.Fatalf("unmatched row count = %d err=%v, want 1 left", n, err)
	}
}
