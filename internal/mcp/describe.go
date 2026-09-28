package mcp

import "context"

// DescribeOutput is the reference an LLM needs before it can write good SQL or
// call any other tool. It is deliberately self-contained: schema, column
// semantics, the health predicates, the filter vocabularies, the live known-*
// sets and the server's own result limits.
type DescribeOutput struct {
	// Schema is the live SQLite schema exactly as the running database holds
	// it, read through the same bounded SELECT reader every query uses.
	Schema []SchemaObject `json:"schema" jsonschema:"live sqlite_schema contents for the running database"`
	// Storage reports whether durable storage is on. With it off, query,
	// captures and the durable log page cannot work.
	Storage StorageInfo `json:"storage"`
	// Tables documents the columns an analysis actually uses.
	Tables []TableDoc `json:"tables"`
	// Predicates are the exact SQL health predicates, so a model never has to
	// guess what "is an error" means.
	Predicates PredicateDoc `json:"predicates"`
	// Dimensions, StatusClasses and LiveStatuses are the accepted vocabularies
	// for the explorer, its status facet and the live s= selector.
	Dimensions    []string `json:"dimensions"`
	StatusClasses []string `json:"status_classes"`
	LiveStatuses  []string `json:"live_statuses"`
	// KnownClients/Providers/Models are the live capture-scope vocabularies.
	KnownClients   []string `json:"known_clients"`
	KnownProviders []string `json:"known_providers"`
	KnownModels    []string `json:"known_models"`
	// Indexes lists the indexes a query planner can use, including the absence
	// of one on status_code.
	Indexes []string `json:"indexes"`
	// Limits are this server's result limits, so a model can size a query
	// instead of discovering the ceiling by being cut off.
	Limits LimitDoc `json:"limits"`
	// Notes carries the few rules that are not expressible as a table.
	Notes []string `json:"notes"`
}

// SchemaObject is one row of sqlite_schema.
type SchemaObject struct {
	Type  string `json:"type" jsonschema:"object type: table, index, view or trigger"`
	Name  string `json:"name" jsonschema:"object name"`
	Table string `json:"tbl_name" jsonschema:"owning table for an index or trigger"`
	SQL   string `json:"sql,omitempty" jsonschema:"exact DDL as the running database stores it"`
}

// StorageInfo reports durable-storage availability.
type StorageInfo struct {
	Enabled        bool   `json:"enabled" jsonschema:"false means query, capture and durable paging are unavailable"`
	Dropped        uint64 `json:"dropped" jsonschema:"records dropped by write-channel overflow since boot"`
	TotalsDegraded bool   `json:"totals_degraded" jsonschema:"true when since-inception totals exclude a failed boot scan"`
}

// TableDoc documents one table's columns.
type TableDoc struct {
	Table   string      `json:"table" jsonschema:"table name"`
	Summary string      `json:"summary" jsonschema:"what the table holds"`
	Columns []ColumnDoc `json:"columns" jsonschema:"the columns an analysis uses, with their units and meaning"`
	Notes   []string    `json:"notes,omitempty" jsonschema:"table-specific rules a query author must know"`
}

// ColumnDoc documents one column.
type ColumnDoc struct {
	Name    string `json:"name" jsonschema:"exact column name"`
	Type    string `json:"type" jsonschema:"storage type"`
	Unit    string `json:"unit,omitempty" jsonschema:"unit of the stored value"`
	Meaning string `json:"meaning" jsonschema:"what the column means and how to use it"`
}

// PredicateDoc carries the health predicates as ready-to-use SQL.
type PredicateDoc struct {
	IsError     string `json:"is_error" jsonschema:"exact SQL for the canonical error predicate"`
	RateLimited string `json:"rate_limited" jsonschema:"exact SQL for a request that saw a 429 on the final or any absorbed attempt"`
	Note        string `json:"note" jsonschema:"why 429 alone is not an error"`
}

// LimitDoc reports the server's result limits.
type LimitDoc struct {
	QueryMaxRows        int    `json:"query_max_rows" jsonschema:"this server's client-side cap on query rows"`
	LogPageMin          int    `json:"log_page_min" jsonschema:"smallest durable log page the proxy accepts"`
	LogPageMax          int    `json:"log_page_max" jsonschema:"largest durable log page the proxy accepts"`
	ExplorerGroups      int    `json:"explorer_max_groups" jsonschema:"maximum groups an explorer response carries"`
	ChartMaxBuckets     int    `json:"chart_max_buckets" jsonschema:"maximum chart buckets in a response"`
	MaxScopeFilters     int    `json:"max_scope_filters" jsonschema:"maximum repeated filters in one scope"`
	ServerQueryRowLimit string `json:"server_query_row_limit" jsonschema:"the proxy's own storage_query_max_rows, enforced with HTTP 413"`
}

