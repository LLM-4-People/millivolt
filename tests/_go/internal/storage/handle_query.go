package storage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHandleQueryRejectsNonSelect(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reject := []string{
		"PRAGMA table_info(requests)",
		"DROP TABLE requests",
		"  delete from requests",
		"INSERT INTO requests (id) VALUES ('x')",
		"SELECT * FROM requests; DROP TABLE requests", // multi-statement
		"WITH x AS (SELECT 1) SELECT * FROM x",        // not SELECT-leading
		"UPDATE requests SET model='x'",
	}
	for _, q := range reject {
		req := httptest.NewRequest("GET", "/metrics/query?q="+url.QueryEscape(q), nil)
		w := httptest.NewRecorder()
		s.HandleQuery(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("q=%q: status = %d, want 400", q, w.Code)
		}
	}

	// A legit SELECT works and returns JSON.
	req := httptest.NewRequest("GET", "/metrics/query?q="+url.QueryEscape("SELECT COUNT(*) AS n FROM requests"), nil)
	w := httptest.NewRecorder()
	s.HandleQuery(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("valid SELECT: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	// No permissive CORS header on the query endpoint.
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want unset", got)
	}

	post := httptest.NewRequest(http.MethodPost, "/metrics/query?q="+url.QueryEscape("SELECT 1"), nil)
	pw := httptest.NewRecorder()
	s.HandleQuery(pw, post)
	if pw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST query: status = %d, want 405", pw.Code)
	}
	if got := pw.Header().Get("Allow"); got != "GET" {
		t.Errorf("POST query Allow = %q, want GET", got)
	}
}

// TestHandleQueryStrictQuery pins the route's single consumed key under the
// strict-query rule: a malformed pair the lenient read would drop answers
// the invalid-query 400 (not the missing-q wording a dropped pair self-heals
// into), and a repeated q is denied, never first-wins.
func TestHandleQueryStrictQuery(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	errorText := func(w *httptest.ResponseRecorder) string {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not the flat error shape: %v (%q)", err, w.Body.String())
		}
		return body.Error
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics/query", nil)
	req.URL.RawQuery = "q=%zz"
	w := httptest.NewRecorder()
	s.HandleQuery(w, req)
	if w.Code != http.StatusBadRequest || errorText(w) != "invalid query" {
		t.Fatalf("malformed q: status=%d error=%q, want 400 invalid query", w.Code, errorText(w))
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics/query", nil)
	req.URL.RawQuery = "q=" + url.QueryEscape("SELECT 1") + "&q=" + url.QueryEscape("SELECT 2")
	w = httptest.NewRecorder()
	s.HandleQuery(w, req)
	if w.Code != http.StatusBadRequest || errorText(w) != "duplicate q" {
		t.Fatalf("repeated q: status=%d error=%q, want 400 duplicate q", w.Code, errorText(w))
	}
}

// TestReadPoolIsReadOnly pins the defense-in-depth invariant: the dashboard
// read pool must reject writes at the DB layer (mode=ro), so a mutation that
// somehow passed validateQuery still fails. Regression test for the modernc
// driver ignoring "path?mode=ro" without the "file:" URI scheme.
func TestReadPoolIsReadOnly(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.rdb.Exec("DELETE FROM requests"); err == nil {
		t.Error("read pool allowed a DELETE; want mode=ro to reject writes")
	}
}

// TestValidateQueryDeniesAttach pins the fail-closed choke point: ATTACH (which
// SQLite honors even on a mode=ro connection, opening the attached DB writable)
// and other side-effecting keywords are rejected at the query boundary.
func TestValidateQueryDeniesAttach(t *testing.T) {
	for _, q := range []string{
		"SELECT 1; ATTACH DATABASE '/tmp/x.db' AS aux",
		"SELECT * FROM requests WHERE id='x' ATTACH DATABASE '/tmp/x.db' AS aux",
		"SELECT load_extension('/tmp/evil')",
		"SELECT * FROM requests; DROP TABLE requests",
		"PRAGMA writable_schema=ON",
		"ATTACH DATABASE '/tmp/x.db' AS aux",
	} {
		if err := validateQuery(q); err == nil {
			t.Errorf("validateQuery(%q) = nil, want rejection", q)
		}
	}
	// A legitimate SELECT still passes; a column named "attachment" is not a
	// false positive for the ATTACH keyword, and the underscore freeze rows
	// pin the word-continuation boundary the containsWord doc states: '_'
	// continues a word, so "attach_x" and "x_attach" pass the same
	// whole-word grammar.
	for _, q := range []string{
		"SELECT provider, COUNT(*) FROM requests GROUP BY provider",
		"SELECT attachment FROM requests",
		"SELECT attach_x FROM requests",
		"SELECT x_attach FROM requests",
	} {
		if err := validateQuery(q); err != nil {
			t.Errorf("validateQuery(%q) = %v, want nil", q, err)
		}
	}
}
