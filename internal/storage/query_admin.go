package storage

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

// errNotSelect rejects any dashboard query that is not a single SELECT.
var errNotSelect = errors.New("only a single SELECT statement is allowed")

// ErrQueryLimit rejects the entire result; callers must never serve a prefix.
var ErrQueryLimit = errors.New("query result exceeds its byte, row, or column limit")

// Ad-hoc SELECTs cannot materialize thousands of independent large columns
// before the result-byte check. Internal safety guardrail, not user-tunable;
// the canonical requests schema (including SELECT *) fits below this ceiling.
const queryMaxColumns = 128

// queryRenderHook is a test seam (production leaves it nil). It reports a
// completed ad-hoc rendering: queryRenderRows once Query materializes its row
// slice, queryRenderJSON once the endpoint assembles the full JSON result. The
// endpoint regression test requires exactly one JSON rendering and no rows
// rendering, which fails if the handler re-marshals a Query row slice.
var queryRenderHook func(kind string)

const (
	queryRenderRows = "rows"
	queryRenderJSON = "json"
)

// Query owns ad-hoc SELECT execution time and result resource bounds. The
// SELECT-only HTTP restriction stays in HandleQuery. SQLite working memory
// (sorting/aggregate intermediates) is not a strict per-query heap budget.
// The shared scan already encodes each row for the byte cap; this path keeps
// only the decoded rows because it has no response body to build.
func (s *Store) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	out := make([]map[string]any, 0)
	err := s.queryScan(ctx, query, nil, func(row map[string]any) {
		out = append(out, row)
	}, args...)
	if err != nil {
		return nil, err
	}
	if queryRenderHook != nil {
		queryRenderHook(queryRenderRows)
	}
	return out, nil
}

// queryJSON executes the same bounded SELECT as Query and returns the complete
// JSON array assembled from the per-row encodings the byte cap already
// requires. This is the endpoint's one full-result encode: HandleQuery writes
// these bytes directly instead of re-marshaling the rows. The array is only
// returned after every row passed the caps, so a limit failure never leaves a
// partial body.
func (s *Store) queryJSON(ctx context.Context, query string, args ...any) ([]byte, error) {
	out := make([]byte, 0, 2)
	out = append(out, '[')
	if err := s.queryScan(ctx, query, &out, nil, args...); err != nil {
		return nil, err
	}
	out = append(out, ']')
	if queryRenderHook != nil {
		queryRenderHook(queryRenderJSON)
	}
	return out, nil
}

