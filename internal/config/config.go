package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
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

	// ---- queueing / rate-limit handling ----
	// The proxy transparently queues requests when a provider returns 429, and
	// transparently retries 429 and any 5xx (transient upstream errors) with
	// exponential backoff - Retry-After / rate-limit reset headers are a
	// floor, not a replacement - so clients never see a transient failure.
	// A 429 carrying a durable quota/billing
	// error (insufficient_quota/credits, spend limits) is never retried: it is
	// surfaced immediately (waiting cannot clear it).
	MaxConcurrent int           `yaml:"max_concurrent" json:"max_concurrent"` // per provider+key; 0 = unlimited
	MaxQueueSize  int           `yaml:"max_queue_size" json:"max_queue_size"` // per group; 0 = unlimited
	MaxQueueWait  time.Duration `yaml:"max_queue_wait" json:"max_queue_wait"` // max queue wait before 429; 0 = unlimited
	MaxRetries    int           `yaml:"max_retries" json:"max_retries"`       // max transient (429/5xx) retries
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
	// long provider hint (daily limits are often 30–60m). Generous defaults:
	// these are upstream-recovery pauses, and too-short backoff just
	// re-hammers a struggling provider.
	BaseBackoff time.Duration `yaml:"base_backoff" json:"base_backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff" json:"max_backoff"`

	// QualityRetries bounds the transparent re-attempts of a degenerate 200
	// (an empty completion, tool_calls with zero tool calls, or a truncated
	// stream): the non-streaming pre-write retry and the cursor one-shot
	// fresh-user re-ask. Each re-attempt re-sends the full request, so a small
	// budget is the sane ceiling; 0 disables quality handling entirely.
	QualityRetries int `yaml:"quality_retries" json:"quality_retries"`

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
	// when the client sent neither max_tokens nor max_completion_tokens
	// (Anthropic requires max_tokens).
	AnthropicDefaultMaxTokens int `yaml:"anthropic_default_max_tokens" json:"anthropic_default_max_tokens"`
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
	// The archive is buffered; this is the admit/reject ceiling, not a heap guarantee.
	BackupMaxBytes ByteSize `yaml:"backup_max_bytes" json:"backup_max_bytes"`

	// ---- HTTP server timeouts ----
	// ReadHeaderTimeout guards against slowloris; IdleTimeout bounds keep-alive
	// idle connections. There is deliberately no WriteTimeout: it would kill
	// long-lived SSE streams.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" json:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout" json:"idle_timeout"`

	// SSEKeepaliveInterval is how often a quiet streaming socket gets an SSE
	// comment (`: keepalive`) so proxies and the client do not drop the
	// connection during queue wait, operator hold, upstream TTFB, or mid-stream
	// gaps. Chat Completions clients ignore SSE comments. Hot-reloads (new
	// streams; in-flight pacers keep the interval they started with).
	SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval" json:"sse_keepalive_interval"`

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

	// Providers, keyed by an X-Proxy-* base-URL-derived provider label, hold
	// optional per-provider overrides for how to read usage/cost/cache fields
	// from that provider's responses (they mostly all report usage+cost back,
	// but under different key names). See ProviderOverride.
	Providers map[string]ProviderOverride `yaml:"providers" json:"providers"`

	// ProviderAliases merges provider labels: an old label recorded under a
	// previous naming scheme is rewritten to its canonical label at capture
	// time and in existing stored history (boot + reload). Already-buffered
	// records keep their prior label until age-out/restart; removing a mapping
	// cannot recover rewritten spellings. This is separate from model display
	// grouping and never infers provider aliases from hardcoded names.
	ProviderAliases map[string]string `yaml:"provider_aliases" json:"provider_aliases"`

	// Model canonicalization merges spelling variants of the same model in
	// every grouped surface (explorer model dimension, scope filters, debug
	// checklist) through a fully data-driven ordered rule list - see
	// internal/config/modelcanon.go (the semantic owner). Dashboard display
	// only - records, request details, and purge/export filters keep the stored
	// spelling. Debug matching has separate native base-model normalization.
	ModelRules []ModelRule `yaml:"model_rules" json:"model_rules"`
}

// provPathRE is the dotted JSON path shape every provider field-map value
// must match (object segments, optionally indexed by numeric array
// segments). Shared by cost_keys, usage_keys, and models_keys validation so
// all three reject garbage at the load boundary with one rule.
var provPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// canonicalUsageFieldSet is the lookup-set form of
// metrics.CanonicalUsageFields - the single owner of the canonical usage
// field names (served to the Settings UI dropdown, consumed by ParseUsage).
// usage_keys keys are checked against it so the load boundary accepts
// exactly what the UI offers and what ParseUsage reads; a typo'd canonical
// name would otherwise be a silent runtime no-op.
var canonicalUsageFieldSet = func() map[string]bool {
	set := make(map[string]bool, len(metrics.CanonicalUsageFields))
	for _, f := range metrics.CanonicalUsageFields {
		set[f] = true
	}
	return set
}()

// checkProvPath rejects a provider field-map entry whose value is not a
// dotted JSON path (provPathRE). label and slot locate the entry in the
// error ("providers.<label>.<slot>"); name is the map key the path belongs
// to, empty for the slice-shaped cost_keys.
func checkProvPath(label, slot, name, path string) error {
	if !provPathRE.MatchString(path) {
		if name == "" {
			return fmt.Errorf("providers.%s.%s: %q is not a dotted JSON path", label, slot, path)
		}
		return fmt.Errorf("providers.%s.%s: %s: %q is not a dotted JSON path", label, slot, name, path)
	}
	return nil
}

