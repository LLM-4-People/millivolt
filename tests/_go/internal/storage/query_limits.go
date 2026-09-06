package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestQueryExactByteAndRowBounds(t *testing.T) {
	opts := testOpts
	opts.QueryMaxBytes = config.StorageQueryMaxBytesMin
	opts.QueryMaxRows = 3
	s := openPurgeTestStore(t, opts)
	// [{"v":""}] is ten bytes; parameter content is plain ASCII.
	for _, extra := range []int{0, 1} {
		rows, err := s.Query(t.Context(), "SELECT ? AS v", strings.Repeat("x", opts.QueryMaxBytes-10+extra))
		if extra == 1 {
			if !errors.Is(err, ErrQueryLimit) || rows != nil {
				t.Fatalf("oversized result=%v err=%v", rows, err)
			}
			continue
		}
		data, marshalErr := json.Marshal(rows)
		if err != nil || marshalErr != nil || len(data) != opts.QueryMaxBytes {
			t.Fatalf("exact boundary bytes=%d err=%v/%v", len(data), err, marshalErr)
		}
	}
	for _, n := range []int{3, 4} {
		q := fmt.Sprintf("SELECT n FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<%d) SELECT n FROM c)", n)
		rows, err := s.Query(t.Context(), q)
		if n == 3 && (err != nil || len(rows) != n) {
			t.Fatalf("exact row boundary=%d err=%v", len(rows), err)
		}
		if n == 4 && (!errors.Is(err, ErrQueryLimit) || rows != nil) {
			t.Fatalf("row overflow returned prefix=%v err=%v", rows, err)
		}
	}
	rows, err := s.Query(t.Context(), "SELECT 1 WHERE 0")
	data, _ := json.Marshal(rows)
	if err != nil || string(data) != "[]" {
		t.Fatalf("empty result=%s err=%v", data, err)
	}
}

func TestQueryEngineBoundsAndHTTPFailure(t *testing.T) {
	opts := testOpts
	opts.QueryMaxBytes = config.StorageQueryMaxBytesMin
	opts.QueryMaxRows = 1
	s := openPurgeTestStore(t, opts)
	columns := make([]string, queryMaxColumns+1)
	for i := range columns {
		columns[i] = fmt.Sprintf("1 AS c%d", i)
	}
	for _, q := range []string{
		fmt.Sprintf("SELECT zeroblob(%d) AS v", opts.QueryMaxBytes+1),
		fmt.Sprintf("SELECT zeroblob(%d) AS v", opts.QueryMaxBytes), // base64 expansion exceeds JSON budget
		"SELECT 1 AS v UNION ALL SELECT 2",
		"SELECT " + strings.Join(columns, ","),
	} {
		rows, err := s.Query(t.Context(), q)
		if rows != nil || !errors.Is(err, ErrQueryLimit) {
			t.Fatalf("q=%q rows=%v err=%v", q, rows, err)
		}
		w := httptest.NewRecorder()
		s.HandleQuery(w, httptest.NewRequest(http.MethodGet, "/metrics/query?q="+url.QueryEscape(q), nil))
		if w.Code != http.StatusRequestEntityTooLarge || !json.Valid(w.Body.Bytes()) || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status=%d type=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] == nil {
			t.Fatalf("partial/non-error body=%s", w.Body.String())
		}
	}
	if rows, err := s.Query(t.Context(), "SELECT "+strings.Join(columns[:queryMaxColumns], ",")); err != nil || len(rows[0]) != queryMaxColumns {
		t.Fatalf("column boundary rows=%v err=%v", rows, err)
	}
	if _, err := s.Query(t.Context(), "SELECT FROM"); err == nil || errors.Is(err, ErrQueryLimit) {
		t.Fatalf("syntax error misclassified: %v", err)
	}
}

func TestQueryRestoresConnectionLimits(t *testing.T) {
	opts := testOpts
	opts.QueryMaxBytes = config.StorageQueryMaxBytesMin
	opts.QueryMaxRows = 2
	s := openPurgeTestStore(t, opts)
	s.rdb.SetMaxOpenConns(1)
	readLimits := func() [2]int {
		t.Helper()
		c, err := s.rdb.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var out [2]int
		for i, id := range []int{sqlite3.SQLITE_LIMIT_LENGTH, sqlite3.SQLITE_LIMIT_COLUMN} {
			out[i], err = sqlite.Limit(c, id, -1)
			if err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	want := readLimits()
	for _, q := range []string{"SELECT 1", "SELECT FROM", fmt.Sprintf("SELECT zeroblob(%d)", opts.QueryMaxBytes+1), "SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3"} {
		_, _ = s.Query(t.Context(), q)
		if got := readLimits(); got != want {
			t.Fatalf("q=%q limits=%v want=%v", q, got, want)
		}
	}
	// Query-only limits must not alter internal read-pool authority.
	var blob []byte
	if err := s.rdb.QueryRowContext(t.Context(), "SELECT zeroblob(?)", opts.QueryMaxBytes+1).Scan(&blob); err != nil || len(blob) != opts.QueryMaxBytes+1 {
		t.Fatalf("internal read still limited: %d,%v", len(blob), err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if rows, err := s.Query(canceled, "SELECT 1"); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("canceled=%v,%v", rows, err)
	}
	running, stop := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stop()
	if rows, err := s.Query(running, "SELECT sum(n) FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<1000000000) SELECT n FROM c)"); !errors.Is(err, context.DeadlineExceeded) || rows != nil {
		t.Fatalf("running query cancellation=%v,%v", rows, err)
	}
	if got := readLimits(); got != want {
		t.Fatalf("running cancellation limits=%v want=%v", got, want)
	}
	s.opts.QueryTimeout = time.Nanosecond
	if rows, err := s.Query(t.Context(), "SELECT 1"); !errors.Is(err, context.DeadlineExceeded) || rows != nil {
		t.Fatalf("owner timeout=%v,%v", rows, err)
	}
	if got := readLimits(); got != want {
		t.Fatalf("cancellation limits=%v want=%v", got, want)
	}
}

func TestQueryMissingLimitsFailClosed(t *testing.T) {
	for _, missing := range []string{"bytes", "rows"} {
		opts := testOpts
		if missing == "bytes" {
			opts.QueryMaxBytes = 0
		} else {
			opts.QueryMaxRows = 0
		}
		s := openPurgeTestStore(t, opts)
		if rows, err := s.Query(t.Context(), "SELECT 1"); rows != nil || !errors.Is(err, ErrQueryLimit) {
			t.Fatalf("missing %s silently defaulted: %v,%v", missing, rows, err)
		}
	}
}