// queryScan is the one ad-hoc SELECT execution owner shared by Query and the
// HTTP endpoint. It applies the query-only SQLite limits, scans rows, enforces
// QueryMaxRows and QueryMaxBytes against the same per-row json.Marshal length,
// and routes each row to its caller's rendering: keepRow collects the decoded
// map, jsonOut collects the JSON array, and each caller supplies exactly one.
// Engine-limit failures are mapped to ErrQueryLimit here, before either
// rendering can expose a prefix.
func (s *Store) queryScan(ctx context.Context, query string, jsonOut *[]byte, keepRow func(map[string]any), args ...any) (err error) {
	defer func() {
		if err != nil && queryEngineLimit(err) {
			err = fmt.Errorf("%w: %v", ErrQueryLimit, err)
		}
	}()
	if s.opts.QueryMaxBytes < 2 || s.opts.QueryMaxRows < 1 {
		return ErrQueryLimit
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	conn, err := s.rdb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	type limit struct{ id, value int }
	var previous []limit
	defer func() {
		var restoreErr error
		for _, old := range previous {
			if _, e := sqlite.Limit(conn, old.id, old.value); e != nil {
				restoreErr = errors.Join(restoreErr, e)
			}
		}
		if restoreErr != nil {
			// Never return a connection with query-only limits to the shared
			// pool: internal history readers must keep their normal authority.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("restore query limits: %w", restoreErr))
		}
	}()
	for _, bound := range [...]limit{{sqlite3.SQLITE_LIMIT_LENGTH, s.opts.QueryMaxBytes}, {sqlite3.SQLITE_LIMIT_COLUMN, queryMaxColumns}} {
		old, e := sqlite.Limit(conn, bound.id, bound.value)
		if e != nil {
			return e
		}
		previous = append(previous, limit{bound.id, old})
	}
	// modernc steps the first row inside QueryContext; limits must already
	// be installed here, not merely before Rows.Next/Scan.
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	// Keys, their order and the duplicate-name collapse are decided once per
	// query; the per-row cost is one leaf encode per column and nothing else.
	// One reusable value/pointer pair and one reusable row buffer replace the
	// per-row map, its key sort and its marshal.
	proj, err := newQueryProjection(cols)
	if err != nil {
		return err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	var row jsonSink
	rowEnc := json.NewEncoder(&row)

	resultBytes := 2 // JSON array brackets, including the empty-array case
	rowCount := 0
	for rows.Next() {
		if rowCount == s.opts.QueryMaxRows {
			return ErrQueryLimit
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row.buf = row.buf[:0]
		if err := proj.encodeRow(&row, rowEnc, values); err != nil {
			return err
		}
		separator := 0
		if rowCount > 0 {
			separator = 1
		}
		// The cap counts the encoded row, never a map slice: this is the same
		// length json.Marshal of the row map produces, so the boundary is
		// unchanged.
		if len(row.buf) > s.opts.QueryMaxBytes-resultBytes-separator {
			return ErrQueryLimit
		}
		resultBytes += len(row.buf) + separator
		rowCount++
		if jsonOut != nil {
			if separator == 1 {
				*jsonOut = append(*jsonOut, ',')
			}
			*jsonOut = append(*jsonOut, row.buf...)
		}
		if keepRow != nil {
			decoded := make(map[string]any, len(cols))
			for i, c := range cols {
				decoded[c] = values[i]
			}
			keepRow(decoded)
		}
	}
	return rows.Err()
}

func queryEngineLimit(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	if sqliteErr.Code()&0xff == sqlite3.SQLITE_TOOBIG {
		return true
	}
	// SQLite reports the COLUMN limit as generic SQLITE_ERROR, not TOOBIG.
	// Match only the engine's documented-limit diagnostics, not syntax errors.
	if sqliteErr.Code() == sqlite3.SQLITE_ERROR {
		for _, message := range [...]string{"too many columns in result set", "too many terms in ORDER BY clause", "too many terms in GROUP BY clause"} {
			if strings.Contains(sqliteErr.Error(), message) {
				return true
			}
		}
	}
	return false
}

// validateQuery enforces the dashboard query contract: a single SELECT
// statement, nothing else. The read pool is opened mode=ro as a second layer
// of defense - but mode=ro only pins the MAIN database read-only: SQLite still
// honors ATTACH on a read-only connection and opens the attached DB read-write.
// So ATTACH (and other schema/mutation keywords that can ride a SELECT-leading
// string) are denied here explicitly - the real choke point the driver can't
// provide (modernc.org/sqlite exposes no authorizer hook).
func validateQuery(q string) error {
	t := strings.TrimSpace(q)
	if t == "" {
		return errNotSelect
	}
	if !strings.HasPrefix(strings.ToUpper(t), "SELECT") {
		return errNotSelect
	}
	// Single statement only: reject any statement terminator.
	if strings.Contains(t, ";") {
		return errNotSelect
	}
	// Deny side-effecting keywords even inside a SELECT-leading string (e.g.
	// "SELECT … ATTACH …", "SELECT load_extension(...)"). Word-boundary match
	// at the ASCII boundary: ASCII letters, digits and underscores continue a
	// word, so a superstring spelling such as a column named "attachment" or
	// "attach_x" passes, while a column literally named "attach" is denied
	// with the keyword - the accepted cost of denying the bare word
	// everywhere. A byte outside ASCII ends a word even when it spells a
	// letter or digit, so an attaché-class spelling trips the keyword
	// (containsWord states the measured boundary).
	up := strings.ToUpper(t)
	for _, kw := range []string{"ATTACH", "DETACH", "PRAGMA", "LOAD_EXTENSION"} {
		if containsWord(up, kw) {
			return errNotSelect
		}
	}
	return nil
}

// containsWord reports whether s contains the keyword kw surrounded by
// non-word characters. The boundary is ASCII: ASCII letters, digits and
// underscores continue a word (isAlphaNum), so "ATTACH" is not found in
// "attachment", "attach_x" or "x_attach"; a byte outside ASCII ends a word
// even when it spells a letter or digit, so an attaché-class spelling trips
// the keyword - measured after ToUpper, "attaché", "éattach", "attachß",
// "attachﬁ" and an Arabic-Indic digit joined to the keyword are all denied,
// while "attachſ" passes only because its upper-case is the ASCII "S".
func containsWord(s, kw string) bool {
	for i := 0; ; {
		idx := strings.Index(s[i:], kw)
		if idx < 0 {
			return false
		}
		j := i + idx
		before := j == 0 || !isAlphaNum(s[j-1])
		after := j+len(kw) >= len(s) || !isAlphaNum(s[j+len(kw)])
		if before && after {
			return true
		}
		i = j + len(kw)
	}
}

func isAlphaNum(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// HandleQuery serves a JSON array of rows for a SELECT query passed as
// ?q=... (URL-decoded). Enforces SELECT-only, single-statement, and a bounded
// execution time. A nil store returns 503, never inference fallback.
// Same-origin dashboard endpoint; no CORS header.
func (s *Store) HandleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, message string) {
		adminjson.WriteErrorJSON(w, status, message)
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s == nil {
		fail(http.StatusServiceUnavailable, ErrStorageDisabled.Error())
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		fail(http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "q"); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if q.Get("q") == "" {
		fail(http.StatusBadRequest, "missing q")
		return
	}
	query := q.Get("q")
	if err := validateQuery(query); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.queryJSON(r.Context(), query)
	if err != nil {
		// The verbatim body serves the operator's browser (operator-gated
		// path); the server-side log is the durable record of the cause.
		log.Printf("storage: query failed: %v", err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrQueryLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		fail(status, err.Error())
		return
	}
	w.Write(data)
}