// sortedKeys returns m's keys in sorted order, for error-reporting loops over
// Go maps: map iteration order is randomized, so an unsorted loop reports a
// nondeterministic first error when several entries are invalid. Every loop in
// Validate that can return an error iterates through this.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	// Headers are extra upstream headers sent on every request to this
	// provider (LLM calls and models discovery), e.g. mimicking a first-party
	// client's wire fingerprint. Values override client-forwarded headers;
	// the client's explicit X-Proxy-Headers injection map still wins. Two
	// per-request template placeholders expand at send time: "{{uuid4}}" (a
	// fresh UUID v4 per request) and "{{platform}}" (rust-style "os; arch",
	// e.g. "linux; x86_64"). Names are validated as RFC 7230 tokens and
	// values as single-line header values on load.
	Headers map[string]string `yaml:"headers" json:"headers"`
}

// cloneStrMap copies a string map, preserving nil (a nil map stays nil so
// "absent" and "empty" never merge into one value).
func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Clone returns a deep copy of c, so a reload can swap in a fully independent
// snapshot: the Providers map, its nested slices/maps, and the AllowedBaseURLs
// slice are all copied, never shared with a prior snapshot. The proxy treats
// each snapshot as immutable, so a live snapshot is never mutated by a reload.
func (c *Config) Clone() *Config {
	out := *c // value copy of all scalar fields
	if c.AllowedBaseURLs != nil {
		out.AllowedBaseURLs = append([]string(nil), c.AllowedBaseURLs...)
	}
	if c.StormStatusCodes != nil {
		out.StormStatusCodes = append([]string{}, c.StormStatusCodes...)
	}
	if c.Providers != nil {
		out.Providers = make(map[string]ProviderOverride, len(c.Providers))
		for k, v := range c.Providers {
			out.Providers[k] = ProviderOverride{
				CostKeys:   append([]string(nil), v.CostKeys...),
				UsageKeys:  cloneStrMap(v.UsageKeys),
				ModelsPath: v.ModelsPath,
				ModelsKeys: cloneStrMap(v.ModelsKeys),
				Headers:    cloneStrMap(v.Headers),
			}
		}
	}
	if c.ProviderAliases != nil {
		out.ProviderAliases = make(map[string]string, len(c.ProviderAliases))
		for k, v := range c.ProviderAliases {
			out.ProviderAliases[k] = v
		}
	}
	if c.ModelRules != nil {
		out.ModelRules = make([]ModelRule, len(c.ModelRules))
		copy(out.ModelRules, c.ModelRules)
	}
	return &out
}

// DashLogRowsMin/Max are the allowed band for dash_log_rows: Validate,
// overlay, Settings schema, and GET /metrics/agg/log share this pair.
const (
	DashLogRowsMin = 10
	DashLogRowsMax = 500
	// QualityRetriesMax is the band for quality_retries (0 disables).
	QualityRetriesMax = 3
	// QueueRetryAfterMax is the Retry-After hint cap (HTTP delta-seconds).
	QueueRetryAfterMax = 24 * time.Hour
	// Inbound body band (Validate, overlay, Schema).
	MaxRequestBytesMin = 1024
	MaxRequestBytesMax = 1 << 30
	// Debug capture retention / size bands (Validate, overlay, Schema).
	DebugCaptureTTLMin      = time.Hour
	DebugCaptureTTLMax      = 7 * 24 * time.Hour
	DebugCaptureMaxBytesMin = 4 << 10  // 4 KiB
	DebugCaptureMaxBytesMax = 16 << 20 // 16 MiB
	// Ad-hoc query result bands (Validate, overlay, Settings schema).
	StorageQueryMaxBytesMin = 4 << 10
	StorageQueryMaxBytesMax = 64 << 20
	StorageQueryMaxRowsMin  = 1
	StorageQueryMaxRowsMax  = 100000
	// Settings backup archive band (Validate, overlay, Schema).
	BackupMaxBytesMin = 1 << 20 // 1 MiB
	BackupMaxBytesMax = 4 << 30 // 4 GiB
)

// Discovery limits bound upstream metadata work, not total Go heap usage.
const (
	ModelsDiscoveryTimeoutMin  = time.Second
	ModelsDiscoveryTimeoutMax  = 5 * time.Minute
	ModelsDiscoveryMaxBytesMin = 4 << 10
	ModelsDiscoveryMaxBytesMax = 64 << 20
	ModelsDiscoveryMaxPagesMin = 1
	ModelsDiscoveryMaxPagesMax = 1000
)

// HeartbeatIntervalMin/Max bound cursor_heartbeat_interval and
// sse_keepalive_interval (Validate, overlay, Schema). TypeLine prints them
// through FormatDuration.
const (
	HeartbeatIntervalMin = time.Nanosecond
	HeartbeatIntervalMax = 10 * time.Minute
)

func heartbeatRange() string {
	return FormatDuration(HeartbeatIntervalMin) + ".." + FormatDuration(HeartbeatIntervalMax)
}

