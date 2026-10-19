package mcp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// rows is one SQL result set: the proxy returns a flat object per row, keyed by
// column name, with JSON scalars (a number may arrive as float64, int64, or
// string depending on the column's declared type).
type rows []map[string]any

// querySQL runs one bounded SELECT through the proxy's read-only SQL reader and
// materializes every row. It serves the paths that need the whole result (the
// describe schema probe, a capture page, the values fold); the query tool
// streams through queryRows instead.
func (c *Client) querySQL(ctx context.Context, statement string) (rows, error) {
	values, err := queryValues(statement)
	if err != nil {
		return nil, err
	}
	var out rows
	if err := c.getJSON(ctx, routeQuery, values, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// queryValues is the one owner of a SQL statement leaving this server: the
// local pre-check plus the q parameter every SQL reader sends. The proxy
// remains the authority on what runs.
func queryValues(statement string) (url.Values, error) {
	if err := validateSelect(statement); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Set("q", statement)
	return values, nil
}

// queryPage is one streamed SELECT result. Rows holds only the prefix this
// server keeps; Total is every element the proxy produced, including rows
// decoded only to be counted.
type queryPage struct {
	Rows          []map[string]any
	Bytes         int
	Total         int
	ByteTruncated bool
}

// queryRows runs one bounded SELECT and decodes the proxy's bare JSON array
// element by element, so the full result is never materialized. It owns the
// row cap and the encoded-size cap that used to run over a decoded slice:
//
//   - Rows keeps the source-order prefix inside rowLimit while the exact
//     json.Marshal length of each row fits byteBudget.
//   - Bytes is the sum of those encoded lengths for the kept rows.
//   - Total counts every element, including the rows past either cap.
//   - ByteTruncated reports that byteBudget withheld a row inside rowLimit, so
//     the caller reports the byte clamp instead of the row clamp.
//
// The whole response body still has to be read to count Total, but it is not
// retained, and a row past the caps is decoded into a throwaway value rather
// than a map. The proxy's own bare-array response is unchanged.
func (c *Client) queryRows(ctx context.Context, statement string, rowLimit, byteBudget int) (queryPage, error) {
	values, err := queryValues(statement)
	if err != nil {
		return queryPage{}, err
	}
	// openRaw owns the failure-body bound and the *APIError shape, so an
	// oversized failure is the read-budget error rather than a partial proxy
	// message exactly as the buffered path reports it.
	resp, err := c.openRaw(ctx, http.MethodGet, routeQuery, values)
	if err != nil {
		return queryPage{}, err
	}
	defer resp.Body.Close()
	return decodeQueryRows(resp.StatusCode, resp.Body, rowLimit, byteBudget)
}

// decodeQueryRows is the one streaming decoder for the query route. It keeps
// the JSON decode error shape the buffered path used, after deciding the read
// budget first: readBody observed the whole body before unmarshaling, so a
// body over budget is a read error even when its JSON is also malformed.
func decodeQueryRows(status int, body io.Reader, rowLimit, byteBudget int) (queryPage, error) {
	page := queryPage{Rows: []map[string]any{}}
	bounded := newBoundedBody(body)
	decoder := json.NewDecoder(bounded)
	fail := func(err error) (queryPage, error) {
		failure := bounded.failure
		drainErr := bounded.drain()
		if failure != nil {
			return queryPage{}, readFailure(failure)
		}
		if drainErr != nil {
			return queryPage{}, readFailure(drainErr)
		}
		if bounded.overflowed() {
			return queryPage{}, readBudgetExceeded()
		}
		return queryPage{}, fmt.Errorf("millivolt returned HTTP %d with a body this tool could not decode as JSON (%d bytes): %v",
			status, bounded.consumed(), err)
	}

	token, err := decoder.Token()
	if err != nil {
		return fail(err)
	}
	array := false
	switch value := token.(type) {
	case json.Delim:
		if value != '[' {
			return fail(errors.New("the result is not a JSON array"))
		}
		array = true
	case nil:
		// The buffered decode accepted a top-level null as an empty result;
		// keep that acceptance instead of turning it into a new failure.
	default:
		return fail(errors.New("the result is not a JSON array"))
	}
	if array {
		for decoder.More() {
			page.Total++
			// Past either cap a row still has to be validated and counted, but
			// it must not become a map. Decoding into an empty struct skips
			// every field and keeps the same object/null shape gate the map
			// target had.
			if page.Total > rowLimit || page.ByteTruncated {
				var skipped struct{}
				if err := decoder.Decode(&skipped); err != nil {
					return fail(err)
				}
				continue
			}
			var row map[string]any
			if err := decoder.Decode(&row); err != nil {
				return fail(err)
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				// A row the encoder cannot produce is not measurable, so it
				// cannot be budgeted; keep it rather than silently dropping
				// data, the materializing clamp's own rule.
				page.Rows = append(page.Rows, row)
				continue
			}
			if page.Bytes+len(encoded) > byteBudget {
				// Whole rows only, and the first row that does not fit ends
				// the kept prefix exactly as the old byte clamp did.
				page.ByteTruncated = true
				continue
			}
			page.Bytes += len(encoded)
			page.Rows = append(page.Rows, row)
		}
		if _, err := decoder.Token(); err != nil {
			return fail(err)
		}
	}
	// json.Unmarshal rejected data after the array; keep that gate. Decoding
	// into an empty struct again avoids materializing a trailing document.
	var trailing struct{}
	switch err := decoder.Decode(&trailing); {
	case err == io.EOF:
	case err == nil:
		return fail(errors.New("unexpected data after the result array"))
	default:
		return fail(err)
	}
	failure := bounded.failure
	if err := bounded.drain(); err != nil {
		return queryPage{}, readFailure(err)
	}
	if failure != nil {
		return queryPage{}, readFailure(failure)
	}
	if bounded.overflowed() {
		return queryPage{}, readBudgetExceeded()
	}
	return page, nil
}

// validateSelect mirrors the proxy's own server-side gate (SELECT-prefixed, no
// statement terminator) so an obvious mistake is a clear message here instead
// of a round trip. The proxy remains the authority, including its keyword
// denials; this is a fast local pre-check, never a second boundary.
func validateSelect(statement string) error {
	trimmed := strings.TrimSpace(statement)
	switch {
	case trimmed == "":
		return fmt.Errorf("query is required")
	case !strings.HasPrefix(strings.ToUpper(trimmed), "SELECT"):
		return fmt.Errorf("only a single SELECT statement is allowed; the proxy rejects anything else")
	case strings.Contains(trimmed, ";"):
		return fmt.Errorf("only a single SELECT statement is allowed; remove the ';'")
	}
	return nil
}

// schemaStatement is the live schema probe: every table, index and trigger as
// the running database holds it. sqlite_schema is one of the tables the read
// pool exposes.
const schemaStatement = `SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY type, name`

// rowString reads one column from a SQL result row, tolerating the mixed
// scalar types the JSON reader can produce for a SQLite column.
func rowString(row map[string]any, column string) string {
	switch value := row[column].(type) {
	case string:
		return value
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(value, 10)
	case bool:
		return strconv.FormatBool(value)
	default:
		return fmt.Sprint(value)
	}
}

// rowInt64 reads one integer column from a SQL result row, tolerating the mixed
// scalar types the JSON reader can produce for a SQLite column.
func rowInt64(row map[string]any, column string) int64 {
	switch value := row[column].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// debugStatus is GET /admin/debug: the capture session state plus the
// known client/provider/model vocabularies a capture scope accepts.
type debugStatus struct {
	OK             bool           `json:"ok"`
	Enabled        bool           `json:"enabled"`
	Sessions       []debugSession `json:"sessions"`
	KnownClients   []string       `json:"known_clients"`
	KnownProviders []string       `json:"known_providers"`
	KnownModels    []string       `json:"known_models"`
	Until          *string        `json:"until"`
	TTL            string         `json:"ttl"`
	MaxBytes       string         `json:"max_bytes"`
	// Warning is the proxy's persistence warning: the state was applied in
	// memory but could not be saved for restart.
	Warning string `json:"warning,omitempty"`
}

type debugSession struct {
	ID        string   `json:"id"`
	Clients   []string `json:"clients"`
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
	Duration  string   `json:"duration"`
	Until     *string  `json:"until"`
	RanMs     int64    `json:"ran_ms"`
	// Captures is a live count of documents stored for this session.
	Captures int64 `json:"captures"`
}

// storageState is the durable-storage signal read from the bootstrap payload.
// Capture requires it: with db_path empty the proxy accepts a session, reports
// success, and never stores a document.
type storageState struct {
	Enabled        bool   `json:"enabled"`
	Dropped        uint64 `json:"dropped"`
	TotalsDegraded bool   `json:"totals_degraded"`
}

// bootstrapSnapshot is the subset of /metrics/bootstrap these tools read. It is
// deliberately partial: the full payload carries every recent record, which is
// what makes it too large to hand a model unfiltered.
type bootstrapSnapshot struct {
	Seq      int64  `json:"seq"`
	Feed     string `json:"feed_id"`
	Counters struct {
		InFlight int64 `json:"in_flight"`
		TotalReq int64 `json:"total_requests"`
		TotalErr int64 `json:"total_errors"`
	} `json:"counters"`
	Storage    storageState     `json:"storage"`
	Records    []map[string]any `json:"records"`
	ModelCanon *ModelCanonMap   `json:"model_canon,omitempty"`
}

// bootstrap reads the live snapshot for the storage signal and the headline
// counters.
func (c *Client) bootstrap(ctx context.Context) (*bootstrapSnapshot, error) {
	return c.bootstrapFields(ctx, "")
}

// bootstrapFields asks for one of the proxy's closed bootstrap views. storage
// and model_canon avoid decoding the dashboard's records/KPI/state document.
func (c *Client) bootstrapFields(ctx context.Context, fields string) (*bootstrapSnapshot, error) {
	var query url.Values
	if fields != "" {
		query = url.Values{"fields": {fields}}
	}
	var snapshot bootstrapSnapshot
	if err := c.getJSON(ctx, routeBootstrap, query, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// modelRules fetches the proxy's effective model-canonicalization rules from
// the full bootstrap form and compiles them with config.CompileModelRules, the
// proxy's own semantic owner. An absent rules block is the identity pipeline,
// which is what the proxy itself folds an empty rule list with.
func (c *Client) modelRules(ctx context.Context) ([]config.ModelRuleExec, error) {
	snapshot, err := c.bootstrapFields(ctx, "model_canon")
	if err != nil {
		return nil, err
	}
	if snapshot.ModelCanon == nil {
		return nil, nil
	}
	return config.CompileModelRules(snapshot.ModelCanon.Rules), nil
}

// debug reads the capture session state.
func (c *Client) debug(ctx context.Context) (*debugStatus, error) {
	var status debugStatus
	if err := c.getJSON(ctx, routeDebug, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// throttleLimits is one provider's current counts. Only the two dimensions a
// window can apply to are read here.
type throttleLimits struct {
	requests int64
	tokens   int64
}

// throttleLimit reads the provider's current counts from GET /admin/throttle,
// which is the same document the dashboard renders. A provider with no entry
// has no policy, which is zero counts.
func (c *Client) throttleLimit(ctx context.Context, provider string) (throttleLimits, error) {
	var document struct {
		Throttles []struct {
			Provider string `json:"provider"`
			Requests int64  `json:"requests"`
			Tokens   int64  `json:"tokens"`
		} `json:"throttles"`
	}
	if err := c.getJSON(ctx, routeThrottle, nil, &document); err != nil {
		return throttleLimits{}, err
	}
	for _, entry := range document.Throttles {
		if entry.Provider == provider {
			return throttleLimits{requests: entry.Requests, tokens: entry.Tokens}, nil
		}
	}
	return throttleLimits{}, nil
}

// captureRecords lists captured request records, newest first. The proxy has no
// listing endpoint for captures, so the record ids come from a SQL probe over
// the requests table: `debug = 1` marks a captured request,
// `debug_session_id` names the session, and the request_debug sidecar must
// still exist and not have expired. A NULL before is the same paired keyset
// cursor the durable log page uses; a non-NULL before is sent even when its
// record id is empty, because that empty id is a real boundary key.
func (c *Client) captureRecords(ctx context.Context, sessionID string, before *pageCursor, limit int) (rows, error) {
	statement := captureListingStatement(sessionID, before, limit, time.Now().UnixMilli())
	return c.querySQL(ctx, statement)
}

// captureListingStatement is the one owner of the capture listing SQL. It
// filters to rows whose request_debug sidecar exists and is still readable, so
// the listing never advertises a capture document audit_capture_get would 404.
// Session and cursor text are byte-exact hex TEXT expressions rather than
// quoted literals: a schema-valid value such as "a;b" or "attach" would
// otherwise trip the proxy's statement gate even though SQLite would parse it.
func captureListingStatement(sessionID string, before *pageCursor, limit int, nowMs int64) string {
	statement := "SELECT id, started_at, status_code, provider, model, client, debug_session_id, cost FROM requests" +
		" WHERE debug = 1 AND EXISTS (SELECT 1 FROM request_debug AS d WHERE d.id = requests.id AND (d.expires_at = 0 OR d.expires_at > " +
		strconv.FormatInt(nowMs, 10) + "))"
	if sessionID != "" {
		statement += " AND debug_session_id = " + sqlHexText(sessionID)
	}
	if before != nil {
		statement += " AND (started_at, id) < (" + strconv.FormatInt(before.StartMs, 10) + ", " + sqlHexText(before.ID) + ")"
	}
	statement += " ORDER BY started_at DESC, id DESC LIMIT " + strconv.Itoa(limit)
	return statement
}

// sqlHexText renders arbitrary client-controlled text as a SQLite TEXT
// expression built from a hex blob literal, so the statement source contains
// only [0-9a-f] and fixed SQL words. The proxy's statement gate scans the whole
// SQL text, including literals, for ';' and for ATTACH/DETACH/PRAGMA/
// LOAD_EXTENSION as words, so a raw spelling such as "attach" or "a;b" cannot
// ride a quoted literal through it even though SQLite would parse that
// literal correctly. Hex encoding is byte-exact, so quotes, semicolons,
// backslashes, NUL and non-ASCII bytes all survive. CAST(... AS TEXT) is
// required: a bare blob would compare as a BLOB against the model column's
// TEXT, which never compares equal and sorts after every text value. Under the
// column's BINARY collation the cast result compares exactly like the quoted
// literal it replaces.
func sqlHexText(value string) string {
	return "CAST(X'" + hex.EncodeToString([]byte(value)) + "' AS TEXT)"
}
