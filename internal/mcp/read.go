package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// rows is one SQL result set: the proxy returns a flat object per row, keyed by
// column name, with JSON scalars (a number may arrive as float64, int64, or
// string depending on the column's declared type).
type rows []map[string]any

// querySQL runs one bounded SELECT through the proxy's read-only SQL reader.
// This is the single owner of a SQL statement leaving this server; the describe
// tool's schema probe uses it too.
func (c *Client) querySQL(ctx context.Context, statement string) (rows, error) {
	if err := validateSelect(statement); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Set("q", statement)
	var out rows
	if err := c.getJSON(ctx, routeQuery, values, &out); err != nil {
		return nil, err
	}
	return out, nil
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
	Storage storageState     `json:"storage"`
	Records []map[string]any `json:"records"`
}

// bootstrap reads the live snapshot for the storage signal and the headline
// counters.
func (c *Client) bootstrap(ctx context.Context) (*bootstrapSnapshot, error) {
	var snapshot bootstrapSnapshot
	if err := c.getJSON(ctx, routeBootstrap, nil, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// debug reads the capture session state.
func (c *Client) debug(ctx context.Context) (*debugStatus, error) {
	var status debugStatus
	if err := c.getJSON(ctx, routeDebug, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// captureRecords lists captured request records, newest first. The proxy has no
// listing endpoint for captures, so the record ids come from a SQL probe over
// the requests table: `debug = 1` marks a captured request and
// `debug_session_id` names the session. beforeMs/beforeID are the same paired
// keyset cursor the durable log page uses, so both or neither.
func (c *Client) captureRecords(ctx context.Context, sessionID string, beforeMs int64, beforeID string, limit int) (rows, error) {
	statement := "SELECT id, started_at, status_code, provider, model, client, debug_session_id, cost FROM requests WHERE debug = 1"
	if sessionID != "" {
		statement += " AND debug_session_id = " + sqlString(sessionID)
	}
	if beforeID != "" {
		statement += " AND (started_at, id) < (" + strconv.FormatInt(beforeMs, 10) + ", " + sqlString(beforeID) + ")"
	}
	statement += " ORDER BY started_at DESC, id DESC LIMIT " + strconv.Itoa(limit)
	return c.querySQL(ctx, statement)
}

// sqlString quotes a string literal for the read-only SQL surface. Single
// quotes are doubled; the result is always a literal, never an expression.
func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