// Default returns the built-in defaults. This is the only place defaults live.
func Default() *Config {
	return &Config{
		Listen:              ":8080",
		DBPath:              "proxy.db",
		HistorySize:         10000,
		ShutdownTimeout:     5 * time.Second,
		RestartDrainTimeout: 10 * time.Minute,

		MaxRequestBytes:         32 << 20, // 32 MiB
		UpstreamTimeout:         5 * time.Minute,
		ModelsDiscoveryTimeout:  30 * time.Second,
		ModelsDiscoveryMaxBytes: 8 << 20,
		ModelsDiscoveryMaxPages: 100,

		MaxConnsPerHost:     512,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,

		MaxRetries:      5,
		QueueRetryAfter: 2 * time.Second,
		BaseBackoff:     1 * time.Second, // start at 1s, double each attempt/failed request, cap at MaxBackoff
		MaxBackoff:      2 * time.Minute,

		QualityRetries: 1,

		StormEnabled:           false,
		StormProviderEnabled:   true,
		StormModelEnabled:      true,
		StormBannerEnabled:     true,
		StormWindow:            time.Minute,
		StormMinSamples:        20,
		StormErrorPercent:      50,
		StormInitialBackoff:    5 * time.Second,
		StormMaxBackoff:        2 * time.Minute,
		StormBackoffMultiplier: 2,
		StormJitterPercent:     20,
		StormRecoverySuccesses: 2,
		StormMaxQueue:          1000,
		StormMaxWait:           5 * time.Minute,
		StormMaxScopes:         2048,
		StormMaxRetries:        20,
		StormStatusCodes:       []string{"500", "502", "503", "504"},
		StormTransportErrors:   true,
		StormStreamErrors:      true,

		AutoTokenRefresh: true,

		// The shipped canonicalization pipeline is data, not semantics: fold
		// case, strip vendor/ and :tag, unify digit dots. Every step is
		// editable/removable in Settings; an explicitly empty list groups by
		// the exact stored spelling (presence-based slice merge).
		ModelRules: DefaultModelRules(),

		DebugCaptureTTL:      24 * time.Hour,
		DebugCaptureMaxBytes: 1 << 20, // 1 MiB

		ConversationIdleGap: 30 * time.Minute,
		ConversationMaxOpen: 64,

		AnthropicDefaultMaxTokens:  4096,
		CursorDefaultContextWindow: 0, // 0 = omit; let the client use its own default
		CursorParkTTL:              3 * time.Hour,
		CursorHeartbeatInterval:    5 * time.Second,

		StorageWriteChanCap:  8192,
		StorageBatchCap:      256,
		StorageFlushInterval: 500 * time.Millisecond,
		StorageQueryTimeout:  10 * time.Second,
		StorageQueryMaxBytes: 8 << 20,
		StorageQueryMaxRows:  10000,
		BackupMaxBytes:       1 << 30,

		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,

		SSEKeepaliveInterval: 15 * time.Second,

		DashLogRows:       60,
		DashPollInterval:  5 * time.Second,
		DashChartRefresh:  15 * time.Second,
		DashExplorerStale: 15 * time.Second,
	}
}

// Example returns the shipped configuration, reusing every server default and
// enabling two optional public provider profiles. These noncredential headers
// are compatibility snapshots, not verified or stable provider API contracts.
// Operators can edit or remove them in their private config or Settings.
// Built-in defaults remain provider-neutral; loading no config never adds them.
func Example() *Config {
	c := Default()
	const grokVersion = "1.0.13" // Keep the compatibility identity internally consistent.
	c.Providers = map[string]ProviderOverride{
		"cursor.sh": {
			UsageKeys:  map[string]string{},
			ModelsKeys: map[string]string{},
			Headers: map[string]string{
				"X-Cursor-Agent-Allowed-Tools": "mcp_tool_call",
				"x-cursor-client-type":         "cli",
				"x-cursor-client-version":      "cli-2026.08.11-0000000",
				"x-ghost-mode":                 "true",
				"x-request-id":                 "{{uuid4}}",
			},
		},
		"x.ai": {
			// The documented language-models endpoint reports modalities.
			// Its version field is a model version, never a context length.
			// Cost/usage remain automatic, including USD tick conversion.
			UsageKeys:  map[string]string{},
			ModelsPath: "/language-models",
			ModelsKeys: map[string]string{
				"input_modalities":  "input_modalities",
				"output_modalities": "output_modalities",
			},
			Headers: map[string]string{
				"User-Agent":               "grok-shell/" + grokVersion + " ({{platform}})",
				"x-grok-client-identifier": "grok-shell",
				"x-grok-client-version":    grokVersion,
				"x-grok-req-id":            "{{uuid4}}",
			},
		},
	}
	return c
}

func yamlKeySet(present map[string]any, key string) bool {
	_, ok := present[key]
	return ok
}

// mergeOverlay copies Schema keys that appear in present from src onto c.
// Omitted keys keep Default(). Maps merge by key; slices and scalars replace.
func (c *Config) mergeOverlay(src *Config, present map[string]any) {
	for _, f := range Schema() {
		if !yamlKeySet(present, f.Key) {
			continue
		}
		dst := c.fieldRV(f.Key)
		from := src.fieldRV(f.Key)
		if !dst.IsValid() || !from.IsValid() || !dst.CanSet() {
			continue
		}
		if dst.Kind() == reflect.Map {
			if dst.IsNil() {
				dst.Set(reflect.MakeMap(dst.Type()))
			}
			iter := from.MapRange()
			for iter.Next() {
				dst.SetMapIndex(iter.Key(), iter.Value())
			}
			continue
		}
		dst.Set(from)
	}
}

