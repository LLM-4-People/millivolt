package mcp

import (
	"context"
	"slices"
	"strconv"
)

// sortedSet renders a name set in a stable order, so describe output does not
// depend on map iteration.
func sortedSet(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

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
	// Vocabularies are the accepted values of every filter dimension and of the
	// live status selector. A dimension listed with an empty Values is free text
	// and points at the values tool.
	Vocabularies []VocabularyDoc `json:"vocabularies"`
	// KnownClients/Providers/Models are the live capture-scope vocabularies.
	KnownClients   []string `json:"known_clients"`
	KnownProviders []string `json:"known_providers"`
	KnownModels    []string `json:"known_models"`
	// KnownTools are the tool names the most recent live records actually
	// carried. They are from the ring, not from all history.
	KnownTools []string `json:"known_tools"`
	// Indexes lists the indexes a query planner can use, including the absence
	// of one on status_code.
	Indexes []string `json:"indexes"`
	// Limits are this server's result limits, so a model can size a query
	// instead of discovering the ceiling by being cut off.
	Limits LimitDoc `json:"limits"`
	// Notes carries the few rules that are not expressible as a table.
	Notes []string `json:"notes"`
}

// VocabularyDoc bounds one dimension's value space. A dimension whose values are
// a closed set lists them; a free-text dimension says so and points at the
// values tool, because an unknown value on those is an empty SUCCESS and not an
// error, which is the single most damaging gap in an otherwise silent surface.
type VocabularyDoc struct {
	Dimension string   `json:"dimension" jsonschema:"the filter dimension, or live_statuses for the s= selector"`
	Values    []string `json:"values" jsonschema:"the accepted values, or an empty array when the dimension is free text"`
	Note      string   `json:"note" jsonschema:"how this value space is bounded, and what a wrong value does"`
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
	QueryMaxBytes       int    `json:"query_max_bytes" jsonschema:"this server's client-side cap on the encoded size of one query result"`
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
		Vocabularies: vocabularies,
		KnownTools:   []string{},
		Limits: LimitDoc{
			QueryMaxRows:        s.limits.QueryMaxRows,
			QueryMaxBytes:       s.limits.QueryMaxBytes,
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
			"There is NO unixnow() function in this SQLite build. For a relative window use strftime('%s','now')*1000, " +
				"for example WHERE started_at >= strftime('%s','now')*1000 - 3600000 for the last hour.",
			"Use keyset pagination (WHERE (started_at, id) < (?, ?)) rather than OFFSET for deep history; OFFSET degrades on every page.",
			"For history older than the snapshot window, page /metrics/agg/log with the paired before_ms/before_id cursor. The snapshot is capped at 8 * dash_log_rows records.",
			"dashboard_version is a frontend asset fingerprint, not an API version. There is no API versioning and no published stability contract.",
			"Filters in the export and purge grammars match raw stored values, never canonicalized model names. " +
				"The explore, chart and records scope filters are the opposite: they match the CANONICAL model spelling the proxy derives " +
				"from its model_rules, which is what the values tool on dim 'model' enumerates.",
			"Structural limits worth knowing before you plan a query: explore has NO time window and breaks history down by ONE dimension, " +
				"so it cannot cross-tabulate; chart is the only windowed tool and cannot group; query is the only tool that can express a time range in SQL. " +
				"The explorer caps groups at " + strconv.Itoa(explorerMaxGroups) + " and the chart caps buckets at " + strconv.Itoa(chartMaxBuckets) + " SERVER-SIDE, " +
				"so those drops happen in the proxy and the tools' own truncated flag is false when groups were dropped.",
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
	out.KnownTools = observedToolNames(snapshot.Records)

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

// StatusClasses are the explorer's status facet values, exactly as the proxy
// derives them (internal/web statusClass). They are NOT the HTTP class
// shorthand a reader would guess: there is no 3xx class at all, because a 3xx
// never reaches a recorded row, and a 499 is its own "cancel" class rather than
// a 4xx. Listing a 3xx here would make `f=status:3xx` a silently empty filter,
// and omitting cancel would hide the class every client abort falls into.
var StatusClasses = []string{"2xx", "cancel", "4xx", "5xx", "err"}

// TimeBuckets are the values of the `time` dimension: a server-local daypart,
// NOT a duration. The proxy computes them from the record's start time
// (internal/metrics TimeBucket), so `f=time:24h` is a valid-looking filter that
// matches nothing, and the only way to select a recent window is the chart
// window or a started_at range in SQL.
var TimeBuckets = []string{"night", "work", "evening", "weekend"}

// ErrorFilterKeys are the three fields an `error` filter value is built from. The
// proxy joins them with '|' in that order (internal/web errorKey), and fills the
// derived parts: a missing type becomes http_<status> and a missing code becomes
// the status itself. So an error value looks like
// "upstream_timeout|429|rate limited", and the parts are the exact stored
// error_type, error_code and error_msg.
var ErrorFilterKeys = []string{"type", "code", "message"}

// vocabularies is the one owner of every filter dimension's value space. A
// dimension with no closed set says so and points at the values tool, which is
// how a model discovers the names before filtering on them.
var vocabularies = []VocabularyDoc{
	{
		Dimension: "status",
		Values:    StatusClasses,
		Note: "the proxy's own status classes, not HTTP shorthand: there is no 3xx class (a 3xx never reaches a recorded row), " +
			"499 is 'cancel' and not a 4xx, and a status below 200 is 'err'. " +
			"The s= selector is a DIFFERENT vocabulary: an exact code or a live class",
	},
	{
		Dimension: "live_statuses",
		Values:    liveStatusClasses,
		Note:      "what the s= selector accepts beside an exact HTTP status code; it is not the status dimension",
	},
	{
		Dimension: "time",
		Values:    TimeBuckets,
		Note: "a server-local daypart bucket, NOT a duration: 'time:24h' matches nothing. " +
			"weekend is Saturday or Sunday, night is before 08:00, work is 08:00 to 16:00, evening is from 16:00, in the server's local time",
	},
	{
		Dimension: "error",
		Values:    ErrorFilterKeys,
		Note: "a filter value is type|code|message joined with '|', in that order, from the stored error_type, error_code and error_msg. " +
			"An absent type is written http_<status> and an absent code is written as the status, so the value never has an empty part. " +
			"Use the values tool on error_type, error_code and error_message to discover them",
	},
	{
		Dimension: "tool",
		Note: "the tool names carried by the request's tool_names array; free text. known_tools lists what the most recent live records carried, " +
			"and the values tool on dim 'tool' enumerates them across durable history",
	},
	{
		Dimension: "client",
		Note:      "free text: the proxy's client classification. Use the values tool on dim 'client'. An unknown value is an EMPTY result, not an error",
	},
	{
		Dimension: "provider",
		Note:      "free text: the upstream provider label. Use the values tool on dim 'provider'. An unknown value is an EMPTY result, not an error",
	},
	{
		Dimension: "model",
		Note: "the CANONICAL spelling the explore, chart and records filters match, folded through the proxy's configured model_rules; " +
			"the values tool on dim 'model' enumerates those canonical names. A raw stored spelling matches nothing. An unknown value is an EMPTY result, not an error",
	},
	{
		Dimension: "conversation",
		Note: "the conversation_id grouping key. The values tool lists the most frequent ones, but this dimension is not enumerable exhaustively " +
			"because history holds many conversations",
	},
	{
		Dimension: "key",
		Note:      "the key_hash (a SHA-256 prefix digest of the upstream key, never the key). The values tool enumerates the digests in use",
	},
}

// observedToolNames reads the tool names the live snapshot's recent records
// actually carried. It costs no extra request because the snapshot is already
// fetched, and it is labelled as live-ring evidence in the field's meaning
// rather than as a history-wide vocabulary, which is what the values tool is for.
func observedToolNames(records []map[string]any) []string {
	seen := map[string]bool{}
	for _, record := range records {
		names, ok := record["tool_names"].([]any)
		if !ok {
			continue
		}
		for _, name := range names {
			if text, ok := name.(string); ok && text != "" {
				seen[text] = true
			}
		}
	}
	return sortedSet(seen)
}

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
			{Name: "client_ip", Type: "TEXT", Meaning: "client address as observed; may be a proxy address, so treat it as a hint, not an identity"},
			{Name: "client_lang", Type: "TEXT", Meaning: "client-declared language, when the client sent one"},
			{Name: "method", Type: "TEXT", Meaning: "inbound HTTP method"},
			{Name: "status_code", Type: "INTEGER", Meaning: "final HTTP status returned to the client"},
			{Name: "started_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "request start; the ordering key for keyset pagination"},
			{Name: "first_token_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "absolute time of the first streamed token; 0 when nothing streamed"},
			{Name: "last_token_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "absolute time of the last token, so ttft_ms is first_token_at - started_at"},
			{Name: "first_answer_at", Type: "INTEGER", Unit: "unix milliseconds", Meaning: "absolute time of the first non-empty assistant content, when any; 0 for a tool-call-only answer"},
			{Name: "duration_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "end to end client-visible duration"},
			{Name: "ttft_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "time to first token, measured from the successful attempt when retries happened"},
			{Name: "processing_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "provider-reported server processing time"},
			{Name: "queue_wait_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "time parked in the scheduler queue"},
			{Name: "retry_after_ms", Type: "INTEGER", Unit: "milliseconds", Meaning: "upstream Retry-After hint on a 429"},
			{Name: "input_tokens", Type: "INTEGER", Meaning: "prompt tokens billed"},
			{Name: "output_tokens", Type: "INTEGER", Meaning: "completion tokens billed"},
			{Name: "total_tokens", Type: "INTEGER", Meaning: "input + output tokens"},
			{Name: "cache_read_tokens", Type: "INTEGER", Meaning: "cached prompt tokens, a subset of input_tokens"},
			{Name: "cache_write_tokens", Type: "INTEGER", Meaning: "prompt tokens written to the upstream cache"},
			{Name: "reasoning_tokens", Type: "INTEGER", Meaning: "reasoning tokens inside output_tokens"},
			{Name: "gen_tokens", Type: "INTEGER", Meaning: "generated tokens used for the generation speed measure"},
			{Name: "answer_tokens", Type: "INTEGER", Meaning: "tokens in actual answer content, excluding tool-call payloads"},
			{Name: "had_answer_content", Type: "INTEGER", Meaning: "1 when the response carried non-empty assistant content rather than only tool calls"},
			{Name: "cost", Type: "REAL", Unit: "USD", Meaning: "reported cost; 0 when the provider reported none"},
			{Name: "overall_tps", Type: "REAL", Unit: "tokens/second", Meaning: "end-to-end generation speed"},
			{Name: "gen_tps", Type: "REAL", Unit: "tokens/second", Meaning: "decode speed after the first token"},
			{Name: "turns_user", Type: "INTEGER", Meaning: "user turns in the request"},
			{Name: "turns_assistant", Type: "INTEGER", Meaning: "assistant turns in the request"},
			{Name: "turns_tool", Type: "INTEGER", Meaning: "tool-result turns in the request"},
			{Name: "last_turn_role", Type: "TEXT", Meaning: "role of the final turn in the request"},
			{Name: "chars_system", Type: "INTEGER", Meaning: "system-prompt characters"},
			{Name: "chars_user", Type: "INTEGER", Meaning: "user-message characters"},
			{Name: "chars_assistant", Type: "INTEGER", Meaning: "assistant-message characters in the request history"},
			{Name: "chars_tool", Type: "INTEGER", Meaning: "tool-result characters in the request history"},
			{Name: "images", Type: "INTEGER", Meaning: "images attached to the request"},
			{Name: "attachments", Type: "INTEGER", Meaning: "non-image attachments"},
			{Name: "client_meta", Type: "TEXT", Meaning: "serialized client metadata; its shape is internal, not a stable contract"},
			{Name: "prompt_preview", Type: "TEXT", Meaning: "short preview of the prompt; SENSITIVE, and empty unless content capture is enabled"},
			{Name: "response_preview", Type: "TEXT", Meaning: "short preview of the response; SENSITIVE, and empty unless content capture is enabled"},
			{Name: "response_headers", Type: "TEXT", Meaning: "upstream response headers with credentials redacted; JSON object text"},
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
			{Name: "rate_limit_remaining", Type: "INTEGER", Meaning: "requests left in the provider's window after this request; -1 when the provider reported no budget"},
			{Name: "rate_limit_limit", Type: "INTEGER", Meaning: "the provider's request budget for the window; -1 when unreported"},
			{Name: "provider_request_id", Type: "TEXT", Meaning: "upstream request id, for correlating with a provider's own logs"},
			{Name: "provider_server", Type: "TEXT", Meaning: "upstream server label, when the provider reported one"},
			{Name: "conversation_id", Type: "TEXT", Meaning: "conversation grouping key; the explorer's conversation facet"},
			{Name: "parent_conversation_id", Type: "TEXT", Meaning: "client-declared parent conversation"},
			{Name: "debug", Type: "INTEGER", Meaning: "1 when a debug capture session matched this request"},
			{Name: "debug_session_id", Type: "TEXT", Meaning: "the capture session that matched"},
			{Name: "client_disconnected", Type: "INTEGER", Meaning: "1 when the client went away first"},
			{Name: "path", Type: "TEXT", Meaning: "inbound request path"},
			{Name: "req_max_tokens", Type: "INTEGER", Meaning: "requested max_tokens; 0 when unset"},
			{Name: "req_temperature", Type: "REAL", Meaning: "requested temperature; 0 when unset, so 0 and unset are indistinguishable"},
			{Name: "req_top_p", Type: "REAL", Meaning: "requested top_p; 0 when unset"},
			{Name: "req_n", Type: "INTEGER", Meaning: "requested number of choices"},
			{Name: "req_stop", Type: "INTEGER", Meaning: "count of stop sequences requested"},
			{Name: "req_seed", Type: "INTEGER", Meaning: "requested seed; 0 when unset"},
			{Name: "req_tools_count", Type: "INTEGER", Meaning: "how many tool definitions the request offered"},
			{Name: "req_tool_choice", Type: "TEXT", Meaning: "requested tool choice policy"},
			{Name: "req_parallel_tools", Type: "INTEGER", Meaning: "parallel tool call flag; 0 when unset or off"},
			{Name: "req_logit_bias", Type: "INTEGER", Meaning: "whether a logit bias was supplied; the values themselves are not stored"},
			{Name: "req_top_logprobs", Type: "INTEGER", Meaning: "requested top_logprobs"},
			{Name: "req_logprobs", Type: "INTEGER", Meaning: "whether logprobs were requested"},
			{Name: "req_response_format", Type: "TEXT", Meaning: "requested response format, for example json_object"},
			{Name: "req_service_tier", Type: "TEXT", Meaning: "requested service tier"},
			{Name: "req_thinking", Type: "INTEGER", Meaning: "whether a reasoning/thinking budget was requested"},
			{Name: "req_reasoning_effort", Type: "TEXT", Meaning: "requested reasoning effort level"},
			{Name: "req_verbosity", Type: "TEXT", Meaning: "requested verbosity level"},
			{Name: "req_metadata_keys", Type: "INTEGER", Meaning: "count of client metadata keys; the values are not stored"},
			{Name: "req_stream_opts", Type: "INTEGER", Meaning: "stream options bitfield as sent by the client"},
		},
		Notes: []string{
			"No index on status_code, error_type or cost: filter on started_at ranges or an indexed column to keep a scan bounded.",
			"debug = 1 rows are exactly the requests with a readable capture document (until debug_capture_ttl expires), and a purge of those rows " +
				"deletes the capture document with them.",
			"The req_* family records the request PARAMETERS the client asked for, not what the upstream did. 0 is used for every unset value, " +
				"so a 0 cannot be distinguished from an absent one.",
			"prompt_preview and response_preview are captured content and are SENSITIVE; with content capture off they are empty.",
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
