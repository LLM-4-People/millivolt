package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Service binds the client and the limits policy, and owns tool registration.
// Every handler is a method here rather than a free function, so no tool can
// reach a client or a limit without going through this owner.
type Service struct {
	client *Client
	limits Limits
	// previewToken authorizes a purge against a real preview of the same
	// filter. It is derived from the operator credential at construction, so a
	// rotated credential invalidates every outstanding preview.
	previewToken previewToken
}

// NewService validates the setup parameters and returns the tool service. It is
// the single construction path: a caller cannot obtain a Service without a
// validated origin, a validated credential and a validated limits policy.
func NewService(proxyURL, operatorToken string, limits Limits) (*Service, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	client, err := NewClient(proxyURL, operatorToken, limits)
	if err != nil {
		return nil, err
	}
	return &Service{client: client, limits: limits, previewToken: newPreviewToken(operatorToken)}, nil
}

// Origin is the proxy origin the tools call. It never carries the credential.
func (s *Service) Origin() string { return s.client.Origin() }

// handler is the shape of every tool implementation in this package. Keeping
// the plain context-and-input signature means the handlers are ordinary methods
// that a test can call directly, with no SDK plumbing in the way.
type handler[In, Out any] func(context.Context, In) (*Out, error)

// addTool adapts a handler to the SDK's ToolHandlerFor. A returned error becomes
// a tool result the model can read (the SDK marks it as an error result), never
// a transport-level crash. The zero Out accompanies an error and is never
// serialized into the result.
func addTool[In, Out any](server *sdk.Server, tool *sdk.Tool, run handler[In, Out]) {
	sdk.AddTool(server, tool, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		out, err := run(ctx, in)
		if err != nil {
			var zero Out
			return nil, zero, err
		}
		if out == nil {
			var zero Out
			return nil, zero, nil
		}
		return nil, *out, nil
	})
}