// Validate checks the merged config for type and range sanity, failing closed
// at the load boundary. Ranges are deliberately permissive guardrails (catch
// nonsense like a negative count, a negative duration, or a zero idle-per-host
// pool that Go would silently turn into 2) - they are not meant to second-guess
// a deliberate operator choice within a sane band.
func (c *Config) Validate() error {
	// strings / addresses
	host, port, err := net.SplitHostPort(c.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 0 || n > 65535 || strings.ContainsAny(host, " /?#@\t\r\n") {
		return fmt.Errorf("listen: must be host:port with a numeric port in 0..65535")
	}
	// allowed_base_urls entries are byte-prefix-matched against request base
	// URLs at runtime (internal/proxy), which are trailing-slash-trimmed and
	// can never carry userinfo, query, or fragment (the proxy rejects those
	// at the request boundary). A scheme-less entry like "api.openai.com"
	// would load cleanly and then silently deny every request - and so would
	// every other dead form: a trailing slash (no trimmed base can start
	// with a longer prefix), userinfo (which also persists credentials in
	// the config), a query/fragment suffix (checked on the raw string: a
	// bare trailing "?" or "#" parses to an empty query/fragment but is
	// exactly as dead, since the request boundary rejects the byte outright),
	// or a non-lowercase host (the match is byte-sensitive, so mixed case
	// only ever matches byte-identical client casing). All rejected here;
	// reachability is a runtime concern, never checked.
	for _, entry := range c.AllowedBaseURLs {
		u, err := url.Parse(entry)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("allowed_base_urls: %q must be an absolute http:// or https:// URL", entry)
		}
		if u.User != nil {
			return fmt.Errorf("allowed_base_urls: %q must not carry userinfo: a request base URL never does, and the entry persists credentials in the config", entry)
		}
		if strings.Contains(entry, "?") {
			return fmt.Errorf("allowed_base_urls: %q must not carry a query: a request base URL never does, so the entry can never match", entry)
		}
		if strings.Contains(entry, "#") {
			return fmt.Errorf("allowed_base_urls: %q must not carry a fragment: a request base URL never does, so the entry can never match", entry)
		}
		if strings.HasSuffix(entry, "/") {
			return fmt.Errorf("allowed_base_urls: %q must not end in \"/\": request base URLs are trailing-slash-trimmed, so the entry can never match", entry)
		}
		if u.Host != strings.ToLower(u.Host) {
			return fmt.Errorf("allowed_base_urls: %q: host must be lowercase (the runtime prefix match is byte-sensitive); use %q",
				entry, u.Scheme+"://"+strings.ToLower(u.Host)+u.EscapedPath())
		}
	}
	// counts / sizes (>= 0; 0 means "unlimited" only where documented)
	if c.HistorySize < 1 || c.HistorySize > 1_000_000 {
		return fmt.Errorf("history_size: must be 1..1000000, got %d", c.HistorySize)
	}
	if err := checkByteSize("max_request_bytes", c.MaxRequestBytes, MaxRequestBytesMin, MaxRequestBytesMax); err != nil {
		return err
	}
	if c.DebugCaptureTTL < DebugCaptureTTLMin || c.DebugCaptureTTL > DebugCaptureTTLMax {
		return fmt.Errorf("debug_capture_ttl: must be %s..%s, got %s", FormatDuration(DebugCaptureTTLMin), FormatDuration(DebugCaptureTTLMax), FormatDuration(c.DebugCaptureTTL))
	}
	if err := checkByteSize("debug_capture_max_bytes", c.DebugCaptureMaxBytes, DebugCaptureMaxBytesMin, DebugCaptureMaxBytesMax); err != nil {
		return err
	}
	if c.MaxConnsPerHost < 0 || c.MaxIdleConns < 0 {
		return fmt.Errorf("max_conns_per_host/max_idle_conns must be >= 0 (0 = unlimited)")
	}
	// net/http.Transport treats MaxIdleConnsPerHost == 0 as DefaultMaxIdleConnsPerHost
	// (2), not unlimited - so 0 is a silent dual-default, never a valid setting.
	if c.MaxIdleConnsPerHost < 1 {
		return fmt.Errorf("max_idle_conns_per_host: must be >= 1, got %d", c.MaxIdleConnsPerHost)
	}
	if c.MaxConnsPerHost > 0 && c.MaxIdleConnsPerHost > c.MaxConnsPerHost {
		return fmt.Errorf("max_idle_conns_per_host (%d) cannot exceed max_conns_per_host (%d)", c.MaxIdleConnsPerHost, c.MaxConnsPerHost)
	}
	if c.MaxConcurrent < 0 || c.MaxQueueSize < 0 || c.MaxRetries < 0 {
		return fmt.Errorf("max_concurrent/max_queue_size/max_retries: must be >= 0")
	}
	if c.QualityRetries < 0 || c.QualityRetries > QualityRetriesMax {
		return fmt.Errorf("quality_retries: must be 0..%d, got %d", QualityRetriesMax, c.QualityRetries)
	}
	if err := checkQueueRetryAfter(c.QueueRetryAfter); err != nil {
		return err
	}
	if c.ConversationMaxOpen < 1 || c.ConversationMaxOpen > 100000 {
		return fmt.Errorf("conversation_max_open: must be 1..100000, got %d", c.ConversationMaxOpen)
	}
	if c.AnthropicDefaultMaxTokens < 1 || c.AnthropicDefaultMaxTokens > 1_000_000 {
		return fmt.Errorf("anthropic_default_max_tokens: must be 1..1000000, got %d", c.AnthropicDefaultMaxTokens)
	}
	if c.CursorDefaultContextWindow < 0 {
		return fmt.Errorf("cursor_default_context_window: must be >= 0, got %d", c.CursorDefaultContextWindow)
	}
	if c.CursorParkTTL <= 0 {
		return fmt.Errorf("cursor_park_ttl: must be > %s", FormatDuration(0))
	}
	if c.CursorHeartbeatInterval < HeartbeatIntervalMin || c.CursorHeartbeatInterval > HeartbeatIntervalMax {
		return fmt.Errorf("cursor_heartbeat_interval: must be %s, got %s", heartbeatRange(), FormatDuration(c.CursorHeartbeatInterval))
	}
	if c.SSEKeepaliveInterval < HeartbeatIntervalMin || c.SSEKeepaliveInterval > HeartbeatIntervalMax {
		return fmt.Errorf("sse_keepalive_interval: must be %s, got %s", heartbeatRange(), FormatDuration(c.SSEKeepaliveInterval))
	}
	if c.DashLogRows < DashLogRowsMin || c.DashLogRows > DashLogRowsMax {
		return fmt.Errorf("dash_log_rows: must be %d..%d, got %d", DashLogRowsMin, DashLogRowsMax, c.DashLogRows)
	}
	if c.DashPollInterval <= 0 {
		return fmt.Errorf("dash_poll_interval: must be > %s", FormatDuration(0))
	}
	if c.DashChartRefresh <= 0 {
		return fmt.Errorf("dash_chart_refresh: must be > %s", FormatDuration(0))
	}
	if c.DashExplorerStale <= 0 {
		return fmt.Errorf("dash_explorer_stale: must be > %s", FormatDuration(0))
	}
	if c.StorageWriteChanCap < 1 || c.StorageBatchCap < 1 || c.StorageBatchCap > c.StorageWriteChanCap {
		return fmt.Errorf("storage_batch_cap (%d) must be 1..storage_write_chan_cap (%d)", c.StorageBatchCap, c.StorageWriteChanCap)
	}
	if err := validateModelsDiscovery(c); err != nil {
		return err
	}
	if err := validateQueryLimits(c); err != nil {
		return err
	}
	if err := checkByteSize("backup_max_bytes", c.BackupMaxBytes, BackupMaxBytesMin, BackupMaxBytesMax); err != nil {
		return err
	}
	if err := validateStorm(c); err != nil {
		return err
	}
	// durations: negatives are always invalid. 0 is meaningful where documented
	// (unlimited / disabled); heartbeat/SSE floors are 1ns, not 1ms.
	durs := map[string]time.Duration{
		"shutdown_timeout":       c.ShutdownTimeout,
		"restart_drain_timeout":  c.RestartDrainTimeout,
		"upstream_timeout":       c.UpstreamTimeout,
		"idle_conn_timeout":      c.IdleConnTimeout,
		"max_queue_wait":         c.MaxQueueWait,
		"base_backoff":           c.BaseBackoff,
		"max_backoff":            c.MaxBackoff,
		"conversation_idle_gap":  c.ConversationIdleGap,
		"storage_flush_interval": c.StorageFlushInterval,
		"storage_query_timeout":  c.StorageQueryTimeout,
		"read_header_timeout":    c.ReadHeaderTimeout,
		"idle_timeout":           c.IdleTimeout,
		"debug_capture_ttl":      c.DebugCaptureTTL,
	}
	for _, name := range sortedKeys(durs) {
		if d := durs[name]; d < 0 {
			return fmt.Errorf("%s: duration must be >= 0, got %s", name, FormatDuration(d))
		}
	}
	// 0 is not a valid setting for these: ticker/context/backoff would panic
	// or disable the mechanism with no documented ZeroMeans.
	posDurs := []struct {
		name string
		d    time.Duration
	}{
		{"shutdown_timeout", c.ShutdownTimeout},
		{"base_backoff", c.BaseBackoff},
		{"max_backoff", c.MaxBackoff},
		{"conversation_idle_gap", c.ConversationIdleGap},
		{"storage_flush_interval", c.StorageFlushInterval},
		{"storage_query_timeout", c.StorageQueryTimeout},
	}
	for _, f := range posDurs {
		if f.d <= 0 {
			return fmt.Errorf("%s: must be > %s, got %s", f.name, FormatDuration(0), FormatDuration(f.d))
		}
	}
	// base_backoff must not exceed max_backoff (the retry loop would clamp
	// immediately, making the base meaningless).
	if c.BaseBackoff > c.MaxBackoff {
		return fmt.Errorf("base_backoff (%s) cannot exceed max_backoff (%s)", FormatDuration(c.BaseBackoff), FormatDuration(c.MaxBackoff))
	}
	// provider_aliases must be a well-formed one-hop rename map: both sides
	// non-empty, no self-mapping, no chains. The map is applied in a single
	// pass (capture-time + one UPDATE per pair), so a chain could never
	// resolve and would silently split the label again - rejected, not
	// resolved.
	for _, from := range sortedKeys(c.ProviderAliases) {
		to := c.ProviderAliases[from]
		if strings.TrimSpace(from) == "" {
			return fmt.Errorf("provider_aliases: alias label must be non-empty")
		}
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("provider_aliases: %s: target label must be non-empty", from)
		}
		if from == to {
			return fmt.Errorf("provider_aliases: %s: cannot alias a label to itself", from)
		}
		if _, chained := c.ProviderAliases[to]; chained {
			return fmt.Errorf("provider_aliases: %s → %s: target is itself aliased; chains are not resolved", from, to)
		}
	}
	// model_rules is validated at this boundary (modes, pattern
	// compilability, length cap) - see modelcanon.go.
	if err := ValidateModelRules(c.ModelRules); err != nil {
		return err
	}
	// providers.<label> field maps: models_path must be a clean path
	// (it is joined onto the base URL with the same path-joiner the models
	// route uses - a query/fragment or spaces would corrupt the join);
	// cost_keys, usage_keys, and models_keys values must be dotted JSON
	// paths (one provPathRE rule shared by all three); usage_keys keys must
	// be canonical usage fields (the Settings dropdown and ParseUsage
	// consume exactly those - anything else is a silent runtime no-op).
	// Denied at load, never clamped. Labels (and the nested map keys) are
	// visited in sorted order so the first reported error is deterministic
	// when several entries are invalid.
	for _, label := range sortedKeys(c.Providers) {
		p := c.Providers[label]
		// The label is the runtime join key - the provider name derived
		// from the request's base URL, never empty. An empty or
		// whitespace-only label can never match one, so its whole section
		// would be a silent no-op. Same rule as the Settings POST path
		// (asProviders in values.go), enforced here because Validate is
		// the one choke point every load path (YAML and Settings POST)
		// passes through.
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("providers: label must be non-empty")
		}
		if p.ModelsPath != "" {
			if !strings.HasPrefix(p.ModelsPath, "/") {
				return fmt.Errorf("providers.%s.models_path %q: must start with /", label, p.ModelsPath)
			}
			if strings.ContainsAny(p.ModelsPath, "?# \t\r\n") {
				return fmt.Errorf("providers.%s.models_path %q: query, fragment, and whitespace are not allowed in a path", label, p.ModelsPath)
			}
		}
		for _, path := range p.CostKeys {
			if err := checkProvPath(label, "cost_keys", "", path); err != nil {
				return err
			}
		}
		for _, field := range sortedKeys(p.UsageKeys) {
			if !canonicalUsageFieldSet[field] {
				return fmt.Errorf("providers.%s.usage_keys: %q is not a canonical usage field (one of: %s)", label, field, strings.Join(metrics.CanonicalUsageFields, ", "))
			}
			if err := checkProvPath(label, "usage_keys", field, p.UsageKeys[field]); err != nil {
				return err
			}
		}
		for _, field := range sortedKeys(p.ModelsKeys) {
			if strings.TrimSpace(field) == "" {
				return fmt.Errorf("providers.%s.models_keys: output field name must be non-empty", label)
			}
			if err := checkProvPath(label, "models_keys", field, p.ModelsKeys[field]); err != nil {
				return err
			}
		}
		names := sortedKeys(p.Headers)
		if err := ValidateHeaderNames(names); err != nil {
			return fmt.Errorf("providers.%s.headers: %w", label, err)
		}
		for _, name := range names {
			if !validHeaderValue(p.Headers[name]) {
				return fmt.Errorf("providers.%s.headers: %s: value must be a single-line printable header value", label, name)
			}
		}
	}
	return nil
}

