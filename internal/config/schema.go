package config

import (
	"fmt"
	"strings"
	"time"
)

// Kind is the UI/API type of one configurable. The dashboard renders a control
// from this; Apply converts JSON into the Go field from this - never from a
// second, parallel type table.
type Kind string

const (
	KindString     Kind = "string"
	KindInt        Kind = "int"
	KindBytes      Kind = "bytes"
	KindBool       Kind = "bool"
	KindDuration   Kind = "duration"
	KindStrings    Kind = "strings"
	KindProviders  Kind = "providers"
	KindAliases    Kind = "aliases"
	KindModelRules Kind = "model_rules"
)

// Category groups fields in the Settings menu (and in the generated YAML).
type Category struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
}

// Field is one user-tunable. Schema() is the single registry: the dashboard,
// YAML writer, GET /admin/config, and Apply all read this. Defaults live only
// in Default(); ranges here are the UI hint and must match Validate() (the
// deny-by-default choke). Integer bands used in more than two places are
// named constants (DashLogRowsMin/Max).
type Field struct {
	Key       string   `json:"key"`
	Category  string   `json:"category"`
	Label     string   `json:"label"`
	Help      string   `json:"help"`
	Kind      Kind     `json:"kind"`
	HotReload bool     `json:"hot_reload"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	ZeroMeans string   `json:"zero_means,omitempty"`
	Unit      string   `json:"unit,omitempty"`
}

func num(v float64) *float64 { return &v }

// Categories is the Settings menu's section list, in display order.
func Categories() []Category {
	return []Category{
		{ID: "server", Label: "Server", Help: "Bind address, database, and ring depth restart to apply. shutdown_timeout applies at the next SIGINT/SIGTERM."},
		{ID: "request", Label: "Request", Help: "Inbound body cap, SSRF allowlist, optional body-preview, and debug-capture retention."},
		{ID: "upstream", Label: "Upstream", Help: "Connection pool, header timeout, and stream keepalives."},
		{ID: "queue", Label: "Queue & retry", Help: "Per-key concurrency, transparent 429/5xx retry, and quality re-asks."},
		{ID: "storm", Label: "Error storm protection", Help: "Bounded recovery queues for affected providers or exact model names. Hot-reloads; policy changes clear observations and wake waiters. Banner visibility changes do not reset protection."},
		{ID: "conversation", Label: "Conversations", Help: "How the request log groups turns into conversations."},
		{ID: "format", Label: "Format translation", Help: "Anthropic defaults and Cursor agent.v1 bridging."},
		{ID: "storage", Label: "Storage", Help: "Durable SQLite write pipeline. Restart to apply."},
		{ID: "dashboard", Label: "Dashboard", Help: "Live-view cadence and request-log window. Hot-reloads; open dashboards pick up the next tick."},
		{ID: "models", Label: "Models", Help: "Model grouping rules - an ordered rewrite pipeline merging spelling variants of the same model in every grouped surface. Records keep their exact spelling; hot-reloads."},
		{ID: "providers", Label: "Providers", Help: "Optional per-provider JSON field-name maps: usage/cost response keys, the models metadata endpoint merged into /v1/models, and optional upstream wire headers. Empty = auto-detect."},
	}
}

// Schema is the single registry of every user-tunable. Adding a Config field
// requires an entry here; TestSchemaCoversConfigFields fails if they drift.
func Schema() []Field {
	return []Field{
		// ---- server ----
		{Key: "listen", Category: "server", Label: "Listen address",
			Help: "Host:port the HTTP server binds. Restart required.",
			Kind: KindString, HotReload: false},
		{Key: "db_path", Category: "server", Label: "Database path",
			Help: "SQLite metrics store. Empty disables durability (ring only). Restart required.",
			Kind: KindString, HotReload: false},
		{Key: "history_size", Category: "server", Label: "History size",
			Help: "In-memory ring depth and startup backfill count. Durable history and paged request logs can extend beyond this window. Restart required.",
			Kind: KindInt, HotReload: false, Min: num(1), Max: num(1_000_000)},
		{Key: "shutdown_timeout", Category: "server", Label: "Shutdown timeout",
			Help: "HTTP shutdown wait after active request contexts are canceled on SIGINT/SIGTERM. Storage draining follows separately. Must be > 0; applies at the next shutdown without restarting the listener.",
			Kind: KindDuration, HotReload: true},
		{Key: "restart_drain_timeout", Category: "server", Label: "Restart drain timeout",
			Help: "How long a dashboard-triggered restart waits for in-flight streams. Expiry aborts the restart and resumes accepting traffic; unfinished streams are not killed. New connections queue during a successful handoff.",
			Kind: KindDuration, HotReload: true, Min: num(0), ZeroMeans: "wait indefinitely"},
		{Key: "read_header_timeout", Category: "server", Label: "Read header timeout",
			Help: "Slowloris guard on inbound headers. Restart required.",
			Kind: KindDuration, HotReload: false, Min: num(0), ZeroMeans: "no timeout"},
		{Key: "idle_timeout", Category: "server", Label: "Idle timeout",
			Help: "Keep-alive idle connections. No write timeout (it would kill SSE). Restart required.",
			Kind: KindDuration, HotReload: false, Min: num(0), ZeroMeans: "no timeout"},

		// ---- request ----
		{Key: "max_request_bytes", Category: "request", Label: "Max request bytes",
			Help: "Inbound body the proxy will buffer to read routing metadata. Larger bodies are rejected with 413.",
			Kind: KindBytes, HotReload: true, Min: num(1024), Max: num(1 << 30), Unit: "bytes"},
		{Key: "allowed_base_urls", Category: "request", Label: "Allowed base URLs",
			Help: "SSRF allowlist of upstream base-URL prefixes. Empty allows any target (single trusted operator). Each entry must be an absolute http:// or https:// URL with a host.",
			Kind: KindStrings, HotReload: true},
		{Key: "capture_body_preview", Category: "request", Label: "Capture body preview",
			Help: "Store a short bounded preview of the last user prompt / response in metrics. Off by default so content is not persisted.",
			Kind: KindBool, HotReload: true},
		{Key: "debug_capture_ttl", Category: "request", Label: "Debug capture TTL",
			Help: "How long a full debug capture (bodies and headers) is kept after the request finishes. Operator debug sessions choose which traffic is captured; this is blob retention, not the session timer.",
			Kind: KindDuration, HotReload: true, Min: num(float64(DebugCaptureTTLMin)), Max: num(float64(DebugCaptureTTLMax))},
		{Key: "debug_capture_max_bytes", Category: "request", Label: "Debug capture max bytes",
			Help: "Cap on one debug capture (request body plus client-facing response bytes). Past the cap the rest is dropped and the capture is marked truncated.",
			Kind: KindBytes, HotReload: true, Min: num(float64(DebugCaptureMaxBytesMin)), Max: num(float64(DebugCaptureMaxBytesMax)), Unit: "bytes"},
		{Key: "auto_token_refresh", Category: "request", Label: "Auto token refresh",
			Help: "Exchange an expired or expiring JWT when the client sends X-Proxy-Refresh-Token. All supported providers use inference handback: HTTP 401 with token_expired and access_token in the error body, plus refresh_token only when the exchange returns a different replacement. No inference is sent; adopt the returned credentials and retry, retaining the old refresh token if no replacement is returned. Model discovery refreshes transparently without returning tokens. Stateless: no reusable credentials are retained.",
			Kind: KindBool, HotReload: true},

		// ---- upstream ----
		{Key: "upstream_timeout", Category: "upstream", Label: "Upstream header timeout",
			Help: "How long to wait for upstream response headers. Streaming bodies are not subject to this. Transient header timeouts are retried.",
			Kind: KindDuration, HotReload: true, Min: num(0), ZeroMeans: "no header cap"},
		{Key: "models_discovery_timeout", Category: "upstream", Label: "Model discovery timeout",
			Help: "One upstream metadata budget for transparent key refresh, all model pages and optional enrichment. Never applies to LLM streams. New discoveries use reloaded limits.",
			Kind: KindDuration, HotReload: true, Min: num(float64(ModelsDiscoveryTimeoutMin)), Max: num(float64(ModelsDiscoveryTimeoutMax))},
		{Key: "models_discovery_max_bytes", Category: "upstream", Label: "Model discovery bytes",
			Help: "Aggregate response bytes read after transport decoding across model pages, unary RPCs and enrichment. Primary overflow fails; optional enrichment is discarded. This is not a strict heap limit.",
			Kind: KindBytes, HotReload: true, Min: num(ModelsDiscoveryMaxBytesMin), Max: num(ModelsDiscoveryMaxBytesMax), Unit: "bytes"},
		{Key: "models_discovery_max_pages", Category: "upstream", Label: "Model discovery pages",
			Help: "Maximum upstream model responses per discovery, including a unary RPC and optional enrichment. Limits are captured once, never reset between pages.",
			Kind: KindInt, HotReload: true, Min: num(ModelsDiscoveryMaxPagesMin), Max: num(ModelsDiscoveryMaxPagesMax)},
		{Key: "max_conns_per_host", Category: "upstream", Label: "Max conns per host",
			Help: "Total connections per upstream host. Sized for concurrent SSE streams, not requests/sec.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "unlimited"},
		{Key: "max_idle_conns", Category: "upstream", Label: "Max idle conns",
			Help: "Process-wide idle connection pool.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "unlimited"},
		{Key: "max_idle_conns_per_host", Category: "upstream", Label: "Max idle conns per host",
			Help: "Idle connections kept per upstream host. Cannot exceed max conns per host when that is set. net/http treats 0 as a default of 2, not unlimited, so this must be >= 1.",
			Kind: KindInt, HotReload: true, Min: num(1)},
		{Key: "idle_conn_timeout", Category: "upstream", Label: "Idle conn timeout",
			Help: "How long an idle upstream connection is kept before closing.",
			Kind: KindDuration, HotReload: true, Min: num(0), ZeroMeans: "never expire"},
		{Key: "sse_keepalive_interval", Category: "upstream", Label: "SSE keepalive interval",
			Help: "How often a quiet streaming socket gets a `: keepalive` comment (queue wait, operator hold, TTFB, mid-stream gaps). Range 1ns..10m. Applies to streams started after reload.",
			Kind: KindDuration, HotReload: true, Min: num(1), Max: num(float64(HeartbeatIntervalMax))},

		// ---- queue ----
		{Key: "max_concurrent", Category: "queue", Label: "Max concurrent",
			Help: "In-flight sends per provider+key. 0 is unlimited.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "unlimited"},
		{Key: "max_queue_size", Category: "queue", Label: "Max queue size",
			Help: "Queued waiters per group. 0 is unlimited.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "unlimited"},
		{Key: "max_queue_wait", Category: "queue", Label: "Max queue wait",
			Help: "How long a request may sit in the queue before 429. 0 is unlimited. Operator-hold time is excluded.",
			Kind: KindDuration, HotReload: true, Min: num(0), ZeroMeans: "unlimited"},
		{Key: "max_retries", Category: "queue", Label: "Max retries",
			Help: "Transparent retries of transient 429 / 5xx / transport failures. Durable quota 429s are never retried.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "no retries"},
		{Key: "queue_retry_after", Category: "queue", Label: "Queue Retry-After",
			Help: "Retry-After hint (seconds) sent when our own queue is full or the wait is exceeded.",
			Kind: KindInt, HotReload: true, Min: num(0), Max: num(QueueRetryAfterMax), Unit: "seconds"},
		{Key: "base_backoff", Category: "queue", Label: "Base backoff",
			Help: "Initial delay when the provider sends no retry hint. Doubles each attempt and each consecutive failed request, up to max backoff. Cannot exceed max backoff. Must be > 0.",
			Kind: KindDuration, HotReload: true},
		{Key: "max_backoff", Category: "queue", Label: "Max backoff",
			Help: "Cap on adaptive backoff. Does not clamp a provider Retry-After / rate-limit-reset (daily limits are often 30–60m). Must be > 0.",
			Kind: KindDuration, HotReload: true},
		{Key: "quality_retries", Category: "queue", Label: "Quality retries",
			Help: "Transparent re-attempts of a degenerate 200 (empty completion, empty tool_calls, truncated stream) and Cursor's empty-resume re-ask. 0 disables.",
			Kind: KindInt, HotReload: true, Min: num(0), Max: num(QualityRetriesMax), ZeroMeans: "disabled"},

		// ---- storm ----
		{Key: "storm_enabled", Category: "storm", Label: "Enable protection",
			Help: "Detect elevated upstream failure rates and queue affected sends for bounded recovery probes. Opt-in; requests can still fail on cancellation, exhausted retry budgets, queue limits or permanent errors. Retries can repeat upstream execution and billing.",
			Kind: KindBool, HotReload: true},
		{Key: "storm_provider_enabled", Category: "storm", Label: "Provider protection",
			Help: "Gate every model on a provider only when all active models individually meet the sample and failure thresholds. Active means a request in the same detection window; idle models expire.",
			Kind: KindBool, HotReload: true},
		{Key: "storm_model_enabled", Category: "storm", Label: "Model protection",
			Help: "Gate an affected exact recorded model name within its provider. Dashboard model grouping does not broaden this scope.",
			Kind: KindBool, HotReload: true},
		{Key: "storm_banner_enabled", Category: "storm", Label: "Show incident banner",
			Help: "Show affected providers/models, safe error categories, failure percentages, queue state and recovery timing at the top of the dashboard. Changing visibility does not reset protection.",
			Kind: KindBool, HotReload: true},
		{Key: "storm_window", Category: "storm", Label: "Detection window",
			Help: "Rolling time range for successful and eligible failed upstream attempts. Models with no requests in this window expire from provider-wide checks.",
			Kind: KindDuration, HotReload: true, Min: num(float64(time.Second)), Max: num(float64(time.Hour))},
		{Key: "storm_min_samples", Category: "storm", Label: "Minimum samples",
			Help: "Minimum successful or eligible failed upstream attempts in the detection window before a model qualifies. Provider-wide protection requires every active model to qualify. Retries count as separate attempts; this is not a distinct-request percentage.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(1_000_000)},
		{Key: "storm_error_percent", Category: "storm", Label: "Failure threshold",
			Help: "Minimum percentage of sampled upstream attempts with a selected failure. Applied to each exact model after the minimum sample count; all active models must qualify for provider-wide protection.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(100), Unit: "percent"},
		{Key: "storm_initial_backoff", Category: "storm", Label: "Initial recovery delay",
			Help: "Initial storm cooldown before a recovery probe. Cannot exceed maximum recovery delay. Valid upstream Retry-After/reset hints still apply independently.",
			Kind: KindDuration, HotReload: true, Min: num(float64(time.Millisecond)), Max: num(float64(time.Hour))},
		{Key: "storm_max_backoff", Category: "storm", Label: "Maximum recovery delay",
			Help: "Cap on exponentially increased storm cooldowns. Must be at least initial recovery delay. Does not shorten upstream Retry-After/reset hints.",
			Kind: KindDuration, HotReload: true, Min: num(float64(time.Millisecond)), Max: num(float64(time.Hour))},
		{Key: "storm_backoff_multiplier", Category: "storm", Label: "Recovery delay multiplier",
			Help: "Multiply the storm cooldown after a failed recovery probe, up to maximum recovery delay.",
			Kind: KindInt, HotReload: true, Min: num(2), Max: num(10)},
		{Key: "storm_jitter_percent", Category: "storm", Label: "Recovery delay jitter",
			Help: "Random spread as a percentage of the storm cooldown to separate recovery attempts. 0 disables jitter.",
			Kind: KindInt, HotReload: true, Min: num(0), Max: num(100), ZeroMeans: "no jitter", Unit: "percent"},
		{Key: "storm_recovery_successes", Category: "storm", Label: "Recovery successes",
			Help: "Consecutive successful recovery probes required before queued traffic resumes normally.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(100)},
		{Key: "storm_max_queue", Category: "storm", Label: "Maximum queued requests",
			Help: "Maximum requests waiting for storm recovery. Excess work is rejected locally; requests and their bounded bodies stay in process memory while waiting.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(100000)},
		{Key: "storm_max_wait", Category: "storm", Label: "Maximum queue wait",
			Help: "Maximum duration of each storm gate wait, not total request duration across all retries and other queues. Caller cancellation may end the wait earlier.",
			Kind: KindDuration, HotReload: true, Min: num(float64(time.Millisecond)), Max: num(float64(24 * time.Hour))},
		{Key: "storm_max_scopes", Category: "storm", Label: "Maximum tracked scopes",
			Help: "Bound on provider and exact-model detector scopes retained in process memory. Increasing it does not establish a total process memory ceiling.",
			Kind: KindInt, HotReload: true, Min: num(2), Max: num(100000)},
		{Key: "storm_max_retries", Category: "storm", Label: "Additional storm retries",
			Help: "Additional retries beyond max_retries only while a relevant storm is active and the failure matches a selected eligible transient condition. Does not replay meaningful emitted upstream content. 0 keeps the ordinary retry budget.",
			Kind: KindInt, HotReload: true, Min: num(0), Max: num(1000), ZeroMeans: "no additional retries"},
		{Key: "storm_status_codes", Category: "storm", Label: "HTTP failure statuses",
			Help: "Exact status strings selected for detection and additional storm retries: 429 or 500..599, without duplicates. Empty disables HTTP status triggers. 429 is opt-in because limits may be credential-specific; durable quota/billing 429s remain excluded.",
			Kind: KindStrings, HotReload: true},
		{Key: "storm_transport_errors", Category: "storm", Label: "Transient transport failures",
			Help: "Include transport failures already eligible for the shared retry policy. Caller cancellation and permanent transport errors are excluded.",
			Kind: KindBool, HotReload: true},
		{Key: "storm_stream_errors", Category: "storm", Label: "Response and stream failures",
			Help: "Include recorded in-band, stream and quality failures to protect future sends. Reports a generic response error without retaining error bodies for the banner. Never replays meaningful emitted upstream content.",
			Kind: KindBool, HotReload: true},

		// ---- conversation ----
		{Key: "conversation_idle_gap", Category: "conversation", Label: "Idle gap",
			Help: "A gap longer than this starts a new conversation for the same client+key. Generous so thinking pauses inside one task don't split it. Must be > 0.",
			Kind: KindDuration, HotReload: true},
		{Key: "conversation_max_open", Category: "conversation", Label: "Max open",
			Help: "Cap on concurrently-open conversations per client+key. Oldest is evicted past it.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(100000)},

		// ---- format ----
		{Key: "anthropic_default_max_tokens", Category: "format", Label: "Anthropic default max tokens",
			Help: "Injected into translated Anthropic requests when the client sent neither max_tokens nor max_completion_tokens.",
			Kind: KindInt, HotReload: true, Min: num(1), Max: num(1_000_000)},
		{Key: "cursor_default_context_window", Category: "format", Label: "Cursor default context window",
			Help: "Optional context-window hint (tokens) stamped onto every model in GET /v1/models when X-Proxy-Format is cursor. 0 omits the field.",
			Kind: KindInt, HotReload: true, Min: num(0), ZeroMeans: "omit (client default)"},
		{Key: "cursor_park_ttl", Category: "format", Label: "Cursor park TTL",
			Help: "How long a parked Cursor Run stream stays open waiting for tool results. Must be > 0. Expiry closes the stream; the next continuation cold-starts.",
			Kind: KindDuration, HotReload: true},
		{Key: "cursor_heartbeat_interval", Category: "format", Label: "Cursor heartbeat",
			Help: "How often the proxy pings a Cursor agent.v1 run (idle parked runs included). Range 1ns..10m. Applies to runs created after reload.",
			Kind: KindDuration, HotReload: true, Min: num(1), Max: num(float64(HeartbeatIntervalMax))},

		// ---- storage ----
		{Key: "storage_write_chan_cap", Category: "storage", Label: "Write channel cap",
			Help: "Buffered records before overflow drops. Restart required.",
			Kind: KindInt, HotReload: false, Min: num(1)},
		{Key: "storage_batch_cap", Category: "storage", Label: "Batch cap",
			Help: "Max records per SQLite transaction. Cannot exceed the write channel cap. Restart required.",
			Kind: KindInt, HotReload: false, Min: num(1)},
		{Key: "storage_flush_interval", Category: "storage", Label: "Flush interval",
			Help: "Partial-batch flush cadence. Must be > 0 (a zero ticker panics). Restart required.",
			Kind: KindDuration, HotReload: false},
		{Key: "storage_query_timeout", Category: "storage", Label: "Query timeout",
			Help: "Bounds dashboard SELECT queries. Must be > 0. Open and AggAPI bind at start (restart required); pause/throttle persist reads the live snapshot.",
			Kind: KindDuration, HotReload: false},
		{Key: "storage_query_max_bytes", Category: "storage", Label: "Query result bytes",
			Help: "Maximum JSON bytes returned by /metrics/query; also bounds SQLite string/blob values. Oversized results fail with HTTP 413, never truncate. Does not cap all SQLite working memory. Restart required.",
			Kind: KindInt, HotReload: false, Min: num(StorageQueryMaxBytesMin), Max: num(StorageQueryMaxBytesMax)},
		{Key: "storage_query_max_rows", Category: "storage", Label: "Query result rows",
			Help: "Maximum rows returned by /metrics/query. Oversized results fail with HTTP 413; use SQL filters or LIMIT. Does not limit dashboard aggregates or exports. Restart required.",
			Kind: KindInt, HotReload: false, Min: num(StorageQueryMaxRowsMin), Max: num(StorageQueryMaxRowsMax)},

		// ---- dashboard ----
		{Key: "dash_log_rows", Category: "dashboard", Label: "Request log rows",
			Help: "Page size of the live request log (newest first). Scrolling near the bottom loads more, including older durable history.",
			Kind: KindInt, HotReload: true, Min: num(DashLogRowsMin), Max: num(DashLogRowsMax)},
		{Key: "dash_poll_interval", Category: "dashboard", Label: "Poll interval",
			Help: "KPI / pause / SSE-fallback poll cadence. Must be > 0.",
			Kind: KindDuration, HotReload: true},
		{Key: "dash_chart_refresh", Category: "dashboard", Label: "Chart refresh",
			Help: "How often the traffic chart re-fetches server-authoritative buckets and period totals. Must be > 0.",
			Kind: KindDuration, HotReload: true},
		{Key: "dash_explorer_stale", Category: "dashboard", Label: "Explorer stale after",
			Help: "How old a cached explorer breakdown may be before it is re-fetched. Must be > 0.",
			Kind: KindDuration, HotReload: true},

		// ---- providers ----
		{Key: "providers", Category: "providers", Label: "Provider field maps",
			Help: "Per-provider cost/usage JSON field paths, optional models_path/models_keys enrichment, and headers (a string map). Field names only, never static prices. Custom cost_keys must report USD; leave cost_in_usd_ticks to automatic unit conversion. Header values expand {{uuid4}} to a fresh UUID and {{platform}} to the server's Rust-style os; arch pair; unknown templates stay literal. On HTTP relay requests, configured headers override forwarded/extracted-auth values and X-Proxy-Headers wins. Model discovery and the Cursor bridge construct headers separately and do not apply X-Proxy-Headers. Headers can contain secrets; keep runtime configuration private.",
			Kind: KindProviders, HotReload: true},
		{Key: "provider_aliases", Category: "providers", Label: "Provider aliases",
			Help: "Map an old provider label to a canonical label. Rewrites existing stored history at boot and on reload; new requests use the canonical label. Existing in-memory records keep their prior label until they age out or the process restarts. Removing a mapping does not restore rewritten history. Chains and self-maps are rejected.",
			Kind: KindAliases, HotReload: true},
		// ---- models ----
		{Key: "model_rules", Category: "models", Label: "Model grouping rules",
			Help: "Ordered rewrite pipeline that merges spelling variants of the same model in every grouped surface (explorer model dimension, scope filters, debug checklist, Clear/Logs optgroups, and the request-log leaf\u2019s displayed name). Each rule is one step: exact (whole-string merge old → new), pattern (regex rewrite of all occurrences, $1 capture refs), lower (fold case). Rules apply once in order; the shipped default folds case, strips a `vendor/` namespace, strips a trailing `:tag`, strips a trailing architecture/quant suffix (`-fp4`, `-nvfp4`, `-bf16`, `-int8`, `-q4_k_m`), and unifies `.` with `-` between digits. An explicitly empty list groups by the exact stored spelling. Stored values remain unchanged; purge/export match them exactly and request details show the original. Debug matching uses separate native base-model normalization, not these display rules. Hot-reloads.",
			Kind: KindModelRules, HotReload: true},
	}
}

// FieldByKey returns the schema entry for a yaml key, or nil.
func FieldByKey(key string) *Field {
	for i, f := range Schema() {
		if f.Key == key {
			fp := Schema()[i]
			return &fp
		}
	}
	return nil
}

// TypeLine is the "type · range · default" annotation written into YAML
// comments via WriteYAML (Settings Apply). Not rendered in the Settings form.
func (f Field) TypeLine(def any) string {
	var b strings.Builder
	b.WriteString("type: ")
	switch f.Kind {
	case KindString:
		b.WriteString("string")
	case KindInt:
		b.WriteString("int")
	case KindBytes:
		b.WriteString("int (bytes)")
	case KindBool:
		b.WriteString("bool")
	case KindDuration:
		b.WriteString("duration")
	case KindStrings:
		b.WriteString("list of string")
	case KindProviders:
		b.WriteString("map of provider label → {cost_keys, usage_keys, models_path, models_keys, headers}")
	case KindAliases:
		b.WriteString("map of old provider label → canonical label")
	case KindModelRules:
		b.WriteString("ordered list of {mode: exact|pattern|lower, from, to} rewrite rules")
	default:
		b.WriteString(string(f.Kind))
	}
	// KindBytes already writes "int (bytes)"; only KindInt appends Unit
	// (queue_retry_after → "int (seconds)").
	if f.Kind == KindInt && f.Unit != "" {
		b.WriteString(" (" + f.Unit + ")")
	}
	if f.Min != nil || f.Max != nil || f.ZeroMeans != "" {
		b.WriteString(" · range ")
		switch {
		case f.Min != nil && f.Max != nil:
			b.WriteString(fmt.Sprintf("%s..%s", f.formatBound(*f.Min), f.formatBound(*f.Max)))
		case f.Min != nil:
			b.WriteString(">= " + f.formatBound(*f.Min))
		case f.Max != nil:
			b.WriteString("<= " + f.formatBound(*f.Max))
		}
		if f.ZeroMeans != "" {
			if f.Min != nil || f.Max != nil {
				b.WriteString(" (0 = " + f.ZeroMeans + ")")
			} else {
				b.WriteString("0 = " + f.ZeroMeans)
			}
		}
	}
	if f.Kind == KindDuration && f.Min == nil && f.Max == nil && f.ZeroMeans == "" {
		b.WriteString(" · range > 0")
	}
	switch {
	case f.Kind == KindModelRules:
		// fmt.Sprint of a []ModelRule would dump Go struct literals
		// ("{lower   false}") - describe the shipped pipeline instead.
		if def != nil {
			b.WriteString(" \u00b7 default: the shipped pipeline (lower \u00b7 strip vendor/ \u00b7 strip :tag \u00b7 strip architecture/quant suffix \u00b7 unify . and - between digits); restorable in Settings with \u21ba default pipeline")
		}
	case def != nil && f.Kind != KindProviders && f.Kind != KindAliases && f.Kind != KindStrings:
		b.WriteString(" · default ")
		b.WriteString(fmt.Sprint(def))
	}
	return b.String()
}

// formatBound is TypeLine's range-bound printer: durations via FormatDuration
// (1ns, 10m) so Min/Max never leak as raw nanoseconds; KindBytes as exact
// binary units (GiB/MiB/KiB, same spirit as chrome.js fmtBytes) so a 1GiB
// max is not printed as 1073741824.
func (f Field) formatBound(v float64) string {
	if f.Kind == KindDuration {
		d := time.Duration(v)
		if d == 0 {
			return "0"
		}
		return FormatDuration(d)
	}
	if f.Kind == KindBytes {
		n := int64(v)
		// n != 0: 0 is divisible by every unit and must not print as 0GiB.
		if n != 0 {
			switch {
			case n%(1<<30) == 0:
				return trimNum(float64(n/(1<<30))) + "GiB"
			case n%(1<<20) == 0:
				return trimNum(float64(n/(1<<20))) + "MiB"
			case n > 1<<10 && n%(1<<10) == 0: // 1024 stays 1024 (range 1024..1GiB)
				return trimNum(float64(n/(1<<10))) + "KiB"
			}
		}
		return trimNum(v)
	}
	return trimNum(v)
}

func trimNum(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
