package config

import (
	"time"
)

// Config holds every server-level setting. Providers are never configured
// here: the client app describes the upstream per-request via headers.
//
// Single source of truth for defaults: Default() is the only place a default
// is defined. LoadFile merges a (possibly partial) YAML file over those
// defaults, so a config file only needs to set the keys it overrides. No
// configurable may be hardcoded anywhere else in the codebase - if a value is
// user-tunable, it belongs here.
type Config struct {
	// Listen is the address the HTTP server binds (e.g. ":8080").
	Listen string `yaml:"listen" json:"listen"`
	// DBPath is the durable SQLite metrics store. Empty disables durability
	// (in-memory ring buffer only).
	DBPath string `yaml:"db_path" json:"db_path"`

	// HistorySize is the in-memory ring buffer depth and the number of records
	// backfilled from storage on startup. Durable history and log paging can
	// extend beyond this recent-record window.
	HistorySize int `yaml:"history_size" json:"history_size"`

	// ShutdownTimeout bounds HTTP shutdown after active request contexts are
	// canceled. Storage draining follows separately, outside this HTTP timer.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`

	// RestartDrainTimeout bounds the drain phase of a UI-triggered restart
	// (/admin/restart): how long the old process waits for in-flight streams
	// to finish. Expiry aborts the restart and resumes accepting traffic;
	// unfinished streams are never killed to force a binary upgrade.
	// Zero waits indefinitely.
	RestartDrainTimeout time.Duration `yaml:"restart_drain_timeout" json:"restart_drain_timeout"`

	// MemoryLimit is a soft cap on the Go runtime's total managed memory
	// (heap, stacks, runtime structures), applied once at startup through
	// the runtime's memory-limit mechanism. Zero auto-detects a finite cgroup
	// memory ceiling (80% of it) when GOMEMLIMIT is unset, otherwise leaves
	// the runtime unbounded; a set value overrides the environment for the
	// process lifetime. The
	// limit is soft: exceeding it makes the collector more aggressive
	// (capped near 50% of CPU time) instead of failing requests. It bounds
	// Go-managed memory only, never SQLite's page cache or OS overhead.
	// Restart required.
	MemoryLimit ByteSize `yaml:"go_memory_limit" json:"go_memory_limit"`

	// MaxRequestBytes caps the size of an inbound request body that the proxy
	// will buffer to discover routing metadata. Larger bodies are rejected.
	MaxRequestBytes ByteSize `yaml:"max_request_bytes" json:"max_request_bytes"`

	// UpstreamTimeout is the maximum time to wait for an upstream response's
	// headers. Streaming bodies are not subject to this timeout (only header
	// arrival), so long generations are unaffected. Zero disables the cap.
	// yaml:"-" because a YAML integer would decode as nanoseconds on
	// time.Duration. LoadFile probes the scalar as text so duration tokens
	// ("0s", "5m") parse, then mergeOverlay copies it when the key is present.
	UpstreamTimeout time.Duration `yaml:"-" json:"upstream_timeout"`

	// Model discovery has one aggregate upstream budget, independent of LLM
	// header/stream timeouts. It includes pages, unary RPCs and enrichment;
	// limits are captured once per discovery and hot-reload for new requests.
	ModelsDiscoveryTimeout  time.Duration `yaml:"models_discovery_timeout" json:"models_discovery_timeout"`
	ModelsDiscoveryMaxBytes ByteSize      `yaml:"models_discovery_max_bytes" json:"models_discovery_max_bytes"`
	ModelsDiscoveryMaxPages int           `yaml:"models_discovery_max_pages" json:"models_discovery_max_pages"`

	// AllowedBaseURLs is an optional allowlist of upstream base URL prefixes.
	// When non-empty, only targets matching one of these prefixes are accepted;
	// this mitigates SSRF if the proxy is reachable by untrusted clients. Empty
	// (default) allows any target, appropriate for a single trusted operator.
	// Each entry must be an absolute http:// or https:// URL with a non-empty
	// host (validated on load): the runtime matcher is a plain prefix match,
	// so a scheme-less typo like "api.openai.com" would load cleanly and then
	// silently deny every request.
	AllowedBaseURLs []string `yaml:"allowed_base_urls" json:"allowed_base_urls"`

	// CaptureBodyPreview enables storing a short preview of the last user
	// prompt and (for non-streaming) the response text in the metrics record.
	// Off by default to avoid persisting content.
	CaptureBodyPreview bool `yaml:"capture_body_preview" json:"capture_body_preview"`

	// DebugCaptureTTL is how long a full debug capture (request/response
	// bodies and headers) is kept after the request finishes. Operator debug
	// sessions (dashboard Debug ▾) choose which traffic is captured; this
	// TTL is the retention of those blobs, independent of session lifetime.
	DebugCaptureTTL time.Duration `yaml:"debug_capture_ttl" json:"debug_capture_ttl"`

	// DebugCaptureMaxBytes caps one debug capture (request body + client-facing
	// response bytes). Past the cap the rest is dropped and truncated=true.
	DebugCaptureMaxBytes ByteSize `yaml:"debug_capture_max_bytes" json:"debug_capture_max_bytes"`
	// DebugStoreMaxBytes caps the total durable debug-capture sidecar. New
	// captures are skipped once the sidecar would exceed it; TTL expiry frees
	// room. 0 = unlimited. Restart required (the counter is seeded at boot).
	DebugStoreMaxBytes ByteSize `yaml:"debug_store_max_bytes" json:"debug_store_max_bytes"`

	// AutoTokenRefresh enables stateless expired-token refresh: when a client
	// presents a JWT access token whose exp has passed (within a small
	// margin) plus its own refresh token in X-Proxy-Refresh-Token, the proxy
	// exchanges it at the provider's refresh endpoint. Every supported mechanism
	// uses handback on inference: HTTP 401 with code "token_expired" and the
	// new access_token in the error body, plus refresh_token only when the
	// exchange returns a different replacement.
	// No inference is sent; the client adopts the credentials and retries.
	// Model discovery is exempt and refreshes transparently without returning
	// tokens. The proxy retains no reusable credentials. Hot-reloads.
	AutoTokenRefresh bool `yaml:"auto_token_refresh" json:"auto_token_refresh"`

	// ---- upstream connection pool ----
	// Long-lived SSE streams hold a connection for the whole generation, so the
	// pool is sized for concurrent streams, not requests/sec.
	MaxConnsPerHost     int           `yaml:"max_conns_per_host" json:"max_conns_per_host"`
	MaxIdleConns        int           `yaml:"max_idle_conns" json:"max_idle_conns"`
	MaxIdleConnsPerHost int           `yaml:"max_idle_conns_per_host" json:"max_idle_conns_per_host"`
	IdleConnTimeout     time.Duration `yaml:"idle_conn_timeout" json:"idle_conn_timeout"`

	// UpstreamDialTimeout bounds establishing a new upstream connection.
	// UpstreamDialKeepAlive is the TCP keepalive interval for those
	// connections. UpstreamTLSHandshakeTimeout bounds the TLS handshake.
	// All three apply to new connections; reload swaps the transport without
	// disturbing in-flight streams.
	UpstreamDialTimeout         time.Duration `yaml:"upstream_dial_timeout" json:"upstream_dial_timeout"`
	UpstreamDialKeepAlive       time.Duration `yaml:"upstream_dial_keepalive" json:"upstream_dial_keepalive"`
	UpstreamTLSHandshakeTimeout time.Duration `yaml:"upstream_tls_handshake_timeout" json:"upstream_tls_handshake_timeout"`

	// UpstreamStreamIdleTimeout bounds silence inside an upstream response
	// body. It releases the handler, upstream connection and scheduler slot
	// when a gateway returns 200 and then stops sending. New response bodies
	// use the reloaded value; in-flight bodies keep the value they started
	// with.
	UpstreamStreamIdleTimeout time.Duration `yaml:"upstream_stream_idle_timeout" json:"upstream_stream_idle_timeout"`

	// WholeDocumentReadTimeout is the fallback bound for one whole-document
	// upstream read when the caller supplied no deadline. WholeDocumentReadMax
	// is the ceiling for caller deadlines and must cover both the fallback and
	// the configured model-discovery budget. These apply only to buffered
	// reads, never to streaming SSE.
	WholeDocumentReadTimeout time.Duration `yaml:"whole_document_read_timeout" json:"whole_document_read_timeout"`
	WholeDocumentReadMax     time.Duration `yaml:"whole_document_read_max" json:"whole_document_read_max"`

	// ---- queueing / rate-limit handling ----
	// The proxy transparently queues requests when a provider returns 429, and
	// transparently retries 429 and any 5xx (transient upstream errors) with
	// exponential backoff - Retry-After / rate-limit reset headers are a
	// floor, not a replacement - so clients never see a transient failure.
	// A 429 carrying a durable quota/billing
	// error (insufficient_quota/credits, spend limits) is never retried
	// blindly: quota_pause_mode off surfaces it immediately (waiting cannot
	// clear it); the other modes park it behind a provider-wide pause instead.
	MaxConcurrent int           `yaml:"max_concurrent" json:"max_concurrent"` // per provider+key; 0 = unlimited
	MaxQueueSize  int           `yaml:"max_queue_size" json:"max_queue_size"` // per group; 0 = unlimited
	MaxQueueWait  time.Duration `yaml:"max_queue_wait" json:"max_queue_wait"` // max queue wait before 429; 0 = unlimited
	MaxRetries    int           `yaml:"max_retries" json:"max_retries"`       // max transient (429/5xx) retries
	// MaxInflightRequests is the process-wide inference admission bound. Every
	// accepted request buffers its body before routing, so unbounded handler
	// concurrency lets clients control heap directly. Requests above the bound
	// are answered 503 with Retry-After instead of buffering. 0 = unlimited.
	MaxInflightRequests int `yaml:"max_inflight_requests" json:"max_inflight_requests"`
	// MaxInflightBodyBytes is the process-wide budget for buffered inbound
	// bodies. Admission reserves the declared Content-Length (or the configured
	// request cap for chunked bodies) and refuses a request that would exceed
	// this budget, before reading it. 0 = unlimited.
	MaxInflightBodyBytes ByteSize `yaml:"max_inflight_body_bytes" json:"max_inflight_body_bytes"`
	// MaxInflightResponseBytes is the process-wide budget for whole-document
	// upstream response buffers: translated non-streaming documents, model
	// discovery bodies, and the non-streaming 200 quality spool. The
	// reservation covers the bytes a bounded read can hold, is held only while
	// buffering, and is released after the buffer is written, translated or
	// decoded, on failure, or when an attempt is abandoned for a retry. A
	// request above the remaining budget is refused with 503 before reading the
	// upstream body. 0 = unlimited.
	MaxInflightResponseBytes ByteSize `yaml:"max_inflight_response_bytes" json:"max_inflight_response_bytes"`
	// QueueRetryAfter is the Retry-After hint sent to clients when the proxy's
	// own queue is full or the wait is exceeded. HTTP Retry-After is integer
	// seconds; RetryAfterSeconds is the single conversion for that header.
	QueueRetryAfter time.Duration `yaml:"queue_retry_after" json:"queue_retry_after"`

	// Retry backoff for transient failures (429/5xx/transport). BaseBackoff is
	// the initial adaptive delay; it doubles each attempt and each consecutive
	// failed request, up to MaxBackoff. A provider Retry-After / rate-limit-reset
	// is a floor: never retry sooner than the hint, but a short hint (HTTP
	// Retry-After is integer seconds, so 1s is common on 503 "retry shortly")
	// does not reset or replace the doubling. MaxBackoff does not clamp a
	// long provider hint (daily limits are often 30-60m). Generous defaults:
	// these are upstream-recovery pauses, and too-short backoff just
	// re-hammers a struggling provider.
	BaseBackoff time.Duration `yaml:"base_backoff" json:"base_backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff" json:"max_backoff"`

	// QualityRetries bounds the transparent re-attempts of a degenerate 200
	// (an empty completion or tool_calls with zero tool calls), a truncated
	// stream that relayed no generation content, a retryable in-band provider
	// error dropped before the wire, and the cursor one-shot fresh-user
	// re-ask: the non-streaming pre-write retry, the streaming re-send on the
	// committed SSE connection, and the cursor re-ask. Each re-attempt
	// re-sends the full request, so a small budget is the sane ceiling;
	// 0 disables quality handling entirely.
	QualityRetries int `yaml:"quality_retries" json:"quality_retries"`

	// ThinkingRetries bounds the transparent rescue of requests that die or
	// end without an answer during the thinking/reasoning phase: a stream
	// truncated or reset after reasoning-only output, a cleanly finished
	// reasoning-only completion (reasoning but no answer, no tool call), a
	// retryable in-band provider error that arrived after reasoning-only
	// output, and a non-streaming reasoning-only body. The fresh attempt
	// appends to the committed SSE connection after the already-relayed
	// reasoning (auxiliary display text, never the completion contract) -
	// never after answer content or tool calls, which are never rescuable.
	// finish_reason length is the client's own token cap and is never
	// rescued. The generic OpenAI-compatible relay only; the Cursor bridge
	// and translated Anthropic streams keep their own signaling. Each rescue
	// re-sends the full request (the upstream bills every attempt); 0
	// disables.
	ThinkingRetries int `yaml:"thinking_retries" json:"thinking_retries"`

	// RetryableErrorClasses extends the built-in retryable in-band error
	// vocabulary (OpenAI server_error / service_unavailable family,
	// api_error, overloaded_error, timeout_error, engine_overloaded and the
	// common gateway spellings) with operator-supplied type or code strings:
	// a gateway that reports a transient failure under a custom spelling can
	// be re-sent without a code change. The built-in engine_overloaded
	// capacity code also overrides the generic invalid_request_error
	// wrapper. Matched case-insensitively against the parsed error
	// envelope's type and code fields, never message text. The list is
	// authoritative over the documented client-fault types (a gateway that
	// mislabels a transient failure as invalid_request_error can be opted
	// back in); only durable quota/billing classes are absolutely excluded -
	// validation rejects them at load. Reload applies to new rescue
	// decisions.
	RetryableErrorClasses []string `yaml:"retryable_error_classes" json:"retryable_error_classes"`

	// SuppressClientRetries makes the proxy the clients' sole retry
	// authority: while enabled, every error response the LLM relay returns
	// carries `x-should-retry: false` - the official OpenAI SDK convention
	// (python, node, go, ruby all honor it from source), which overrides the
	// SDKs' 408/409/429/5xx auto-retry defaults and any Retry-After,
	// including provider hints the relay would otherwise forward verbatim.
	// The proxy's own retry ladder has already run by the time the client
	// sees the error, so a client-side retry only duplicates upstream work
	// and billing. No response exists to carry the directive on
	// transport-level failures, and clients that ignore the header (e.g.
	// the Vercel AI SDK / opencode stack, whose retry policy is status- and
	// body-text driven) keep their own policy; mid-stream SSE errors never
	// auto-retry in official SDKs and cannot carry new headers. Success
	// responses are untouched. Reload applies to new responses.
	SuppressClientRetries bool `yaml:"suppress_client_retries" json:"suppress_client_retries"`

	// Error storm protection observes eligible upstream attempts in a bounded
	// rolling window and gates new sends by provider or exact recorded model.
	// Policy changes reset observations and wake waiters; banner visibility
	// changes presentation only. All fields hot-reload.
	StormEnabled           bool          `yaml:"storm_enabled" json:"storm_enabled"`
	StormProviderEnabled   bool          `yaml:"storm_provider_enabled" json:"storm_provider_enabled"`
	StormModelEnabled      bool          `yaml:"storm_model_enabled" json:"storm_model_enabled"`
	StormBannerEnabled     bool          `yaml:"storm_banner_enabled" json:"storm_banner_enabled"`
	StormWindow            time.Duration `yaml:"storm_window" json:"storm_window"`
	StormMinSamples        int           `yaml:"storm_min_samples" json:"storm_min_samples"`
	StormErrorPercent      int           `yaml:"storm_error_percent" json:"storm_error_percent"`
	StormInitialBackoff    time.Duration `yaml:"storm_initial_backoff" json:"storm_initial_backoff"`
	StormMaxBackoff        time.Duration `yaml:"storm_max_backoff" json:"storm_max_backoff"`
	StormBackoffMultiplier int           `yaml:"storm_backoff_multiplier" json:"storm_backoff_multiplier"`
	StormJitterPercent     int           `yaml:"storm_jitter_percent" json:"storm_jitter_percent"`
	StormRecoverySuccesses int           `yaml:"storm_recovery_successes" json:"storm_recovery_successes"`
	StormMaxQueue          int           `yaml:"storm_max_queue" json:"storm_max_queue"`
	// StormMaxWait bounds each storm gate wait, not total request duration.
	// A caller's cancellation can end a wait earlier.
	StormMaxWait   time.Duration `yaml:"storm_max_wait" json:"storm_max_wait"`
	StormMaxScopes int           `yaml:"storm_max_scopes" json:"storm_max_scopes"`
	// StormMaxRetries adds to MaxRetries only for an eligible transient
	// failure while the request's provider or exact model storm is active.
	StormMaxRetries int `yaml:"storm_max_retries" json:"storm_max_retries"`
	// HTTP triggers are exact status strings. An empty list disables them;
	// durable quota/billing 429s are excluded even when "429" is selected.
	StormStatusCodes     []string `yaml:"storm_status_codes" json:"storm_status_codes"`
	StormTransportErrors bool     `yaml:"storm_transport_errors" json:"storm_transport_errors"`
	// Completed in-band/stream/quality errors inform later traffic only;
	// meaningful emitted upstream content is never replayed for storm recovery.
	StormStreamErrors bool `yaml:"storm_stream_errors" json:"storm_stream_errors"`

	// ---- durable quota/billing pause ----
	// QuotaPauseMode selects the provider-wide reaction to a 429 carrying a
	// durable quota/billing error (insufficient_quota/credits, spend limits;
	// the classification owner is metrics.IsNonRetryableQuotaErr):
	//   off    surface the 429 immediately; nothing is parked or retried
	//          (waiting cannot clear the condition on its own).
	//   retry  open a provider-wide recovery gate: the triggering request
	//          absorbs the 429 and re-sends per the storm recovery backoff
	//          (storm_initial_backoff..storm_max_backoff), every new request
	//          to the provider parks on the gate (bounded by storm_max_queue
	//          and storm_max_wait), and the first send that resolves 2xx
	//          closes the gate and drains the queue.
	//   manual create an indefinite provider-scoped pause (an operator hold)
	//          that queues every request, triggering one included, until an
	//          operator resumes it through the pause surface.
	// Hot-reload applies to new observations. A mode switch drops in-memory
	// retry gates (they self-heal on the next durable 429) but never removes
	// operator holds, including manual-mode ones.
	QuotaPauseMode string `yaml:"quota_pause_mode" json:"quota_pause_mode"`
	// QuotaPauseRecoverySuccesses is how many consecutive successful recovery
	// probes close a retry-mode quota gate; 1 releases the queue on the first
	// 2xx. Meaningful only for quota_pause_mode retry.
	QuotaPauseRecoverySuccesses int `yaml:"quota_pause_recovery_successes" json:"quota_pause_recovery_successes"`

	// ---- conversation grouping (dashboard request log) ----
	// ConversationIdleGap: a gap in activity longer than this starts a new
	// conversation for the same client+key. Generous so thinking pauses inside
	// one task don't split it.
	ConversationIdleGap time.Duration `yaml:"conversation_idle_gap" json:"conversation_idle_gap"`
	// ConversationMaxOpen caps concurrently-open conversations per client+key;
	// the oldest is evicted past it. Bounds tracker memory.
	ConversationMaxOpen int `yaml:"conversation_max_open" json:"conversation_max_open"`

	// ---- format translation ----
	// AnthropicDefaultMaxTokens is injected into translated Anthropic requests
	// when the client sent none of max_tokens, max_completion_tokens or
	// max_output_tokens (Anthropic requires max_tokens).
	AnthropicDefaultMaxTokens int `yaml:"anthropic_default_max_tokens" json:"anthropic_default_max_tokens"`
	// TranslationLoss selects what a cross-protocol translation does when the
	// target cannot represent a client field: warn (default) drops optional
	// fields and reports them in X-Proxy-Warnings; reject fails the request
	// with 400 naming the fields before any upstream send. Reject refuses
	// every recorded loss, including a documented approximation such as a
	// clamped temperature. Structural features are rejected in both modes
	// when they cannot be honored. A response-side loss is never fatal
	// because the status line may already be committed: a non-streaming
	// response reports it in X-Proxy-Warnings, and a streaming response
	// drops it without a header. Hot-reloads.
	TranslationLoss string `yaml:"translation_loss" json:"translation_loss"`
	// CursorDefaultContextWindow is an optional context-window hint (tokens)
	// stamped onto every model in the GET /v1/models listing when X-Proxy-Format
	// is cursor. Cursor's agent.v1 GetUsableModels does not expose per-model
	// context windows, so this single operator-set value is the only source
	// (never a hardcoded per-model table). 0 (default) omits the field, letting
	// the client use its own default. Hot-reloads.
	CursorDefaultContextWindow int `yaml:"cursor_default_context_window" json:"cursor_default_context_window"`
	// CursorParkTTL is how long a parked Cursor Run stream stays open waiting
	// for tool results before the proxy closes it. Longer keeps the in-band
	// resume (the same upstream stream - the highest-fidelity path, no cold
	// start) available for sub-agents that run long. Hot-reloads.
	CursorParkTTL time.Duration `yaml:"cursor_park_ttl" json:"cursor_park_ttl"`
	// CursorHeartbeatInterval is how often the proxy sends a ClientHeartbeat on
	// an agent.v1 run - idle parked runs included - to keep the upstream turn
	// alive. Field evidence: even at 5s pings, Cursor's server drops idle
	// parked streams at minute scale, so tuning this bounds (never eliminates)
	// park loss. Read per-run at creation, so a reload applies to new runs.
	CursorHeartbeatInterval time.Duration `yaml:"cursor_heartbeat_interval" json:"cursor_heartbeat_interval"`

	// ---- durable storage pipeline ----
	StorageWriteChanCap  int           `yaml:"storage_write_chan_cap" json:"storage_write_chan_cap"`   // buffered records before overflow drops
	StorageBatchCap      int           `yaml:"storage_batch_cap" json:"storage_batch_cap"`             // max records per SQLite transaction
	StorageFlushInterval time.Duration `yaml:"storage_flush_interval" json:"storage_flush_interval"`   // partial-batch flush cadence
	StorageQueryTimeout  time.Duration `yaml:"storage_query_timeout" json:"storage_query_timeout"`     // bounds dashboard SELECT queries
	StorageQueryMaxBytes ByteSize      `yaml:"storage_query_max_bytes" json:"storage_query_max_bytes"` // ad-hoc SELECT JSON result budget; restart required
	StorageQueryMaxRows  int           `yaml:"storage_query_max_rows" json:"storage_query_max_rows"`   // ad-hoc SELECT row budget; restart required
	// BackupMaxBytes caps one Settings backup download or restore upload.
	// Downloads and restores stream through private staging files; this is the
	// admit/reject ceiling, not a heap guarantee.
	BackupMaxBytes ByteSize `yaml:"backup_max_bytes" json:"backup_max_bytes"`

	// ---- MCP endpoint ----
	// The operator-gated /mcp LLM tool endpoint. mcp_enabled registers the
	// route at boot or keeps it reserved for the 404 handler; the limits are
	// the result-size policy its tools clamp with (mcp.LimitsFromConfig is
	// the mapping). Restart required: the route table and the endpoint's
	// services are built once from the loaded config.
	MCPEnabled         bool          `yaml:"mcp_enabled" json:"mcp_enabled"`
	MCPPageSize        int           `yaml:"mcp_page_size" json:"mcp_page_size"`
	MCPQueryMaxRows    int           `yaml:"mcp_query_max_rows" json:"mcp_query_max_rows"`
	MCPQueryMaxBytes   ByteSize      `yaml:"mcp_query_max_bytes" json:"mcp_query_max_bytes"`
	MCPCaptureMaxBytes ByteSize      `yaml:"mcp_capture_max_bytes" json:"mcp_capture_max_bytes"`
	MCPQueryTimeout    time.Duration `yaml:"mcp_query_timeout" json:"mcp_query_timeout"`
	MCPTimeout         time.Duration `yaml:"mcp_timeout" json:"mcp_timeout"`

	// ---- HTTP server timeouts ----
	// ReadHeaderTimeout guards against slowloris; IdleTimeout bounds keep-alive
	// idle connections. There is deliberately no WriteTimeout: it would kill
	// long-lived SSE streams.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" json:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout" json:"idle_timeout"`

	// RequestBodyIdleTimeout bounds one inbound body read. It is re-armed
	// before every read and cleared at EOF or Close, so it cannot expire while
	// the handler is still streaming the response. RequestBodyReadTimeout is
	// the absolute bound for the whole body; the idle bound must not exceed
	// it. ResponseWriteTimeout is the generic per-write bound for buffered
	// responses; streaming owners may arm a shorter explicit bound.
	RequestBodyIdleTimeout time.Duration `yaml:"request_body_idle_timeout" json:"request_body_idle_timeout"`
	RequestBodyReadTimeout time.Duration `yaml:"request_body_read_timeout" json:"request_body_read_timeout"`
	ResponseWriteTimeout   time.Duration `yaml:"response_write_timeout" json:"response_write_timeout"`

	// SSEKeepaliveInterval is how often a quiet streaming socket gets an SSE
	// comment (`: keepalive`) so proxies and the client do not drop the
	// connection during queue wait, operator hold, or mid-stream gaps. Chat
	// Completions clients ignore SSE comments. Hot-reloads (new streams;
	// in-flight pacers keep the interval they started with).
	SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval" json:"sse_keepalive_interval"`
	// SSEWriteProgressTimeout bounds one client-facing SSE write. A stalled
	// reader fails that write instead of pinning shutdown. New streams use the
	// reloaded value; in-flight pacers keep the value they started with. It
	// must not exceed response_write_timeout.
	SSEWriteProgressTimeout time.Duration `yaml:"sse_write_progress_timeout" json:"sse_write_progress_timeout"`

	// ---- dashboard ----
	// DashLogRows is how many of the most-recent in-scope records the request
	// log renders. The ring itself is HistorySize; this is the visible window.
	// Hot-reloads (the next dashboard fetch/render picks it up).
	DashLogRows int `yaml:"dash_log_rows" json:"dash_log_rows"`
	// DashPollInterval is the KPI / pause / SSE-fallback poll cadence.
	// Hot-reloads.
	DashPollInterval time.Duration `yaml:"dash_poll_interval" json:"dash_poll_interval"`
	// DashChartRefresh is how often the traffic chart re-fetches authoritative
	// server buckets and period totals. Hot-reloads.
	DashChartRefresh time.Duration `yaml:"dash_chart_refresh" json:"dash_chart_refresh"`
	// DashExplorerStale is how old a cached explorer breakdown may be before
	// the dashboard re-fetches it. Hot-reloads.
	DashExplorerStale time.Duration `yaml:"dash_explorer_stale" json:"dash_explorer_stale"`
	// DashBackgroundRefresh stops the dashboard's own teardown while its tab
	// is hidden: the poll keeps running at the browser's throttled background
	// cadence (about one wake per minute on Chromium) and a Web Lock - the
	// documented Chromium freeze exemption - keeps the SSE feed from being
	// frozen. Android and iOS still suspend background pages at the OS level;
	// the flag cannot and does not override that. Hot-reloads.
	DashBackgroundRefresh bool `yaml:"dash_background_refresh" json:"dash_background_refresh"`

	// Providers, keyed by an X-Proxy-* base-URL-derived provider label, hold
	// optional per-provider overrides for how to read usage/cost/cache fields
	// from that provider's responses (they mostly all report usage+cost back,
	// but under different key names). See ProviderOverride. Keys are the
	// canonical lowercase host-derived provider labels; a padded or mixed-case
	// key can never match a request and is rejected at load.
	Providers map[string]ProviderOverride `yaml:"providers" json:"providers"`

	// ProviderAliases merges provider labels: an old label recorded under a
	// previous naming scheme is rewritten to its canonical label at capture
	// time and in existing stored history (boot + reload). Both sides are
	// canonicalized to the lowercase host-derived label at the config boundary.
	// Already-buffered records keep their prior label until age-out/restart;
	// removing a mapping cannot recover rewritten spellings. This is separate
	// from model display grouping and never infers provider aliases from
	// hardcoded names.
	ProviderAliases map[string]string `yaml:"provider_aliases" json:"provider_aliases"`

	// Model canonicalization merges spelling variants of the same model in
	// every grouped surface (explorer model dimension, scope filters, debug
	// checklist) through a fully data-driven ordered rule list - see
	// internal/config/modelcanon.go (the semantic owner). Dashboard display
	// only - records, request details, and purge/export filters keep the stored
	// spelling. Debug matching has separate native base-model normalization.
	ModelRules []ModelRule `yaml:"model_rules" json:"model_rules"`

	// RequestOverrides is the opt-in list of scoped rewrites applied to the
	// UPSTREAM request before relay: set/replace/remove HTTP headers and set
	// Chat Completions or Anthropic client body token ceilings
	// (max_tokens / max_completion_tokens),
	// scoped per client, provider and/or model. Default nil: the feature is
	// fully off and the request path is untouched. Matching is exact-leaf
	// (an empty scope field is a wildcard); all matching rules apply in list
	// order, a later rule winning for the same header or body field.
	// validateRequestOverrides owns the normalization and validation.
	RequestOverrides []RequestOverride `yaml:"request_overrides" json:"request_overrides"`

	// SubConversations is the opt-in per-client list of request body fields
	// whose value identifies a sub-conversation (exemplar: opencode's
	// promptCacheKey). Default nil: tracking is fully off. Each entry names
	// one classified client exactly and 1..4 exact JSON field names, matched
	// only at the request body's top level and checked in order: the first
	// present field decides - a present value that is not a JSON string
	// counts as absent and the next field is checked, while a present string
	// that cannot be decoded or fails the identity rules drops the identity
	// for the request with no later field consulted. A kept value groups the
	// request under a k: conversation identity (after the X-Proxy-Session
	// header, before automatic client+key grouping); an absent or dropped
	// identity is never rejected, and strip removes every configured field
	// present at the top level of the client request body before relay or
	// translation, including a Responses client body.
	// validateSubConversations owns the normalization and validation.
	SubConversations []SubConversation `yaml:"sub_conversations" json:"sub_conversations"`
}