// ValidHeaderName is the shared HTTP field-name token rule for configuration
// and per-request routing controls.
func ValidHeaderName(name string) bool { return httpguts.ValidHeaderFieldName(name) }

// ValidateHeaderNames rejects ambiguous authorities before case-insensitive
// HTTP canonicalization can let map iteration choose which value wins.
func ValidateHeaderNames(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !ValidHeaderName(name) {
			return fmt.Errorf("%q is not a valid header name (RFC 7230 token)", name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate HTTP header name %q", name)
		}
		seen[key] = true
	}
	return nil
}

// validHeaderValue reports value as a single-line header value: printable
// ASCII plus tab (an RFC 7230 field-value), never empty - an empty mapping
// is meaningless and would silently send nothing.
func validHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r == '\r' || r == '\n' || r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return false
		}
	}
	return true
}

// userFile is LoadFile's strict decode target: every Config key plus the
// upstream_timeout text probe. Config declares UpstreamTimeout yaml:"-"
// (a YAML integer would decode as nanoseconds on time.Duration), so the
// probe field makes the key known to the strict decoder while LoadFile still
// parses the scalar as a duration string. Overlay rejects a bare integer.
type userFile struct {
	Config          `yaml:",inline"`
	UpstreamTimeout *string `yaml:"upstream_timeout"`
}

