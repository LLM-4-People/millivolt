package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"golang.org/x/net/http/httpguts"
)

// ValidURLPort accepts an absent port or a decimal TCP port 1-65535. The
// allowed-base-url loader and the request routing boundary share it so a
// loaded allowlist entry can always match a request base URL that the boundary
// accepts.
func ValidURLPort(port string) bool {
	if port == "" {
		return true
	}
	if len(port) > 5 {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
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
		// The runtime prefix match is byte-sensitive, so an entry that only
		// parses after url.Parse folds the scheme to lowercase could match
		// only a client base URL spelled with the same uppercase scheme.
		if !strings.HasPrefix(entry, "http://") && !strings.HasPrefix(entry, "https://") {
			return fmt.Errorf("allowed_base_urls: %q must start with lowercase http:// or https://: the runtime prefix match is byte-sensitive", entry)
		}
		if !ValidURLPort(u.Port()) {
			return fmt.Errorf("allowed_base_urls: %q must use a port between 1 and 65535", entry)
		}
		// "http://:8080" parses with a port but no hostname. No request base
		// URL can be hostless (the request boundary rejects it), so the entry
		// could never match; fail closed at load instead of accepting a dead
		// allowlist rule.
		if u.Hostname() == "" {
			return fmt.Errorf("allowed_base_urls: %q must name a host: a port without a hostname can never match a request base URL", entry)
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
	if c.MemoryLimit != 0 {
		if err := checkByteSize("go_memory_limit", c.MemoryLimit, MemoryLimitMin, MemoryLimitMax); err != nil {
			return err
		}
	}
	if c.DebugCaptureTTL < DebugCaptureTTLMin || c.DebugCaptureTTL > DebugCaptureTTLMax {
		return errRange("debug_capture_ttl", FormatDuration(DebugCaptureTTLMin), FormatDuration(DebugCaptureTTLMax), FormatDuration(c.DebugCaptureTTL))
	}
	if c.DebugStoreMaxBytes != 0 {
		if err := checkByteSize("debug_store_max_bytes", c.DebugStoreMaxBytes, DebugStoreMaxBytesMin, DebugStoreMaxBytesMax); err != nil {
			return err
		}
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
	if c.MaxInflightRequests < 0 || c.MaxInflightRequests > 1_000_000 {
		return fmt.Errorf("max_inflight_requests: must be 0..1000000, got %d", c.MaxInflightRequests)
	}
	if c.MaxInflightBodyBytes != 0 {
		if err := checkByteSize("max_inflight_body_bytes", c.MaxInflightBodyBytes, MaxInflightBodyBytesMin, MaxInflightBodyBytesMax); err != nil {
			return err
		}
	}
	if c.MaxInflightResponseBytes != 0 {
		if err := checkByteSize("max_inflight_response_bytes", c.MaxInflightResponseBytes, MaxInflightResponseBytesMin, MaxInflightResponseBytesMax); err != nil {
			return err
		}
	}
	if c.QualityRetries < 0 || c.QualityRetries > QualityRetriesMax {
		return fmt.Errorf("quality_retries: must be 0..%d, got %d", QualityRetriesMax, c.QualityRetries)
	}
	if c.ThinkingRetries < 0 || c.ThinkingRetries > ThinkingRetriesMax {
		return fmt.Errorf("thinking_retries: must be 0..%d, got %d", ThinkingRetriesMax, c.ThinkingRetries)
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
	switch c.TranslationLoss {
	case TranslationLossWarn, TranslationLossReject:
	default:
		return fmt.Errorf("translation_loss: %q must be one of %s", c.TranslationLoss, translationLossPolicies())
	}
	if c.CursorDefaultContextWindow < 0 {
		return fmt.Errorf("cursor_default_context_window: must be >= 0, got %d", c.CursorDefaultContextWindow)
	}
	if c.CursorParkTTL <= 0 {
		return fmt.Errorf("cursor_park_ttl: must be > %s", FormatDuration(0))
	}
	if c.CursorHeartbeatInterval < HeartbeatIntervalMin || c.CursorHeartbeatInterval > HeartbeatIntervalMax {
		return errRange("cursor_heartbeat_interval",
			FormatDuration(HeartbeatIntervalMin), FormatDuration(HeartbeatIntervalMax), FormatDuration(c.CursorHeartbeatInterval))
	}
	if c.SSEKeepaliveInterval < HeartbeatIntervalMin || c.SSEKeepaliveInterval > HeartbeatIntervalMax {
		return errRange("sse_keepalive_interval",
			FormatDuration(HeartbeatIntervalMin), FormatDuration(HeartbeatIntervalMax), FormatDuration(c.SSEKeepaliveInterval))
	}
	if c.DashLogRows < DashLogRowsMin || c.DashLogRows > DashLogRowsMax {
		return errRange("dash_log_rows",
			strconv.FormatInt(int64(DashLogRowsMin), 10), strconv.FormatInt(int64(DashLogRowsMax), 10), strconv.FormatInt(int64(c.DashLogRows), 10))
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
	if c.StorageWriteChanCap < 1 || c.StorageWriteChanCap > storageWriteChanCapMax {
		return fmt.Errorf("storage_write_chan_cap: must be 1..%d, got %d", storageWriteChanCapMax, c.StorageWriteChanCap)
	}
	if c.StorageBatchCap < 1 || c.StorageBatchCap > c.StorageWriteChanCap {
		return fmt.Errorf("storage_batch_cap (%d) must be 1..storage_write_chan_cap (%d)", c.StorageBatchCap, c.StorageWriteChanCap)
	}
	if err := validateModelsDiscovery(c); err != nil {
		return err
	}
	if err := validateQueryLimits(c); err != nil {
		return err
	}
	if err := validateMCPLimits(c); err != nil {
		return err
	}
	if err := checkByteSize("backup_max_bytes", c.BackupMaxBytes, BackupMaxBytesMin, BackupMaxBytesMax); err != nil {
		return err
	}
	if err := validateStorm(c); err != nil {
		return err
	}
	if err := validateQuotaPause(c); err != nil {
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
		"mcp_query_timeout":      c.MCPQueryTimeout,
		"mcp_timeout":            c.MCPTimeout,
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
		{"mcp_query_timeout", c.MCPQueryTimeout},
		{"mcp_timeout", c.MCPTimeout},
		{"request_body_idle_timeout", c.RequestBodyIdleTimeout},
		{"request_body_read_timeout", c.RequestBodyReadTimeout},
		{"response_write_timeout", c.ResponseWriteTimeout},
		{"sse_write_progress_timeout", c.SSEWriteProgressTimeout},
		{"upstream_stream_idle_timeout", c.UpstreamStreamIdleTimeout},
		{"whole_document_read_timeout", c.WholeDocumentReadTimeout},
		{"whole_document_read_max", c.WholeDocumentReadMax},
		{"upstream_dial_timeout", c.UpstreamDialTimeout},
		{"upstream_dial_keepalive", c.UpstreamDialKeepAlive},
		{"upstream_tls_handshake_timeout", c.UpstreamTLSHandshakeTimeout},
	}
	for _, f := range posDurs {
		if f.d <= 0 {
			return fmt.Errorf("%s: must be > %s, got %s", f.name, FormatDuration(0), FormatDuration(f.d))
		}
	}
	if c.RequestBodyIdleTimeout > c.RequestBodyReadTimeout {
		return fmt.Errorf("request_body_idle_timeout (%s) cannot exceed request_body_read_timeout (%s)",
			FormatDuration(c.RequestBodyIdleTimeout), FormatDuration(c.RequestBodyReadTimeout))
	}
	if c.SSEWriteProgressTimeout > c.ResponseWriteTimeout {
		return fmt.Errorf("sse_write_progress_timeout (%s) cannot exceed response_write_timeout (%s)",
			FormatDuration(c.SSEWriteProgressTimeout), FormatDuration(c.ResponseWriteTimeout))
	}
	if c.WholeDocumentReadMax < c.WholeDocumentReadTimeout {
		return fmt.Errorf("whole_document_read_max (%s) cannot be shorter than whole_document_read_timeout (%s)",
			FormatDuration(c.WholeDocumentReadMax), FormatDuration(c.WholeDocumentReadTimeout))
	}
	if c.WholeDocumentReadMax < c.ModelsDiscoveryTimeout {
		return fmt.Errorf("whole_document_read_max (%s) cannot be shorter than models_discovery_timeout (%s)",
			FormatDuration(c.WholeDocumentReadMax), FormatDuration(c.ModelsDiscoveryTimeout))
	}
	// base_backoff must not exceed max_backoff (the retry loop would clamp
	// immediately, making the base meaningless).
	if c.BaseBackoff > c.MaxBackoff {
		return fmt.Errorf("base_backoff (%s) cannot exceed max_backoff (%s)", FormatDuration(c.BaseBackoff), FormatDuration(c.MaxBackoff))
	}
	// provider_aliases must be a well-formed one-hop rename map: both sides
	// are canonicalized (trimmed + lowercase) before these checks, so a padded
	// pair cannot pass validation and then split history. Both sides must be
	// non-empty, no self-mapping, no chains. The map is applied in a single pass
	// (capture-time + one UPDATE per pair), so a chain could never resolve and
	// would silently split the label again - rejected, not resolved.
	aliases, err := normalizeProviderAliases(c.ProviderAliases)
	if err != nil {
		return fmt.Errorf("provider_aliases: %w", err)
	}
	c.ProviderAliases = aliases
	for _, from := range sortedKeys(c.ProviderAliases) {
		to := c.ProviderAliases[from]
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
	// request_overrides is normalized and validated at this boundary
	// (scope, header grammar and ownership, body band, cap) - the same
	// gate the Settings POST passes through.
	if err := validateRequestOverrides(c); err != nil {
		return err
	}
	// sub_conversations is normalized and validated at this boundary
	// (client, params grammar, caps, duplicate clients) - the same gate
	// the Settings POST passes through.
	if err := validateSubConversations(c); err != nil {
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
		// from the request's base URL, never empty and always lowercase
		// with no surrounding whitespace. A padded or mixed-case label can
		// never match one, so its whole section would be a silent no-op.
		// Same canonical-label rule as provider_aliases, enforced here
		// because Validate is the one choke point every load path (YAML and
		// Settings POST) passes through; asProviders only checks the JSON
		// shape.
		if err := validateProviderLabel(label); err != nil {
			return fmt.Errorf("providers: label %w", err)
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
		// ensure_tools names relay into the upstream tools array verbatim;
		// an empty or duplicated name would be a silent no-op or a double
		// stub. Denied at load, never clamped.
		seenTools := make(map[string]bool, len(p.EnsureTools))
		for _, name := range p.EnsureTools {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("providers.%s.ensure_tools: tool name must be non-empty", label)
			}
			if seenTools[name] {
				return fmt.Errorf("providers.%s.ensure_tools: duplicate tool name %q", label, name)
			}
			seenTools[name] = true
		}
		names := sortedKeys(p.Headers)
		if err := ValidateHeaderNames(names); err != nil {
			return fmt.Errorf("providers.%s.headers: %w", label, err)
		}
		for _, name := range names {
			// An empty mapping value is meaningless and would silently send
			// nothing: the non-empty rule is this mapping's policy, not part
			// of the shared grammar below.
			if value := p.Headers[name]; value == "" || !ValidHeaderValue(value) {
				return fmt.Errorf("providers.%s.headers: %s: value must be a single-line printable header value", label, name)
			}
		}
	}
	return c.validateDocumentSize()
}

// validateDocumentSize enforces the config-document retention bound on the
// canonical YAML the backup encoder retains. Rendering the whole document here
// is acceptable because config changes are not a hot path, and using the same
// writer keeps the validation and archive boundaries from drifting.
func (c *Config) validateDocumentSize() error {
	var buf bytes.Buffer
	if err := WriteYAML(&buf, c); err != nil {
		return fmt.Errorf("config document: render: %w", err)
	}
	if buf.Len() > ConfigDocumentMaxBytes {
		return fmt.Errorf("config document: must be at most %s (%d bytes), got %d bytes",
			FormatByteSize(ConfigDocumentMaxBytes), ConfigDocumentMaxBytes, buf.Len())
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

// ValidHeaderValue is the shared RFC 7230 field-value grammar check for
// configuration and per-request routing controls: horizontal tab, space,
// visible ASCII and obs-text (bytes 128..255) - never NUL/CR/LF or other
// control bytes. It deliberately accepts the empty string: whether an empty
// value is meaningful is the caller's policy (a mapped provider header must
// be non-empty; an X-Proxy-Auth-Prefix may legitimately be "").
func ValidHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == 9 || (c >= 32 && c != 127) {
			continue
		}
		return false
	}
	return true
}

// errRange is the shared wording for a validated field outside its allowed
// range; callers render min/max/got in the field's own unit (byte sizes,
// durations, integers, or the schema-typed bound). The byte-size, duration
// and storm validators, the models_discovery_max_pages /
// storage_query_max_rows integer pair, dash_log_rows, the
// cursor_heartbeat_interval / sse_keepalive_interval duration pair,
// quota_pause_recovery_successes, the request_overrides body ceilings,
// and the sub_conversations param byte bound all route through this one
// owner, so the phrasing cannot drift between them.
func errRange(key, min, max, got string) error {
	return fmt.Errorf("%s: must be %s..%s, got %s", key, min, max, got)
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
				return errRange(limit.key, FormatDuration(ModelsDiscoveryTimeoutMin), FormatDuration(ModelsDiscoveryTimeoutMax), FormatDuration(c.ModelsDiscoveryTimeout))
			}
			if limit.key == "models_discovery_max_bytes" {
				return checkByteSize(limit.key, ByteSize(limit.value), limit.min, limit.max)
			}
			return errRange(limit.key,
				strconv.FormatInt(limit.min, 10), strconv.FormatInt(limit.max, 10), strconv.FormatInt(limit.value, 10))
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
			return errRange(limit.key,
				strconv.FormatInt(int64(limit.min), 10), strconv.FormatInt(int64(limit.max), 10), strconv.FormatInt(int64(limit.value), 10))
		}
	}
	return nil
}

// validateMCPLimits checks the MCP endpoint's result-size bands at the config
// boundary. The constants are the single band owner: internal/mcp's
// Limits.Validate enforces the same ones at service construction, so the
// Settings write path and the endpoint cannot drift apart. The query row pair
// is a default plus its explicit argument ceiling, not a cap on every query.
// The two mcp timeouts join the positive-duration list in Validate instead.
func validateMCPLimits(c *Config) error {
	for _, limit := range [...]struct {
		key             string
		value, min, max int
	}{
		{"mcp_page_size", c.MCPPageSize, MCPPageSizeMin, MCPPageSizeMax},
		{"mcp_query_max_rows", c.MCPQueryMaxRows, MCPQueryMaxRowsMin, MCPQueryMaxRowsMax},
	} {
		if limit.value < limit.min || limit.value > limit.max {
			return errRange(limit.key,
				strconv.FormatInt(int64(limit.min), 10), strconv.FormatInt(int64(limit.max), 10), strconv.FormatInt(int64(limit.value), 10))
		}
	}
	if c.MCPQueryMaxBytes < MCPQueryMaxBytesMin {
		return fmt.Errorf("mcp_query_max_bytes: must be >= %s, got %s",
			FormatByteSize(MCPQueryMaxBytesMin), FormatByteSize(int64(c.MCPQueryMaxBytes)))
	}
	if c.MCPCaptureMaxBytes < MCPCaptureMaxBytesMin {
		return fmt.Errorf("mcp_capture_max_bytes: must be >= %s, got %s",
			FormatByteSize(MCPCaptureMaxBytesMin), FormatByteSize(int64(c.MCPCaptureMaxBytes)))
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
			return errRange(limit.key,
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
	// retryable_error_classes extends the built-in in-band retry vocabulary;
	// matching is case-insensitive, so duplicates are compared that way too.
	// A durable quota/billing class can never become retryable - waiting
	// cannot restore credits - and is rejected here rather than silently
	// denied at match time, so the operator gets the feedback at load.
	classSeen := make(map[string]bool, len(c.RetryableErrorClasses))
	for _, class := range c.RetryableErrorClasses {
		if strings.TrimSpace(class) == "" {
			return fmt.Errorf("retryable_error_classes: %q must not be empty or only whitespace", class)
		}
		lower := strings.ToLower(class)
		if classSeen[lower] {
			return fmt.Errorf("retryable_error_classes: duplicate %q", class)
		}
		classSeen[lower] = true
		if metrics.IsNonRetryableQuotaErr(class, class) {
			return fmt.Errorf("retryable_error_classes: %q is a durable quota/billing class and is never retryable", class)
		}
	}
	return nil
}

// validateQuotaPause owns the quota_pause_mode enum and its recovery
// bound. The enum is exact lowercase (deny by default); the recovery count
// is meaningful only in retry mode but is always range-checked so a stored
// value cannot sit invalid while unused.
func validateQuotaPause(c *Config) error {
	switch c.QuotaPauseMode {
	case QuotaPauseOff, QuotaPauseRetry, QuotaPauseManual:
	default:
		return fmt.Errorf("quota_pause_mode: %q must be one of %s", c.QuotaPauseMode, quotaPauseModes())
	}
	if c.QuotaPauseRecoverySuccesses < 1 || c.QuotaPauseRecoverySuccesses > 100 {
		return errRange("quota_pause_recovery_successes", "1", "100",
			strconv.Itoa(c.QuotaPauseRecoverySuccesses))
	}
	return nil
}