// ProviderOverride customizes how the proxy reads usage/cost/cache fields from
// one provider's responses and how it constructs the /v1/models list it serves
// for that provider. All fields are optional JSON key paths; empty means
// "use the built-in auto-detection". This is a field-name mapping (never a
// static price or static model data) so the proxy stays fully dynamic.
type ProviderOverride struct {
	// CostKeys are JSON key paths (dot-separated, e.g. "usage.cost") checked
	// in order for the per-request cost. Empty uses the built-in candidates.
	CostKeys []string `yaml:"cost_keys" json:"cost_keys"`
	// UsageKeys maps a canonical usage field (one of
	// metrics.CanonicalUsageFields - the same list the Settings dropdown
	// offers and ParseUsage consumes; validated on load) to the dotted key
	// path this provider uses, when it differs from the OpenAI/Anthropic
	// defaults.
	UsageKeys map[string]string `yaml:"usage_keys" json:"usage_keys"`
	// ModelsPath optionally names a second upstream metadata endpoint (a path
	// joined onto the base URL, e.g. "/language-models") whose per-model
	// metadata is merged into the /v1/models list the proxy serves, so a
	// client needs one call for the complete picture. The endpoint must answer
	// with a JSON array of entries either at the root, under "data", or under
	// "models"; entries join the main list by their "id". Fetch failures
	// degrade to the unenriched list (fail-open) - never a 502.
	ModelsPath string `yaml:"models_path" json:"models_path"`
	// ModelsKeys maps the output field name added to each /v1/models entry
	// (e.g. "input_modalities") to the dot-separated path of the value inside
	// the ModelsPath enrichment entry (e.g. "architecture.input_modalities").
	// A mapped field only fills entries that do not already carry it.
	ModelsKeys map[string]string `yaml:"models_keys" json:"models_keys"`
	// EnsureTools names function tools that must be present in the tools
	// array of relayed OpenAI-wire chat bodies for this provider. A
	// first-party client may gate its free tier on its own tool signature;
	// names the request lacks are injected as inert function stubs. When the
	// client supplied no tools, an absent, null, or auto tool_choice is
	// forced to none, an explicit none stays, and a required, named, or
	// malformed choice skips injection entirely. Only chat-shaped bodies (a
	// top-level messages array) are rewritten; a body that already carries
	// every configured name needs no tool edit and is byte-identical only
	// when no configured request override or strip also rewrites the body.
	// Names must be non-empty and unique, validated on load.
	EnsureTools []string `yaml:"ensure_tools" json:"ensure_tools"`
	// Headers are extra upstream headers sent on every request to this
	// provider (LLM calls and models discovery), e.g. mimicking a first-party
	// client's wire fingerprint. Values override client-forwarded headers;
	// the client's explicit X-Proxy-Headers injection map still wins.
	// accept-encoding and range are proxy-owned: the X-Proxy-Headers map
	// rejects them with 400, and a configured or override value is stripped
	// from the final request so the transport keeps its own gzip negotiation
	// and never sends a partial range. Four per-request template placeholders
	// expand at send time: "{{uuid4}}" (a
	// fresh UUID v4 per request), "{{platform}}" (rust-style "os; arch",
	// e.g. "linux; x86_64"), "{{opencode-msg-id}}" and
	// "{{opencode-ses-id}}" (fresh identifiers in the opencode CLI's wire
	// format, prefix plus 12 lowercase hex and 14 base62 characters). Names
	// are validated as RFC 7230 tokens and values as single-line header
	// values on load.
	Headers map[string]string `yaml:"headers" json:"headers"`
}