const (
	skippedExtraDocument = "(extra yaml document)"
	skippedUnparseable   = "(unparseable yaml)"
	invalidConfigSuffix  = ".invalid"
)

// configParseError is a YAML syntax/shape failure. LoadFileRepair moves the
// original aside and writes Default(); other load errors fail closed.
type configParseError struct {
	Path string
	Err  error
}

func (e *configParseError) Error() string {
	return fmt.Sprintf("parse config %s: %s", e.Path, e.Err)
}

func (e *configParseError) Unwrap() error { return e.Err }

func parseConfig(path string, err error) error {
	return &configParseError{Path: path, Err: err}
}

// LoadFile reads the YAML config at path (may be empty) and merges recognized,
// valid keys over the built-in defaults. A missing file yields the defaults.
// Unknown keys, invalid types/ranges, and extra YAML documents are omitted so
// a typo cannot block boot; Settings POST still rejects those inputs. This
// read path does not rewrite the file.
func LoadFile(path string) (*Config, error) {
	cfg, _, err := loadYAMLFile(path)
	return cfg, err
}

// LoadBytes is LoadFile for an in-memory YAML document. Used by backup
// restore so a snapshot is admitted through the same overlay/Validate choke
// as a file, without writing it first.
func LoadBytes(b []byte) (*Config, []string, error) {
	return loadYAMLBytes("backup", b)
}

// LoadFileRepair is LoadFile, then persists a cleaned document when anything
// was dropped. Unparseable files are moved aside to path+invalidConfigSuffix
// and replaced with Default(). Healthcheck and print-only paths must not call this.
func LoadFileRepair(path string) (*Config, []string, error) {
	cfg, skipped, err := loadYAMLFile(path)
	if err != nil {
		var parseErr *configParseError
		if path == "" || !errors.As(err, &parseErr) {
			return nil, nil, err
		}
		invalidPath := path + invalidConfigSuffix
		_ = os.Remove(invalidPath)
		if rerr := os.Rename(path, invalidPath); rerr != nil {
			return nil, nil, fmt.Errorf("repair config %s: %w", path, rerr)
		}
		cfg = Default()
		if werr := WriteFile(path, cfg); werr != nil {
			return nil, nil, fmt.Errorf("repair config %s: %w", path, werr)
		}
		return cfg, []string{skippedUnparseable}, nil
	}
	if len(skipped) > 0 && path != "" {
		if err := WriteFile(path, cfg); err != nil {
			return cfg, skipped, fmt.Errorf("repair config %s: %w", path, err)
		}
	}
	return cfg, skipped, nil
}

func loadYAMLFile(path string) (*Config, []string, error) {
	def := Default()
	if path == "" {
		return def, nil, def.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil, nil
		}
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return loadYAMLBytes(path, b)
}

