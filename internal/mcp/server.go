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
	return &Service{client: client, limits: limits}, nil
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
		Description: "Start here. Returns the live SQLite schema, the requests/request_debug/meta column reference with units and meaning, the exact SQL health predicates (a 429 alone is not an error), the explorer dimension and status vocabularies, the live known client/provider/model names, the indexes, and every result limit. Without this you cannot write correct SQL: timestamps are Unix milliseconds, cost is USD, and attempts/tool_names are JSON-array text columns.",
		Annotations: readOnly("Describe the millivolt instance"),
	}, s.describe)

	addTool(server, &sdk.Tool{
		Name:        "query",
		Title:       "Run a SQL SELECT",
		Description: "Run ONE bounded SELECT against the durable database and return the rows. Must start with SELECT, contain no ';', and name no ATTACH/DETACH/PRAGMA/LOAD_EXTENSION. Use keyset pagination (WHERE (started_at, id) < (?, ?)) instead of OFFSET for deep history. Rows are truncated with an explicit marker when they exceed max_rows. Returns 503 when durable storage is disabled. This is not a sandbox: it reads the same tables the dashboard reads, including sensitive request_debug.payload bodies.",
		Annotations: readOnly("Run a SQL SELECT"),
	}, s.query)

	addTool(server, &sdk.Tool{
		Name:        "explore",
		Title:       "Break history down by dimension",
		Description: "Faceted breakdown of ALL matching history by one dimension (client, provider, model, conversation, key, status, time, tool, error). Each group carries request count, cost, tokens, error and rate-limit counts, blended $/Mtok and TTFT/speed percentiles (null below 4 samples). Scope with the repeated dim:id filters and the status selector; filters are AND-ed across dimensions and OR-ed within one dimension. Returns at most 24 groups.",
		Annotations: readOnly("Break history down by dimension"),
	}, s.explore)

	addTool(server, &sdk.Tool{
		Name:        "chart",
		Title:       "Read the traffic time series",
		Description: "Server-authoritative time series over a window ('all' or a canonical positive integer number of minutes; '007', '+5' and '5.0' are rejected), scoped by the same filters as explore. Returns at most 31 clock-aligned buckets with request, error, rate-limit, token, cost, TTFT and speed values. Period percentiles are computed over the whole period, never averaged from bucket percentiles.",
		Annotations: readOnly("Read the traffic time series"),
	}, s.chart)

	addTool(server, &sdk.Tool{
		Name:        "records",
		Title:       "Page the durable request log",
		Description: "One page of finalized records, newest first, from durable history. Page with the paired next_before_ms/next_before_id cursor (both or neither). The server scans a bounded number of rows per call, so a page can be short or empty while more is still true: a non-advancing cursor means exhaustion. Stop when exhausted is true instead of looping.",
		Annotations: readOnly("Page the durable request log"),
	}, s.records)

	addTool(server, &sdk.Tool{
		Name:        "snapshot",
		Title:       "Read the live snapshot",
		Description: "The dashboard's live snapshot: ring sequence, feed id, in-flight and since-inception totals, the durable-storage signal and the most recent records. A full snapshot is capped at 8 * dash_log_rows records, so older history needs the records tool with its cursor.",
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
		Description: "Start ONE capture session, or edit an existing one by session_id (omitted scope and duration are then preserved). Scope dimensions are AND-ed and names within one dimension are alternatives; there is no unrestricted all-traffic scope. Requires confirm='start capture' and refuses outright when durable storage is disabled, because a session would then report success and capture nothing. Capture stores full request and response bodies. " + retentionNote + ".",
		Annotations: mutating("Start or edit a capture session"),
	}, s.auditStart)

	addTool(server, &sdk.Tool{
		Name:        "audit_stop",
		Title:       "Stop a capture session",
		Description: "Stop one capture session by session_id, or every session with stop_all='stop every capture session'. " + retentionNote + "; there is no delete endpoint.",
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
		Description: "Park matching NEW requests behind a hold, edit a hold in place by hold_id, or resume (paused=false with no hold_id resumes everything). A hold parks new sends and retries; in-flight requests are never cancelled. Overlapping scopes are rejected rather than silently replaced.",
		Annotations: mutating("Pause, edit or resume requests"),
	}, s.setPause)

	addTool(server, &sdk.Tool{
		Name:        "set_throttle",
		Title:       "Set or clear provider limits",
		Description: "Set one provider's budgets across all its clients and keys: concurrency, requests per window and tokens per window. Supplied dimensions merge with the current ones; 0 disables a dimension; clear removes the whole policy. These permit bursts and oversized requests; they are not strict fixed-window quota enforcement.",
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
		Description: "Apply a revision-checked PARTIAL patch: values carries only the keys to change, and omitted keys are untouched. A supplied list or map replaces that whole field, so read the current document first for structured keys such as providers or model_rules. Leave revision empty to use the current one; a stale revision is rejected with 409. The whole result is validated before the file is written, CLI-overridden keys are stripped, and the response names keys that need a restart. A failed reload still leaves the file written, which the output reports explicitly.",
		Annotations: mutating("Patch the configuration"),
	}, s.setConfig)

	addTool(server, &sdk.Tool{
		Name:        "reload_config",
		Title:       "Re-read the configuration file",
		Description: "Re-read the config file and hot-apply the reloadable subset, reporting the keys that changed but stay at their boot value until a restart. This is the only state-changing route the dashboard never calls. The operator token itself is process-bound and is not re-read.",
		Annotations: mutating("Re-read the configuration file"),
	}, s.reloadConfig)

	addTool(server, &sdk.Tool{
		Name:        "purge_preview",
		Title:       "Preview a history deletion",
		Description: "Count the records a filter would delete, without deleting anything. The filter must constrain at least one field. Run this before purge and pass the count back as reviewed_count: traffic can change the row set between the preview and the deletion, so only the filter is fixed.",
		Annotations: readOnly("Preview a history deletion"),
	}, s.purgePreview)

	addTool(server, &sdk.Tool{
		Name:        "purge",
		Title:       "Delete history permanently",
		Description: "PERMANENTLY DELETE matching request history. This is irreversible and the proxy has NO server-side confirmation, NO confirm flag and NO two-step protocol: the credential and this tool's guards are the whole gate. Three guards apply together, and each alone is insufficient. (1) The filter must constrain at least one field; the bodyless command that deletes ALL history is never sent by this tool. (2) confirmation must be the exact phrase 'permanently delete the matching millivolt history'. (3) reviewed_count must equal a purge_preview count for the SAME filter, re-checked immediately before deleting, so a deletion cannot be authorized against a preview of a different row set. Deleted history cannot be recovered from this API. " + purgeNote + ".",
		Annotations: destructive("Delete history permanently"),
	}, s.purge)
}

func boolPtr(value bool) *bool { return &value }