// RequestOverride is one scoped rewrite of the upstream request. The scope
// fields are exact-leaf matches against the request's classified client,
// canonical provider label (after provider_aliases) and recorded model id;
// an empty scope field matches anything, and a rule needs at least one
// scope field and at least one action. All matching rules apply in list
// order; for the same header or body field, a later rule's value wins.
type RequestOverride struct {
	// Client matches the request's classified client name exactly.
	Client string `yaml:"client" json:"client"`
	// Provider matches the request's canonical provider label (the label
	// after provider_aliases) exactly.
	Provider string `yaml:"provider" json:"provider"`
	// Model matches the request's recorded model id exactly.
	Model string `yaml:"model" json:"model"`
	// Headers sets or replaces upstream headers, keyed by header name.
	// validateRequestOverrides trims names and values and canonicalizes
	// name case, so a validated config carries one canonical spelling.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// RemoveHeaders deletes upstream headers by name (same grammar and
	// canonicalization as Headers). A name may not appear in both lists
	// of one rule.
	RemoveHeaders []string `yaml:"remove_headers" json:"remove_headers"`
	// Body rewrites token fields on a matching Chat or Anthropic client
	// body before translation; a nil field leaves the request's own value
	// untouched.
	Body *OverrideBody `yaml:"body" json:"body"`
}