func loadYAMLBytes(path string, b []byte) (*Config, []string, error) {
	def := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, parseConfig(path, err)
	}
	var skipped []string
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		skipped = append(skipped, skippedExtraDocument)
	}
	mapping, err := yamlDocumentMapping(&first)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	present, nodes, err := yamlMappingValues(mapping)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	if len(present) == 0 {
		return def, skipped, nil
	}
	keys := make([]string, 0, len(present))
	for key := range present {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var accepted []string
	for _, key := range keys {
		if FieldByKey(key) == nil {
			skipped = append(skipped, key)
			continue
		}
		loose, err := overlayYAMLKey(def.Clone(), key, nodes[key], present[key])
		if err != nil {
			skipped = append(skipped, key)
			continue
		}
		if loose {
			skipped = append(skipped, key+" (unknown fields)")
		}
		accepted = append(accepted, key)
	}
	cfg, dropped := keepYAMLKeys(def, present, nodes, accepted)
	skipped = append(skipped, dropped...)
	return cfg, skipped, nil
}

func yamlResolved(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func yamlDocumentMapping(n *yaml.Node) (*yaml.Node, error) {
	n = yamlResolved(n)
	if n == nil || n.Kind == 0 {
		return nil, nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil, nil
		}
		n = yamlResolved(n.Content[0])
	}
	if n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("document must be a YAML mapping")
	}
	return n, nil
}

func yamlMappingValues(n *yaml.Node) (map[string]any, map[string]*yaml.Node, error) {
	present := map[string]any{}
	nodes := map[string]*yaml.Node{}
	if n == nil {
		return present, nodes, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := yamlResolved(n.Content[i])
		valNode := yamlResolved(n.Content[i+1])
		if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
			return nil, nil, fmt.Errorf("document must be a YAML mapping")
		}
		key := keyNode.Value
		var v any
		if valNode == nil {
			present[key] = nil
			nodes[key] = n.Content[i+1]
			continue
		}
		if err := valNode.Decode(&v); err != nil {
			return nil, nil, err
		}
		present[key] = v
		nodes[key] = valNode
	}
	return present, nodes, nil
}

func keepYAMLKeys(def *Config, present map[string]any, nodes map[string]*yaml.Node, accepted []string) (*Config, []string) {
	kept := make([]string, 0, len(accepted))
	cfg := def.Clone()
	try := func(key string) bool {
		trial := append(append([]string{}, kept...), key)
		merged, err := mergeYAMLKeys(def, present, nodes, trial)
		if err != nil {
			return false
		}
		kept = trial
		cfg = merged
		return true
	}
	var deferred []string
	for _, key := range accepted {
		if !try(key) {
			deferred = append(deferred, key)
		}
	}
	var skipped []string
	for _, key := range deferred {
		if !try(key) {
			skipped = append(skipped, key)
		}
	}
	return cfg, skipped
}

func mergeYAMLKeys(base *Config, present map[string]any, nodes map[string]*yaml.Node, keys []string) (*Config, error) {
	cfg := base.Clone()
	for _, key := range keys {
		if _, err := overlayYAMLKey(cfg, key, nodes[key], present[key]); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeUserOverlay(raw []byte, strict bool) (*userFile, error) {
	var user userFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(strict)
	if err := dec.Decode(&user); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if user.UpstreamTimeout != nil {
		d, err := parseYAMLDuration(*user.UpstreamTimeout)
		if err != nil {
			return nil, err
		}
		user.Config.UpstreamTimeout = d
	}
	return &user, nil
}

func overlayYAMLKey(dst *Config, key string, valueNode *yaml.Node, value any) (loose bool, err error) {
	field := FieldByKey(key)
	if field == nil {
		return false, fmt.Errorf("unknown config key %q", key)
	}
	if err := checkYAMLType(*field, value); err != nil {
		return false, err
	}
	if valueNode == nil {
		return false, fmt.Errorf("%s: missing yaml node", key)
	}
	raw, err := yaml.Marshal(&yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			valueNode,
		},
	})
	if err != nil {
		return false, err
	}
	user, err := decodeUserOverlay(raw, true)
	if err != nil {
		user, err = decodeUserOverlay(raw, false)
		if err != nil {
			return false, err
		}
		loose = true
	}
	dst.mergeOverlay(&user.Config, map[string]any{key: value})
	return loose, nil
}

// parseYAMLDuration accepts Go duration strings ("5m", "0s"). Settings typed
// zeros go through asDuration, not this YAML probe.
func parseYAMLDuration(s string) (time.Duration, error) {
	return time.ParseDuration(strings.TrimSpace(s))
}

func checkQueueRetryAfter(d time.Duration) error {
	if d < 0 || d > QueueRetryAfterMax || d%time.Second != 0 {
		return fmt.Errorf("queue_retry_after: must be a whole-second duration %s..%s, got %s", FormatDuration(0), FormatDuration(QueueRetryAfterMax), FormatDuration(d))
	}
	return nil
}

// RetryAfterSeconds is the integer-seconds Retry-After value for proxy-owned
// 429s. HTTP Retry-After is delta-seconds; this is the only conversion.
func (c *Config) RetryAfterSeconds() int {
	if c == nil || c.QueueRetryAfter <= 0 {
		return 0
	}
	return int(c.QueueRetryAfter / time.Second)
}

