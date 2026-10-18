package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
)

// serveQuery runs one statement through the operator endpoint and returns the
// recorded response.
func serveQuery(t *testing.T, s *Store, statement string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.HandleQuery(w, httptest.NewRequest(http.MethodGet, "/metrics/query?q="+url.QueryEscape(statement), nil))
	return w
}

// TestHandleQueryJSONMatchesStoreQuery pins the endpoint's one-encode output to
// json.Marshal of the rows Store.Query returns for the same statement. Both
// renderings must agree byte for byte: map key order, HTML and Unicode
// escaping, scalar and blob representation, row order and the empty array.
func TestHandleQueryJSONMatchesStoreQuery(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	for _, tc := range []struct {
		name      string
		statement string
	}{
		{name: "empty", statement: "SELECT 1 AS n WHERE 0"},
		{
			name: "scalars and escaping",
			statement: `SELECT NULL AS n, 1 AS i, -2 AS neg, 9223372036854775807 AS max_i,
				1.5 AS f, 0.000001 AS small, '' AS empty,
				'plain' AS s, '<&>' AS html, 'café 漢字' AS uni,
				'a"b\c' AS quote, '],[' AS separators,
				'line' || char(8232) || 'sep' || char(8233) AS line_separators`,
		},
		{name: "blob", statement: `SELECT x'00ff' AS b`},
		{name: "invalid utf8 text", statement: `SELECT CAST(x'80' AS TEXT) AS bad`},
		{
			name: "multiple rows and columns",
			statement: `SELECT n AS z, printf('row-%d', n) AS a, n * 1.5 AS m
				FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<3) SELECT n FROM c)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.Query(t.Context(), tc.statement)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			want, err := json.Marshal(rows)
			if err != nil {
				t.Fatalf("marshal rows: %v", err)
			}
			w := serveQuery(t, s, tc.statement)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			if !bytes.Equal(w.Body.Bytes(), want) {
				t.Fatalf("endpoint body=%s, want %s", w.Body.String(), want)
			}
		})
	}
}

// TestHandleQueryJSONBoundsMatchQuery pins the exact cap boundary on the
// endpoint: a result at QueryMaxBytes/QueryMaxRows is served byte-identically
// to json.Marshal(rows), while one byte or one row over is the clean 413 with
// only the operator error body (never a result prefix).
func TestHandleQueryJSONBoundsMatchQuery(t *testing.T) {
	opts := testOpts
	opts.QueryMaxBytes = config.StorageQueryMaxBytesMin
	opts.QueryMaxRows = 3
	s := openPurgeTestStore(t, opts)
	wantLimitBody := string(adminjson.ErrorBody(ErrQueryLimit.Error())) + "\n"

	// [{"v":""}] is ten bytes; plain ASCII keeps the padding byte-exact.
	exactBytes := "SELECT ? AS v"
	padding := strings.Repeat("x", opts.QueryMaxBytes-10)
	rows, err := s.Query(t.Context(), exactBytes, padding)
	if err != nil {
		t.Fatalf("exact byte cap: %v", err)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal exact bytes: %v", err)
	}
	if len(want) != opts.QueryMaxBytes {
		t.Fatalf("exact byte result=%d, want %d", len(want), opts.QueryMaxBytes)
	}
	w := serveQuery(t, s, fmt.Sprintf("SELECT '%s' AS v", padding))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("exact byte cap: status=%d body=%s want=%s", w.Code, w.Body.String(), want)
	}

	overBytes := fmt.Sprintf("SELECT '%s' AS v", padding+"x")
	w = serveQuery(t, s, overBytes)
	if w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != wantLimitBody {
		t.Fatalf("one byte over: status=%d body=%q, want 413 %q", w.Code, w.Body.String(), wantLimitBody)
	}

	rowsStatement := func(n int) string {
		return fmt.Sprintf("SELECT n FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<%d) SELECT n FROM c)", n)
	}
	rows, err = s.Query(t.Context(), rowsStatement(opts.QueryMaxRows))
	if err != nil {
		t.Fatalf("exact row cap: %v", err)
	}
	want, err = json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal exact rows: %v", err)
	}
	w = serveQuery(t, s, rowsStatement(opts.QueryMaxRows))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("exact row cap: status=%d body=%s want=%s", w.Code, w.Body.String(), want)
	}

	w = serveQuery(t, s, rowsStatement(opts.QueryMaxRows+1))
	if w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != wantLimitBody {
		t.Fatalf("one row over: status=%d body=%q, want 413 %q", w.Code, w.Body.String(), wantLimitBody)
	}
}

// TestHandleQueryEncodesResultOnce proves the endpoint's full-result encoding
// happens during the bounded scan, not again in the handler. A handler that
// re-marshals a Store.Query row slice reports a rows rendering (and no JSON
// rendering), which this test rejects.
func TestHandleQueryEncodesResultOnce(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	var rendered []string
	previous := queryRenderHook
	queryRenderHook = func(kind string) { rendered = append(rendered, kind) }
	t.Cleanup(func() { queryRenderHook = previous })

	statement := `SELECT 1 AS n UNION ALL SELECT 2`
	rows, err := s.Query(t.Context(), statement)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	rendered = nil

	w := serveQuery(t, s, statement)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(rendered) != 1 || rendered[0] != queryRenderJSON {
		t.Fatalf("endpoint renderings=%v, want exactly [%s]", rendered, queryRenderJSON)
	}
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("endpoint body=%s, want %s", w.Body.String(), want)
	}
}