// Describe assembles the reference. The schema probe goes through the same
// bounded reader as every other query; if durable storage is off the proxy
// answers 503 and that is reported as data, not as a tool failure, because the
// rest of the reference is still true.
func (s *Service) describe(ctx context.Context, _ DescribeInput) (*DescribeOutput, error) {
	out := &DescribeOutput{
		Dimensions:    Dimensions,
		StatusClasses: statusClasses,
		LiveStatuses:  liveStatusClasses,
		Limits: LimitDoc{
			QueryMaxRows:        s.limits.QueryMaxRows,
			LogPageMin:          logPageMin,
			LogPageMax:          logPageMax,
			ExplorerGroups:      explorerMaxGroups,
			ChartMaxBuckets:     chartMaxBuckets,
			MaxScopeFilters:     maxScopeFilters,
			ServerQueryRowLimit: serverQueryRowLimitNote,
		},
		Indexes: []string{
			"idx_requests_log(started_at, id): the keyset order the durable log page and deep history use",
			"idx_requests_provider(provider)",
			"idx_requests_model(model)",
			"there is NO index on status_code, error_type, cost or started_at alone: filtering on those scans the table",
			"request_debug(payload) holds captured request and response BODIES; reading it is sensitive",
		},
		Tables:     tableDocs,
		Predicates: predicates,
		Notes: []string{
			"Timestamps are Unix MILLISECONDS, not seconds: started_at, first_token_at, last_token_at, first_answer_at and final_attempt_at.",
			"ttft_ms, duration_ms, processing_ms, queue_wait_ms and retry_after_ms are integer milliseconds.",
			"cost is USD. overall_tps and gen_tps are tokens per second.",
			"attempts and tool_names are JSON-array TEXT columns: use json_each(attempts) or json_extract(attempts, '$[0].status_code').",
			"Use keyset pagination (WHERE (started_at, id) < (?, ?)) rather than OFFSET for deep history; OFFSET degrades on every page.",
			"For history older than the snapshot window, page /metrics/agg/log with the paired before_ms/before_id cursor. The snapshot is capped at 8 * dash_log_rows records.",
			"dashboard_version is a frontend asset fingerprint, not an API version. There is no API versioning and no published stability contract.",
			"Filters in the export and purge grammars match raw stored values, never canonicalized model names.",
		},
	}

	snapshot, err := s.client.bootstrap(ctx)
	if err != nil {
		return nil, err
	}
	out.Storage = StorageInfo{
		Enabled:        snapshot.Storage.Enabled,
		Dropped:        snapshot.Storage.Dropped,
		TotalsDegraded: snapshot.Storage.TotalsDegraded,
	}

	status, err := s.client.debug(ctx)
	if err != nil {
		// The capture vocabulary is a convenience; losing it must not hide the
		// schema and the predicates.
		out.Notes = append(out.Notes, "known_clients/providers/models could not be read: "+err.Error())
	} else {
		out.KnownClients = list(status.KnownClients)
		out.KnownProviders = list(status.KnownProviders)
		out.KnownModels = list(status.KnownModels)
	}

	schemaRows, err := s.client.querySQL(ctx, schemaStatement)
	if err != nil {
		if !snapshot.Storage.Enabled {
			out.Notes = append(out.Notes,
				"the live schema probe needs durable storage (storage.enabled is false), so only the static reference below is available")
			return out, nil
		}
		return nil, err
	}
	out.Schema = make([]SchemaObject, 0, len(schemaRows))
	for _, row := range schemaRows {
		out.Schema = append(out.Schema, SchemaObject{
			Type:  rowString(row, "type"),
			Name:  rowString(row, "name"),
			Table: rowString(row, "tbl_name"),
			SQL:   rowString(row, "sql"),
		})
	}
	return out, nil
}

// DescribeInput takes no arguments: the reference is a property of the running
// proxy, not of a request.
type DescribeInput struct{}

// statusClasses are the explorer's status facet values.
var statusClasses = []string{"2xx", "3xx", "4xx", "5xx"}