// validateModelsDiscovery checks the one-request metadata budget.
func validateModelsDiscovery(c *Config) error {
	for _, limit := range [...]struct {
		key             string
		value, min, max int64
	}{
		{"models_discovery_timeout", int64(c.ModelsDiscoveryTimeout), int64(ModelsDiscoveryTimeoutMin), int64(ModelsDiscoveryTimeoutMax)},
		{"models_discovery_max_bytes", int64(c.ModelsDiscoveryMaxBytes), ModelsDiscoveryMaxBytesMin, ModelsDiscoveryMaxBytesMax},
		{"models_discovery_max_pages", int64(c.ModelsDiscoveryMaxPages), ModelsDiscoveryMaxPagesMin, ModelsDiscoveryMaxPagesMax},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			if limit.key == "models_discovery_timeout" {
				return fmt.Errorf("%s: must be %s..%s, got %s", limit.key, FormatDuration(ModelsDiscoveryTimeoutMin), FormatDuration(ModelsDiscoveryTimeoutMax), FormatDuration(c.ModelsDiscoveryTimeout))
			}
			if limit.key == "models_discovery_max_bytes" {
				return checkByteSize(limit.key, ByteSize(limit.value), limit.min, limit.max)
			}
			return fmt.Errorf("%s: must be %d..%d, got %d", limit.key, limit.min, limit.max, limit.value)
		}
	}
	return nil
}

func validateQueryLimits(c *Config) error {
	for _, limit := range [...]struct {
		key             string
		value, min, max int
	}{
		{"storage_query_max_bytes", int(c.StorageQueryMaxBytes), StorageQueryMaxBytesMin, StorageQueryMaxBytesMax},
		{"storage_query_max_rows", c.StorageQueryMaxRows, StorageQueryMaxRowsMin, StorageQueryMaxRowsMax},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			if limit.key == "storage_query_max_bytes" {
				return checkByteSize(limit.key, ByteSize(limit.value), int64(limit.min), int64(limit.max))
			}
			return fmt.Errorf("%s: must be %d..%d, got %d", limit.key, limit.min, limit.max, limit.value)
		}
	}
	return nil
}

func validateStorm(c *Config) error {
	for _, limit := range [...]struct {
		key             string
		value, min, max int64
	}{
		{"storm_window", int64(c.StormWindow), int64(time.Second), int64(time.Hour)},
		{"storm_min_samples", int64(c.StormMinSamples), 1, 1_000_000},
		{"storm_error_percent", int64(c.StormErrorPercent), 1, 100},
		{"storm_initial_backoff", int64(c.StormInitialBackoff), int64(time.Millisecond), int64(time.Hour)},
		{"storm_max_backoff", int64(c.StormMaxBackoff), int64(time.Millisecond), int64(time.Hour)},
		{"storm_backoff_multiplier", int64(c.StormBackoffMultiplier), 2, 10},
		{"storm_jitter_percent", int64(c.StormJitterPercent), 0, 100},
		{"storm_recovery_successes", int64(c.StormRecoverySuccesses), 1, 100},
		{"storm_max_queue", int64(c.StormMaxQueue), 1, 100000},
		{"storm_max_wait", int64(c.StormMaxWait), int64(time.Millisecond), int64(24 * time.Hour)},
		{"storm_max_scopes", int64(c.StormMaxScopes), 2, 100000},
		{"storm_max_retries", int64(c.StormMaxRetries), 0, 1000},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			field := FieldByKey(limit.key)
			return fmt.Errorf("%s: must be %s..%s, got %s", limit.key,
				field.formatBound(float64(limit.min)), field.formatBound(float64(limit.max)), field.formatBound(float64(limit.value)))
		}
	}
	if c.StormInitialBackoff > c.StormMaxBackoff {
		return fmt.Errorf("storm_initial_backoff (%s) cannot exceed storm_max_backoff (%s)", FormatDuration(c.StormInitialBackoff), FormatDuration(c.StormMaxBackoff))
	}
	seen := make(map[string]bool, len(c.StormStatusCodes))
	for _, code := range c.StormStatusCodes {
		if code != "429" && (len(code) != 3 || code[0] != '5' || code[1] < '0' || code[1] > '9' || code[2] < '0' || code[2] > '9') {
			return fmt.Errorf("storm_status_codes: %q must be exactly 429 or 500..599", code)
		}
		if seen[code] {
			return fmt.Errorf("storm_status_codes: duplicate %q", code)
		}
		seen[code] = true
	}
	return nil
}

// checkYAMLType is the YAML type gate. Ranges and cross-field rules live in
// Validate, which keepYAMLKeys runs on the merged overlay. Schema Kind is
// the type owner; yaml.v3 must not coerce floats, nulls, or 1.1 bool words.
func checkYAMLType(field Field, v any) error {
	switch field.Kind {
	case KindInt:
		switch v.(type) {
		case int, int64, uint64:
		default:
			return fmt.Errorf("%s: must be an integer", field.Key)
		}
	case KindBytes:
		switch v.(type) {
		case int, int64, uint64, string, ByteSize:
		default:
			return fmt.Errorf("%s: must be a byte size or integer", field.Key)
		}
		if _, err := parseByteSize(v); err != nil {
			return fmt.Errorf("%s: %w", field.Key, err)
		}
	case KindDuration:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: must be a duration string", field.Key)
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: must be a boolean", field.Key)
		}
	case KindString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: must be a string", field.Key)
		}
	case KindStrings:
		switch items := v.(type) {
		case []string:
		case []any:
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s: every item must be a string", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list of strings", field.Key)
		}
	case KindAliases:
		switch m := v.(type) {
		case map[string]string:
		case map[string]any:
			for _, item := range m {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s: every value must be a string", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a map of strings", field.Key)
		}
	case KindProviders:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: must be a map", field.Key)
		}
		for _, item := range m {
			if _, ok := item.(map[string]any); !ok {
				return fmt.Errorf("%s: each provider must be a map", field.Key)
			}
		}
	case KindModelRules:
		switch items := v.(type) {
		case []ModelRule:
		case []any:
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok {
					return fmt.Errorf("%s: every item must be a map", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list", field.Key)
		}
	default:
		return fmt.Errorf("%s: unsupported kind %q", field.Key, field.Kind)
	}
	return nil
}