// Register installs every tool on the server. The list is the whole public
// surface of this server: there is no path that reaches the proxy's
// transparent inference catch-all, and no destructive call other than purge.
func (s *Service) Register(server *sdk.Server) {
	readOnly := func(title string) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true}
	}
	mutating := func(title string) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{Title: title, DestructiveHint: boolPtr(false)}
	}
	destructive := func(title string) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{Title: title, DestructiveHint: boolPtr(true)}
	}

	addTool(server, &sdk.Tool{
		Name:        "describe",
		Title:       "Describe the millivolt instance",
		Description: "Start here. Returns the live SQLite schema, the full requests/request_debug/meta column reference with units and meaning, the exact SQL health predicates (a 429 alone is not an error), the value vocabulary of EVERY filter dimension (including the status classes - there is no 3xx, and 499 is 'cancel' - the time daypart buckets, and the error type|code|message key), the live known client/provider/model names and tool names, the indexes, every result limit, and the working SQL idioms for this build. Without it you cannot write correct SQL: timestamps are Unix milliseconds, cost is USD, attempts/tool_names are JSON-array text columns, unixnow() does not exist (use strftime('%s','now')*1000), and 'time' is a daypart bucket rather than a duration.",
		Annotations: readOnly("Describe the millivolt instance"),
	}, s.describe)

	addTool(server, &sdk.Tool{
		Name:        "query",
		Title:       "Run a SQL SELECT",
		Description: "Run ONE bounded SELECT against the durable database and return the rows. Must start with SELECT, contain no ';', and name no ATTACH/DETACH/PRAGMA/LOAD_EXTENSION. This is the ONLY tool that can express a time range: use WHERE started_at >= strftime('%s','now')*1000 - 3600000 for the last hour, because unixnow() does not exist in this build. There is no cursor, so page in SQL with keyset pagination (WHERE (started_at, id) < (?, ?)) instead of OFFSET. Rows are clamped on BOTH volume and encoded size, with an explicit marker naming what was withheld, so a wide SELECT * is clamped long before the row cap. Returns 503 when durable storage is disabled. This is not a sandbox: it reads the same tables the dashboard reads, including sensitive request_debug.payload bodies.",
		Annotations: readOnly("Run a SQL SELECT"),
	}, s.query)

	addTool(server, &sdk.Tool{
		Name:        "explore",
		Title:       "Break history down by dimension",
		Description: "Faceted breakdown of ALL matching history by ONE dimension (client, provider, model, conversation, key, status, time, tool, error). Each group carries request count, cost, tokens, error and rate-limit counts, blended $/Mtok and TTFT/speed percentiles (null below 4 samples). Scope with the repeated dim:id filters and the status selector; filters are AND-ed across dimensions and OR-ed within one dimension, and an unknown value is an EMPTY breakdown rather than an error, so call the values tool first. TWO structural limits: this tool has NO time window (it always folds ALL history) and it breaks down by a SINGLE dimension, so it cannot cross-tabulate; use query with a WHERE on started_at and a GROUP BY for either. The 24-group cap is enforced SERVER-SIDE, so groups can be dropped while truncation.truncated reads false. Call describe for each dimension's value vocabulary.",
		Annotations: readOnly("Break history down by dimension"),
	}, s.explore)

	addTool(server, &sdk.Tool{
		Name:        "chart",
		Title:       "Read the traffic time series",
		Description: "The only time-windowed tool: a server-authoritative series over a window ('all' or a canonical positive integer number of MINUTES; '007', '+5' and '5.0' are rejected), scoped by the same filters as explore. It cannot group by any dimension, so for a window broken down by provider or model use query with a WHERE on started_at and a GROUP BY. Returns at most 31 clock-aligned buckets with request, error, rate-limit, token, cost, TTFT and speed values, plus period-wide ttft_p/tps_p percentiles and ttft_stat/tps_stat [avg, min, max], computed over the whole period and never averaged from buckets. The 31-bucket cap is enforced SERVER-SIDE, so buckets can be dropped while truncation.truncated reads false.",
		Annotations: readOnly("Read the traffic time series"),
	}, s.chart)

	addTool(server, &sdk.Tool{
		Name:        "records",
		Title:       "Page the durable request log",
		Description: "One page of finalized records, newest first, from durable history, with the raw-to-canonical model_canon map the server applied to the page. Page with the paired before_ms/before_id cursor (both or neither), copying the previous page's next_before_ms/next_before_id pair into them. The server scans a bounded number of rows per call and advances the cursor on every row it SCANNED, including rows the scope filter excluded, so an EMPTY page with an advanced cursor and more true is the normal shape of 'the budget ran out before a match': page again. exhausted is true when the cursor did not advance, and also when more is false: treat it as 'stop paging'. storage reports whether durable storage backs this at all: with storage.enabled false the route scans nothing, so this page is NOT the end of history and the in-memory ring still holds recent records.",
		Annotations: readOnly("Page the durable request log"),
	}, s.records)

	addTool(server, &sdk.Tool{
		Name:        "snapshot",
		Title:       "Read the live snapshot",
		Description: "The dashboard's live snapshot: ring sequence, feed id, in-flight and since-inception totals, the durable-storage signal and the most recent records. A full snapshot is capped at 8 * dash_log_rows records and has no cursor of its own, so older history needs the records tool with its cursor. It reads the in-memory RING, so on a proxy with storage off it is the only source of history at all.",
		Annotations: readOnly("Read the live snapshot"),
	}, s.snapshot)

	addTool(server, &sdk.Tool{
		Name:        "prometheus",
		Title:       "Read the Prometheus exposition",
		Description: "The text/plain Prometheus exposition. It covers the in-memory ring, NOT the durable since-inception totals the chart and explorer use, so the two can legitimately disagree after a restart.",
		Annotations: readOnly("Read the Prometheus exposition"),
	}, s.prometheus)

	addTool(server, &sdk.Tool{
		Name:        "audit_status",
		Title:       "Read the capture state",
		Description: "The debug-capture subsystem's state: live sessions with their scope, deadline and live capture count, the valid client/provider/model scope names, the capture TTL and byte budget, and the durable-storage signal that capture requires. " + retentionNote + ".",
		Annotations: readOnly("Read the capture state"),
	}, s.auditStatus)

	addTool(server, &sdk.Tool{
		Name:        "audit_start",
		Title:       "Start or edit a capture session",
		Description: "Start ONE capture session, or edit an existing one by session_id (omitted scope and duration are then preserved). Scope dimensions are AND-ed and names within one dimension are alternatives; there is no unrestricted all-traffic scope. Every named client, provider and model must ALREADY be in the vocabulary audit_status reports: a session scoped to a name that matches nothing would report enabled:true and capture nothing, so an unknown name is refused with the known values. Requires confirm='start capture' and refuses outright when durable storage is disabled, for the same reason. Capture stores full request and response bodies. " + retentionNote + ".",
		Annotations: mutating("Start or edit a capture session"),
	}, s.auditStart)

	addTool(server, &sdk.Tool{
		Name:        "audit_stop",
		Title:       "Stop a capture session",
		Description: "Stop one capture session by session_id, or every session with stop_all='stop every capture session'. " + retentionNote + ".",
		Annotations: mutating("Stop a capture session"),
	}, s.auditStop)

	addTool(server, &sdk.Tool{
		Name:        "audit_captures_list",
		Title:       "List captured requests",
		Description: "List the requests that have a stored capture document, newest first, paging with the paired before_ms/before_id cursor. The proxy has no capture listing endpoint: this is a bounded SELECT over requests WHERE debug = 1, so it shows stored documents only. Each row's id is what audit_capture_get takes.",
		Annotations: readOnly("List captured requests"),
	}, s.auditCapturesList)

	addTool(server, &sdk.Tool{
		Name:        "audit_capture_get",
		Title:       "Read one captured request",
		Description: "Read one stored capture document by REQUEST RECORD id (from audit_captures_list), not by session id. It contains the request and response headers and bodies, timing, outcome, usage, cost and absorbed attempts. Credentials are redacted from headers; body content is not sanitized, so treat everything returned as sensitive. 404 when the document is absent or its TTL has expired.",
		Annotations: readOnly("Read one captured request"),
	}, s.auditCaptureGet)

	addTool(server, &sdk.Tool{
		Name:        "operator_state",
		Title:       "Read the operator state",
		Description: "Read pause holds, provider limits, storm/quota state and restart status in one call. Use it before any mutation so the change is made against the state that actually exists.",
		Annotations: readOnly("Read the operator state"),
	}, s.operatorState)

	addTool(server, &sdk.Tool{
		Name:        "set_pause",
		Title:       "Pause, edit or resume requests",
		Description: "Park matching NEW requests behind a hold, edit a hold in place by hold_id, or resume (paused=false with no hold_id resumes everything). BLAST RADIUS: a hold with all=true, or a hold that names NO scope at all, is a GLOBAL hold on every client and provider - the proxy treats an omitted scope as all traffic, so leaving every scope field empty is the widest setting this tool has. Narrow it with clients or providers. A hold parks new sends and retries; in-flight requests are never cancelled, and excess waiting work can be refused. Overlapping scopes are rejected rather than silently replaced.",
		Annotations: mutating("Pause, edit or resume requests"),
	}, s.setPause)

	addTool(server, &sdk.Tool{
		Name:        "set_throttle",
		Title:       "Set or clear provider limits",
		Description: setThrottleDescription,
		Annotations: mutating("Set or clear provider limits"),
	}, s.setThrottle)

	addTool(server, &sdk.Tool{
		Name:        "resume_quota",
		Title:       "Close a provider quota gate",
		Description: "Close one provider's open retry-mode quota gate so parked sends resume immediately instead of waiting for the next recovery probe. A manual-mode quota hold resumes through set_pause instead. A provider with no open gate is a no-op.",
		Annotations: mutating("Close a provider quota gate"),
	}, s.resumeQuota)

	addTool(server, &sdk.Tool{
		Name:        "set_config",
		Title:       "Patch the configuration",
		Description: "Apply a revision-checked PARTIAL patch: values carries only the keys to change, and omitted keys are untouched. A supplied list or map replaces that whole field, so call config_get FIRST for structured keys such as providers or model_rules, and pass its revision back. Leave revision empty to use the current one; a stale revision is rejected with 409. The whole result is validated before the file is written, CLI-overridden keys are stripped, and the response names keys that need a restart. The proxy writes the file and only THEN reports a failed reload, so a reload failure comes back as saved=true with the reload error in the output, never as a tool error: the mutation DID succeed and retrying it would fail with 409.",
		Annotations: mutating("Patch the configuration"),
	}, s.setConfig)

	addTool(server, &sdk.Tool{
		Name:        "reload_config",
		Title:       "Re-read the configuration file",
		Description: "Re-read the config file and hot-apply the reloadable subset, reporting the keys that changed but stay at their boot value until a restart. This is the only state-changing route the dashboard never calls. The operator token itself is process-bound and is not re-read.",
		Annotations: mutating("Re-read the configuration file"),
	}, s.reloadConfig)

	addTool(server, &sdk.Tool{
		Name:        "config_get",
		Title:       "Read the configuration",
		Description: "The live configuration document: the file's values, the values actually in force after CLI overrides, the built-in defaults, the per-key schema (type, category and documentation), the keys pinned outside the file by the process (overrides, which set_config strips), the canonical usage and model metadata field names, the restart-required set, whether the file is writable at all, and the revision a revision-checked set_config must echo. Call this before set_config: a supplied list or map replaces that whole field, so a structured key patched without reading it first is replaced wholesale.",
		Annotations: readOnly("Read the configuration"),
	}, s.configGet)

	addTool(server, &sdk.Tool{
		Name:        "values",
		Title:       "List a dimension's values",
		Description: "The distinct values one filter dimension actually has in durable history, most frequent first, with a request count each: client, provider, model, conversation, key, status, tool, error_type, error_code or error_message. Call it before filtering: an unknown value is an EMPTY result from explore, chart and records rather than an error, so a guess turns a correctable mistake into a confidently wrong answer. Two dimensions are deliberately absent: 'time' is a proxy-side daypart bucket and 'error' a proxy-side type|code|message key, neither of which is a stored column, so describe lists their value spaces instead. For a count of one dimension under a filter on another, use query with a GROUP BY.",
		Annotations: readOnly("List a dimension's values"),
	}, s.values)

	addTool(server, &sdk.Tool{
		Name:        "purge_preview",
		Title:       "Preview a history deletion",
		Description: "Count the records a filter would delete, without deleting anything, and issue the preview_token that authorizes exactly that deletion. The filter must constrain at least one field. Run this before purge and pass its preview_token back verbatim: the token is bound to THIS filter, THIS count and a short expiry, so it cannot authorize a different filter that happens to match the same number of rows. Residual gap, stated plainly: the count is re-checked immediately before the deletion, but the re-check and the delete are two separate requests with no shared transaction, so live traffic committed between them is deleted without having been previewed. A filter that can reach a captured request DELETES that request's stored capture document too.",
		Annotations: readOnly("Preview a history deletion"),
	}, s.purgePreview)

	addTool(server, &sdk.Tool{
		Name:        "purge",
		Title:       "Delete history permanently",
		Description: "PERMANENTLY DELETE matching request history. This is irreversible and the proxy has NO server-side confirmation, NO confirm flag and NO two-step protocol: the credential and this tool's guards are the whole gate. Three guards apply together, and each alone is insufficient. (1) The filter must constrain at least one field; the bodyless command that deletes ALL history is never sent by this tool. (2) confirmation must be the exact phrase '" + PurgeConfirmation + "'. (3) preview_token must be the token purge_preview issued for THIS EXACT filter; a bare count is not enough, because two different filters can match the same number of rows. What the token does NOT prevent, stated plainly: the count is re-checked immediately before deleting, and rows committed between that re-check and the delete are removed un-previewed, because the two are separate requests with no shared transaction. Deleting a request also DELETES its stored debug capture document. Deleted history cannot be recovered from this API. " + purgeNote + ".",
		Annotations: destructive("Delete history permanently"),
	}, s.purge)
}

func boolPtr(value bool) *bool { return &value }