// schemaLimits documents the proxy's own bounds. They are constants here
// because they describe the API this server speaks, and describe must state
// them even before any query runs; the config-driven storage_query_max_rows is
// quoted as prose because it is per-deployment.
const (
	logPageMin              = 10
	logPageMax              = 500
	explorerMaxGroups       = 24
	chartMaxBuckets         = 31
	serverQueryRowLimitNote = "storage_query_max_rows (per-deployment, 413 past it); storage_query_max_bytes bounds the encoded result"
)

// tableDocs is the column reference. It documents the columns an analysis
// actually uses rather than restating DDL, which the live schema probe above
// already returns exactly.
var tableDocs = []TableDoc{
	{
		Table:   "requests",
		Summary: "one finalized request per row; the whole history, and the only table most analysis needs",
		Columns: []ColumnDoc{
			{Name: "id", Type: "TEXT", Meaning: "primary key; also the id the debug capture endpoint takes"},
			{Name: "provider", Type: "TEXT", Meaning: "upstream provider label"},
			{Name: "model", Type: "TEXT", Meaning: "requested model, raw spelling (never canonicalized)"},
			{Name: "provider_model", Type: "TEXT", Meaning: "model the upstream echoed back, when it differs"},
			{Name: "client", Type: "TEXT", Meaning: "friendly client classification (for example desktop-client)"},
			{Name: "key_hash", Type: "TEXT", Meaning: "SHA-256 of the upstream key; never the key"},
			{Name: "user_agent", Type: "TEXT", Meaning: "raw client user agent"},
			{Name: "status_code", Type: "INTEGER", Meaning: "final HTTP status returned to the client"},
			{Name: "started_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "request start; the ordering key for keyset pagination"},
			{Name: "duration_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "end to end client-visible duration"},
			{Name: "ttft_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "time to first token, measured from the successful attempt when retries happened"},
			{Name: "processing_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "provider-reported server processing time"},
			{Name: "queue_wait_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "time parked in the scheduler queue"},
			{Name: "retry_after_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "upstream Retry-After hint on a 429"},
			{Name: "input_tokens", Type: "INTEGER", Meaning: "prompt tokens billed"},
			{Name: "output_tokens", Type: "INTEGER", Meaning: "completion tokens billed"},
			{Name: "total_tokens", Type: "INTEGER", Meaning: "input + output tokens"},
			{Name: "cache_read_tokens", Type: "INTEGER", Meaning: "cached prompt tokens, a subset of input_tokens"},
			{Name: "reasoning_tokens", Type: "INTEGER", Meaning: "reasoning tokens inside output_tokens"},
			{Name: "gen_tokens", Type: "INTEGER", Meaning: "generated tokens used for the generation speed measure"},
			{Name: "cost", Type: "REAL", Unit: "USD", Meaning: "reported cost; 0 when the provider reported none"},
			{Name: "overall_tps", Type: "REAL", Unit: "tokens/second", Meaning: "end-to-end generation speed"},
			{Name: "gen_tps", Type: "REAL", Unit: "tokens/second", Meaning: "decode speed after the first token"},
			{Name: "stream", Type: "INTEGER", Meaning: "1 when the client asked for a streamed response"},
			{Name: "tool_calls", Type: "INTEGER", Meaning: "count of tool calls in the response"},
			{Name: "tool_names", Type: "TEXT", Meaning: "JSON array of tool names; use json_each or json_extract"},
			{Name: "attempts", Type: "TEXT", Meaning: "JSON array of absorbed upstream attempts; each has status_code, error_type and at"},
			{Name: "final_attempt_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "start of the attempt that produced the returned response"},
			{Name: "error_type", Type: "TEXT", Meaning: "classified error class; empty on success"},
			{Name: "error_code", Type: "TEXT", Meaning: "provider-specific error code"},
			{Name: "error_msg", Type: "TEXT", Meaning: "upstream error text; may be sensitive"},
			{Name: "finish_reason", Type: "TEXT", Meaning: "upstream finish reason"},
			{Name: "retries", Type: "INTEGER", Meaning: "number of transparently absorbed upstream retries"},
			{Name: "rate_limited", Type: "INTEGER", Meaning: "scheduler pacing flag; NOT the 429 predicate"},
			{Name: "conversation_id", Type: "TEXT", Meaning: "conversation grouping key; the explorer's conversation facet"},
			{Name: "parent_conversation_id", Type: "TEXT", Meaning: "client-declared parent conversation"},
			{Name: "debug", Type: "INTEGER", Meaning: "1 when a debug capture session matched this request"},
			{Name: "debug_session_id", Type: "TEXT", Meaning: "the capture session that matched"},
			{Name: "client_disconnected", Type: "INTEGER", Meaning: "1 when the client went away first"},
			{Name: "path", Type: "TEXT", Meaning: "inbound request path"},
		},
		Notes: []string{
			"No index on status_code, error_type or cost: filter on started_at ranges or an indexed column to keep a scan bounded.",
			"debug = 1 rows are exactly the requests with a readable capture document (until debug_capture_ttl expires).",
		},
	},
	{
		Table:   "request_debug",
		Summary: "captured request and response documents, one per captured request",
		Columns: []ColumnDoc{
			{Name: "id", Type: "TEXT", Meaning: "the REQUEST RECORD id, not the session id"},
			{Name: "session_id", Type: "TEXT", Meaning: "the debug session that captured it"},
			{Name: "captured_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "when the capture was taken"},
			{Name: "expires_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "retention deadline (debug_capture_ttl)"},
			{Name: "payload", Type: "TEXT", Meaning: "request and response headers and bodies; SENSITIVE even after credential redaction"},
		},
		Notes: []string{
			"Only the capture endpoint returns a decoded payload; selecting payload here returns one JSON string per row.",
			"Capture requires durable storage. With db_path empty, sessions start and nothing is ever stored.",
		},
	},
	{
		Table:   "meta",
		Summary: "proxy-owned bookkeeping: durable totals, pause/throttle/debug persistence",
		Columns: []ColumnDoc{
			{Name: "key", Type: "TEXT", Meaning: "bookkeeping key"},
			{Name: "value", Type: "TEXT", Meaning: "serialized value; shape is internal and not a stable contract"},
		},
	},
	{
		Table:   "request_projection_state",
		Summary: "single-row analytical projection bookkeeping (destructive epoch and rowid high-water mark)",
		Columns: []ColumnDoc{
			{Name: "singleton", Type: "INTEGER", Meaning: "always 1; the table holds exactly one row"},
			{Name: "epoch", Type: "INTEGER", Meaning: "incremented on any destructive change; cached aggregates invalidate on it"},
			{Name: "high_rowid", Type: "INTEGER", Meaning: "highest requests rowid covered by the projection"},
		},
	},
	{
		Table:   "sqlite_schema",
		Summary: "the live schema of every table, index and trigger in the database",
		Columns: []ColumnDoc{
			{Name: "type", Type: "TEXT", Meaning: "table, index, view or trigger"},
			{Name: "name", Type: "TEXT", Meaning: "object name"},
			{Name: "tbl_name", Type: "TEXT", Meaning: "owning table"},
			{Name: "sql", Type: "TEXT", Meaning: "exact DDL"},
		},
	},
}

// predicates are the health predicates, quoted exactly. They mirror the proxy's
// metrics.Record.IsError and Record.HasRateLimit so a model's SQL and the
// dashboard's counts can never disagree.
var predicates = PredicateDoc{
	IsError: `(
  (status_code = 429 AND EXISTS (SELECT 1 FROM json_each(requests.attempts) WHERE json_extract(value, '$.status_code') >= 500))
  OR (status_code = 499 AND (error_type != '' OR EXISTS (SELECT 1 FROM json_each(requests.attempts) WHERE json_extract(value, '$.status_code') >= 500)))
  OR (status_code NOT IN (429, 499) AND (error_type != '' OR status_code >= 400))
  OR (status_code < 400 AND EXISTS (SELECT 1 FROM json_each(requests.attempts) WHERE json_extract(value, '$.status_code') >= 500))
)`,
	RateLimited: `(status_code = 429 OR EXISTS (SELECT 1 FROM json_each(requests.attempts) WHERE json_extract(value, '$.status_code') = 429))`,
	Note: "err_final counts distinct requests through is_error; rate_limit_requests counts distinct requests through rate_limited. " +
		"A 429 alone is NOT an error: it is flow control. An absorbed 5xx in attempts IS an error even when a retry recovered. " +
		"A 499 is a client abort unless error_type is set, which means the upstream had already failed in band.",
}

// The column reference above is curated rather than generated: it states the
// meaning and unit an analysis needs, which DDL cannot. A new column is a
// deliberate edit here.