// OverrideBody names the request body fields a rule may rewrite. Both
// fields are optional; nil means "leave the request's value alone".
type OverrideBody struct {
	MaxTokens           *int `yaml:"max_tokens" json:"max_tokens"`
	MaxCompletionTokens *int `yaml:"max_completion_tokens" json:"max_completion_tokens"`
}

// SubConversation is one classified client's tracked sub-conversation
// identity: the client whose requests are tracked, the ordered top-level
// request body field names whose value supplies the identity (the first
// present one decides), and whether the configured fields are stripped from
// the top level of the client request body before relay or translation.
type SubConversation struct {
	// Client matches the request's classified client name exactly: no
	// wildcard, no case folding. validateSubConversations trims it.
	Client string `yaml:"client" json:"client"`
	// Params are exact JSON field names matched only at the top level of the
	// client's request body and checked in order: the first present one
	// decides - a present value that is not a JSON string counts as absent
	// and the next field is checked, while a present string that cannot be
	// decoded or fails the identity rules drops the identity for the
	// request with no later field consulted. One JSON key segment each (see
	// checkSubConversationParam); validateSubConversations trims every name.
	Params []string `yaml:"params" json:"params"`
	// Strip removes every configured field present at the top level of the
	// client request body before relay or translation for this client, not
	// only the one that supplied the tracked value. It applies to any JSON
	// body, including a Responses client body. Default false: the configured
	// fields are forwarded unchanged.
	Strip bool `yaml:"strip" json:"strip"`
}
