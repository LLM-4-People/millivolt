package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

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
	return slices.Sorted(maps.Keys(m))
}

// canonicalProviderLabel is the one provider-label policy shared by the
// providers map and provider_aliases: the runtime derives a label from the
// upstream host (lowercase, no surrounding whitespace), so a configured label
// only ever matches in that canonical form. Anything else is rejected by
// validateProviderLabel rather than kept as a silent no-op.
func canonicalProviderLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// validateProviderLabel checks a configured provider label against the one
// canonical form. A label containing whitespace cannot be the host-derived
// value the request path records, so it is denied just like a mixed-case
// spelling. normalizeProviderAliases calls it after canonicalization;
// Validate calls it directly because provider labels are rejected, not
// rewritten.
func validateProviderLabel(label string) error {
	if label == "" {
		return errors.New("must be non-empty")
	}
	if strings.ContainsAny(label, " \t\r\n") {
		return fmt.Errorf("%q must not contain whitespace", label)
	}
	if label != strings.ToLower(label) {
		return fmt.Errorf("%q must be lowercase", label)
	}
	return nil
}

// normalizeProviderAliases is the one normalizer for provider_aliases. Both
// sides are canonicalized before the self/chain checks, and a collision created
// by that canonicalization is rejected instead of letting map iteration choose
// a winner. It is idempotent, so Apply and the shared Validate boundary may
// both call it without changing a valid map.
func normalizeProviderAliases(aliases map[string]string) (map[string]string, error) {
	if aliases == nil {
		return nil, nil
	}
	out := make(map[string]string, len(aliases))
	seen := make(map[string]string, len(aliases))
	for _, rawFrom := range sortedKeys(aliases) {
		from := canonicalProviderLabel(rawFrom)
		to := canonicalProviderLabel(aliases[rawFrom])
		if err := validateProviderLabel(from); err != nil {
			return nil, fmt.Errorf("alias label %q: %w", from, err)
		}
		if err := validateProviderLabel(to); err != nil {
			return nil, fmt.Errorf("%s: target label: %w", from, err)
		}
		if first, duplicate := seen[from]; duplicate {
			return nil, fmt.Errorf("%q and %q: labels collide after normalization to %q", first, rawFrom, from)
		}
		seen[from] = rawFrom
		out[from] = to
	}
	return out, nil
}

// forbiddenOverrideHeaders is the name set of the request-overrides header
// trust boundary, keyed lowercase (HTTP header names are case-insensitive,
// so the check is too). Credential headers are denied because the proxy
// injects the upstream credential itself; protocol and framing headers are
// denied because the proxy and the HTTP transport own the wire. The value
// is the denial reason used by the error wording. The x-proxy- control
// prefix is denied as a whole and lives in the prefix check inside
// ForbiddenOverrideHeader, not in this set. The proxy's runtime strip maps
// serve a different consumer and stay untouched.
var forbiddenOverrideHeaders = map[string]string{
	"authorization":       "credential",
	"cookie":              "credential",
	"proxy-authorization": "credential",
	"host":                "protocol",
	"content-type":        "protocol",
	"content-length":      "protocol",
	"transfer-encoding":   "protocol",
	"te":                  "protocol",
	"connection":          "protocol",
	"keep-alive":          "protocol",
	"proxy-authenticate":  "protocol",
	"proxy-connection":    "protocol",
	"trailer":             "protocol",
	"upgrade":             "protocol",
}

// ForbiddenOverrideHeader owns the request-overrides header grammar: it
// reports whether a set or removed header name is credential-owned,
// protocol-owned, or carries the proxy's x-proxy- control prefix, along
// with the reason the rejection message explains. Both lists of a rule
// share the grammar: deleting a credential or framing header is as
// breaking as rewriting it.
func ForbiddenOverrideHeader(name string) (reason string, forbidden bool) {
	lower := strings.ToLower(name)
	if reason = forbiddenOverrideHeaders[lower]; reason != "" {
		return reason, true
	}
	if strings.HasPrefix(lower, "x-proxy-") {
		return "control", true
	}
	return "", false
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

// overrideBodyEmpty reports whether a body section carries no field to
// rewrite (nil, or both token fields unset). validateRequestOverrides
// normalizes such a body to nil and yamlwrite renders it as "body: {}".
func overrideBodyEmpty(b *OverrideBody) bool {
	return b == nil || (b.MaxTokens == nil && b.MaxCompletionTokens == nil)
}

// cloneRequestOverrides deep-copies the rule list with all nested
// collections and the body pointers, preserving nil (a nil slice or nil
// body stays nil so "absent" and "empty" never merge into one value).
// Clone and the Map export share this one copy owner, so a snapshot or an
// exported payload can never alias a live config.
func cloneRequestOverrides(rs []RequestOverride) []RequestOverride {
	if rs == nil {
		return nil
	}
	out := make([]RequestOverride, len(rs))
	for i, r := range rs {
		ro := RequestOverride{
			Client:        r.Client,
			Provider:      r.Provider,
			Model:         r.Model,
			Headers:       cloneStrMap(r.Headers),
			RemoveHeaders: append([]string(nil), r.RemoveHeaders...),
		}
		if r.Body != nil {
			body := &OverrideBody{}
			if r.Body.MaxTokens != nil {
				v := *r.Body.MaxTokens
				body.MaxTokens = &v
			}
			if r.Body.MaxCompletionTokens != nil {
				v := *r.Body.MaxCompletionTokens
				body.MaxCompletionTokens = &v
			}
			ro.Body = body
		}
		out[i] = ro
	}
	return out
}

// cloneSubConversations deep-copies the entry list with the nested params
// slice, preserving nil (a nil slice stays nil so "absent" and "empty"
// never merge into one value). Clone and the Map export share this one
// copy owner, so a snapshot or an exported payload can never alias a live
// config.
func cloneSubConversations(ss []SubConversation) []SubConversation {
	if ss == nil {
		return nil
	}
	out := make([]SubConversation, len(ss))
	for i, s := range ss {
		out[i] = SubConversation{
			Client: s.Client,
			Params: append([]string(nil), s.Params...),
			Strip:  s.Strip,
		}
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
	if c.RetryableErrorClasses != nil {
		out.RetryableErrorClasses = append([]string{}, c.RetryableErrorClasses...)
	}
	if c.Providers != nil {
		out.Providers = make(map[string]ProviderOverride, len(c.Providers))
		for k, v := range c.Providers {
			out.Providers[k] = ProviderOverride{
				CostKeys:    append([]string(nil), v.CostKeys...),
				UsageKeys:   cloneStrMap(v.UsageKeys),
				ModelsPath:  v.ModelsPath,
				ModelsKeys:  cloneStrMap(v.ModelsKeys),
				EnsureTools: append([]string(nil), v.EnsureTools...),
				Headers:     cloneStrMap(v.Headers),
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
	out.RequestOverrides = cloneRequestOverrides(c.RequestOverrides)
	out.SubConversations = cloneSubConversations(c.SubConversations)
	return &out
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
// original aside and writes Default(), except for an ambiguous duplicate key,
// which fails closed without rewriting the source. Other load errors fail
// closed too.
type configParseError struct {
	Path string
	Err  error
}

func (e *configParseError) Error() string {
	return fmt.Sprintf("parse config %s: %s", e.Path, e.Err)
}

func (e *configParseError) Unwrap() error { return e.Err }

// duplicateYAMLKeyError marks an ambiguous mapping: the same top-level key is
// defined twice. It is deliberately distinguishable from generic YAML syntax
// errors so LoadFileRepair refuses to rewrite the document instead of replacing
// it with defaults.
type duplicateYAMLKeyError struct {
	Key       string
	Line      int
	FirstLine int
}

func (e *duplicateYAMLKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q at line %d (first defined at line %d)", e.Key, e.Line, e.FirstLine)
}

func parseConfig(path string, err error) error {
	return &configParseError{Path: path, Err: err}
}

// configValueError is a recognized key whose value is invalid. It is not
// repairable debris: dropping it would silently change policy, so boot and
// reload must fail closed instead of replacing it with a default.
type configValueError struct {
	key string
	err error
}

func (e *configValueError) Error() string {
	if e.key == "" {
		return fmt.Sprintf("invalid config value: %v", e.err)
	}
	return fmt.Sprintf("config key %q: %v", e.key, e.err)
}
func (e *configValueError) Unwrap() error { return e.err }

// failClosedYAMLKey lists keys whose absence changes a security or bind
// boundary. An invalid value must never be silently dropped.
func failClosedYAMLKey(key string) bool {
	switch key {
	case "allowed_base_urls", "listen":
		return true
	default:
		return false
	}
}

// LoadFile is the lenient library/test read. It reads the YAML config at path
// (may be empty) and merges recognized, valid keys over the built-in defaults;
// a missing file yields the defaults. Unknown keys, extra YAML documents and an
// invalid type or range for an ordinary key are omitted. `allowed_base_urls`
// and `listen` are the exceptions: their invalid values describe a security or
// bind boundary, so they fail the read instead of being dropped. Production
// does not call it: boot uses LoadFileRepair, reload uses LoadFileStrict, and
// Settings, backup and the healthcheck use their strict reads. A duplicate
// top-level key is ambiguous and returns a parse error naming the key and line.
// This path does not rewrite the file.
func LoadFile(path string) (*Config, error) {
	cfg, _, err := loadYAMLFile(path)
	return cfg, err
}

// LoadFileStrict is the reload admission path. Unlike LoadFile it does not
// salvage a recognized key's invalid value: any unparseable document, unknown
// nested field, extra document, invalid value or cross-field validation
// failure fails the reload so the running policy is preserved. Unknown
// top-level keys remain debris and are reported, never applied. Boot repair
// remains LoadFileRepair's job.
func LoadFileStrict(path string) (*Config, []string, error) {
	if path == "" {
		return Default(), nil, nil
	}
	b, err := ReadDocument(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return loadYAMLBytesFailClosed(path, b, false)
}

// LoadBytes is the backup-restore admission path for an in-memory YAML
// document. It fails closed on invalid recognized values like boot, while
// unknown debris is reported and omitted.
func LoadBytes(b []byte) (*Config, []string, error) {
	return loadYAMLBytesFailClosed("backup", b, true)
}

// LoadFileRepair is the boot-only repair path: it loads under the fail-closed
// policy, then persists a cleaned document when unknown keys, unknown nested
// fields, or extra YAML documents were dropped. A recognized key with an
// invalid value is not debris and returns an error without rewriting the
// source. Unparseable files are moved aside to path+invalidConfigSuffix and
// replaced with Default(). A duplicate top-level key is ambiguous instead of
// debris: it is returned as a parse error and the source is left untouched.
// Healthcheck, print-only and reload paths must not call this.
func LoadFileRepair(path string) (*Config, []string, error) {
	if path == "" {
		cfg := Default()
		return cfg, nil, cfg.Validate()
	}
	b, err := ReadDocument(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil, nil
		}
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	cfg, skipped, err := loadYAMLBytesFailClosed(path, b, true)
	if err != nil {
		var valueErr *configValueError
		if errors.As(err, &valueErr) {
			return nil, nil, err
		}
		var parseErr *configParseError
		if !errors.As(err, &parseErr) {
			return nil, nil, err
		}
		// A duplicate key is ambiguous, not debris: leave the document
		// untouched so the first value is never replaced by a default.
		var duplicate *duplicateYAMLKeyError
		if errors.As(err, &duplicate) {
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
	if len(skipped) > 0 {
		if err := WriteFile(path, cfg); err != nil {
			return cfg, skipped, fmt.Errorf("repair config %s: %w", path, err)
		}
	}
	return cfg, skipped, nil
}

// ReadDocument is the one bounded read owner for a configuration document.
// Boot, reload, inspect and the restore rollback capture all use it. It
// enforces the ConfigDocumentMaxBytes bound on the input bytes before parsing,
// matching the rendered-document bound that Config.validateDocumentSize
// enforces through Validate after parsing.
//
// The pre-open stat rejects a FIFO, device or directory before os.Open, which
// would block indefinitely on a FIFO with no writer. The post-open stat repeats
// the regular-file and size checks for the stat-to-open race, and the bounded
// LimitReader keeps the size guarantee even if the file grows after the check.
func ReadDocument(path string) ([]byte, error) {
	pre, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !pre.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if pre.Size() > int64(ConfigDocumentMaxBytes) {
		return nil, configFileTooLarge(path, pre.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if info.Size() > int64(ConfigDocumentMaxBytes) {
		return nil, configFileTooLarge(path, info.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(ConfigDocumentMaxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > ConfigDocumentMaxBytes {
		return nil, configFileTooLarge(path, int64(len(b)))
	}
	return b, nil
}

func configFileTooLarge(path string, size int64) error {
	return fmt.Errorf("%s: file is %d bytes, above the %s (%d byte) config document limit",
		path, size, FormatByteSize(ConfigDocumentMaxBytes), ConfigDocumentMaxBytes)
}

func loadYAMLFile(path string) (*Config, []string, error) {
	def := Default()
	if path == "" {
		return def, nil, def.Validate()
	}
	b, err := ReadDocument(path)
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
	keys := slices.Sorted(maps.Keys(present))
	var accepted []string
	for _, key := range keys {
		if FieldByKey(key) == nil {
			skipped = append(skipped, key)
			continue
		}
		loose, err := overlayYAMLKey(def.Clone(), key, nodes[key], present[key])
		if err != nil {
			if failClosedYAMLKey(key) {
				return nil, nil, &configValueError{key: key, err: err}
			}
			skipped = append(skipped, key)
			continue
		}
		if loose {
			skipped = append(skipped, key+" (unknown fields)")
		}
		accepted = append(accepted, key)
	}
	cfg, dropped, err := keepYAMLKeys(def, present, nodes, accepted)
	if err != nil {
		return nil, nil, err
	}
	skipped = append(skipped, dropped...)
	return cfg, skipped, nil
}

// loadYAMLBytesFailClosed parses one document for the applying paths (boot
// repair, reload, backup restore). A recognized key's invalid value is always
// an error. repairDebris additionally allows boot to drop and rewrite unknown
// top-level keys, unknown nested fields and extra documents; reload refuses
// the latter two so only an explicit boot repair can clean a document.
func loadYAMLBytesFailClosed(path string, b []byte, repairDebris bool) (*Config, []string, error) {
	def := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, parseConfig(path, err)
	}
	var skipped []string
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if !repairDebris {
			return nil, nil, parseConfig(path, errors.New("multiple YAML documents"))
		}
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
	keys := slices.Sorted(maps.Keys(present))
	accepted := make([]string, 0, len(keys))
	for _, key := range keys {
		if FieldByKey(key) == nil {
			// Unknown top-level keys are obsolete debris, never policy: both
			// paths report them without applying them.
			skipped = append(skipped, key)
			continue
		}
		loose, err := overlayYAMLKey(def.Clone(), key, nodes[key], present[key])
		if err != nil {
			return nil, nil, &configValueError{key: key, err: err}
		}
		if loose {
			if !repairDebris {
				return nil, nil, parseConfig(path, fmt.Errorf("config key %q has unknown fields", key))
			}
			skipped = append(skipped, key+" (unknown fields)")
		}
		accepted = append(accepted, key)
	}
	cfg, err := mergeYAMLKeys(def, present, nodes, accepted)
	if err != nil {
		return nil, nil, &configValueError{err: err}
	}
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
	seen := make(map[string]int, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := yamlResolved(n.Content[i])
		valNode := yamlResolved(n.Content[i+1])
		if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
			return nil, nil, fmt.Errorf("document must be a YAML mapping")
		}
		key := keyNode.Value
		if firstLine, duplicate := seen[key]; duplicate {
			return nil, nil, &duplicateYAMLKeyError{Key: key, Line: keyNode.Line, FirstLine: firstLine}
		}
		seen[key] = keyNode.Line
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

// keepYAMLKeys merges the accepted yaml keys onto def and returns the kept
// config plus the keys the loader must drop. Two-phase contract: a file whose
// whole accepted key set validates loads in a single merge, the common case
// for every written file; the greedy per-key salvage loop only runs when
// whole-set validation fails, so conflicting files keep the existing repair
// semantics unchanged.
func keepYAMLKeys(def *Config, present map[string]any, nodes map[string]*yaml.Node, accepted []string) (*Config, []string, error) {
	if merged, err := mergeYAMLKeys(def, present, nodes, accepted); err == nil {
		return merged, nil, nil
	} else {
		validationErr := err
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
		for _, key := range skipped {
			if failClosedYAMLKey(key) {
				return nil, nil, &configValueError{key: key, err: validationErr}
			}
		}
		return cfg, skipped, nil
	}
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
		return false, errUnknownKey(key)
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

// validateRequestOverrides is the load-boundary gate for the whole
// request-overrides list (called by Validate, i.e. by YAML load AND the
// Settings POST). It normalizes safe input before judging it - scope
// fields, header names and header values are trimmed, header-name case is
// canonicalized, empty collections are dropped - and then enforces the
// rules: a scope and an action per rule, the header token grammar and the
// forbidden-owner set, one canonical name never both set and removed, the
// body band, and the length cap. A rule list that passes is canonical:
// every stored name is in HTTP canonical case, and every collection that
// survives is non-empty.
func validateRequestOverrides(c *Config) error {
	if len(c.RequestOverrides) > RequestOverridesMax {
		return fmt.Errorf("request_overrides: %d rules exceeds the cap of %d", len(c.RequestOverrides), RequestOverridesMax)
	}
	scopes := make(map[[3]string]int, len(c.RequestOverrides))
	for i := range c.RequestOverrides {
		r := &c.RequestOverrides[i]
		r.Client = strings.TrimSpace(r.Client)
		r.Provider = strings.TrimSpace(r.Provider)
		r.Model = strings.TrimSpace(r.Model)
		if r.Client == "" && r.Provider == "" && r.Model == "" {
			return fmt.Errorf("request_overrides[%d]: no scope set - give the rule a client, provider or model, or remove the rule", i)
		}
		if len(r.Headers) == 0 && len(r.RemoveHeaders) == 0 && overrideBodyEmpty(r.Body) {
			return fmt.Errorf("request_overrides[%d]: no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule", i)
		}
		if err := normalizeOverrideHeaders(i, r); err != nil {
			return err
		}
		if r.Body != nil {
			for _, f := range [...]struct {
				name  string
				value *int
			}{
				{"max_tokens", r.Body.MaxTokens},
				{"max_completion_tokens", r.Body.MaxCompletionTokens},
			} {
				if f.value != nil && (*f.value < 1 || *f.value > 1_000_000) {
					return errRange(fmt.Sprintf("request_overrides[%d].body.%s", i, f.name), "1", "1000000", strconv.Itoa(*f.value))
				}
			}
			if overrideBodyEmpty(r.Body) {
				r.Body = nil
			}
		}
		key := [3]string{r.Client, r.Provider, r.Model}
		if first, dup := scopes[key]; dup {
			return fmt.Errorf("request_overrides[%d]: duplicate scope with request_overrides[%d] (client %q, provider %q, model %q); merge the rules or change one scope", i, first, r.Client, r.Provider, r.Model)
		}
		scopes[key] = i
	}
	return nil
}

// normalizeOverrideHeaders validates and canonicalizes one rule's two
// header lists. Names are trimmed, checked against the RFC 7230 token
// grammar and the forbidden-owner set, and rewritten in HTTP canonical
// case; values are trimmed and must be non-empty single-line header
// values. HTTP names are case-insensitive, so "user-agent" and
// "User-Agent" are one header: only a true duplicate (the same canonical
// name twice in one list, or in both lists of one rule) is rejected, and
// an empty collection is normalized away. rule is the rule's index for
// the error messages.
func normalizeOverrideHeaders(rule int, r *RequestOverride) error {
	headers := make(map[string]string, len(r.Headers))
	for _, name := range sortedKeys(r.Headers) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("request_overrides[%d].headers: header name must not be empty or only whitespace", rule)
		}
		if !ValidHeaderName(trimmed) {
			return fmt.Errorf("request_overrides[%d].headers: %q is not a valid header name (RFC 7230 token)", rule, trimmed)
		}
		if reason, forbidden := ForbiddenOverrideHeader(trimmed); forbidden {
			return forbiddenOverrideHeaderError(rule, "headers", trimmed, reason)
		}
		canon := http.CanonicalHeaderKey(trimmed)
		if _, dup := headers[canon]; dup {
			return fmt.Errorf("request_overrides[%d].headers: duplicate HTTP header name %q", rule, trimmed)
		}
		value := strings.TrimSpace(r.Headers[name])
		if value == "" || !ValidHeaderValue(value) {
			return fmt.Errorf("request_overrides[%d].headers: %s: value must be a non-empty single-line header value", rule, canon)
		}
		headers[canon] = value
	}
	remove := make([]string, 0, len(r.RemoveHeaders))
	seen := make(map[string]bool, len(r.Headers)+len(r.RemoveHeaders))
	for _, name := range r.RemoveHeaders {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("request_overrides[%d].remove_headers: header name must not be empty or only whitespace", rule)
		}
		if !ValidHeaderName(trimmed) {
			return fmt.Errorf("request_overrides[%d].remove_headers: %q is not a valid header name (RFC 7230 token)", rule, trimmed)
		}
		if reason, forbidden := ForbiddenOverrideHeader(trimmed); forbidden {
			return forbiddenOverrideHeaderError(rule, "remove_headers", trimmed, reason)
		}
		canon := http.CanonicalHeaderKey(trimmed)
		if seen[canon] {
			return fmt.Errorf("request_overrides[%d].remove_headers: duplicate HTTP header name %q", rule, trimmed)
		}
		seen[canon] = true
		remove = append(remove, canon)
	}
	for canon := range headers {
		if seen[canon] {
			return fmt.Errorf("request_overrides[%d]: %q is both set in headers and removed in remove_headers; keep exactly one action per header", rule, canon)
		}
	}
	if len(headers) == 0 {
		r.Headers = nil
	} else {
		r.Headers = headers
	}
	if len(remove) == 0 {
		r.RemoveHeaders = nil
	} else {
		r.RemoveHeaders = remove
	}
	return nil
}

// forbiddenOverrideHeaderError is the operator wording for a denied header
// name: each reason explains who owns the header, so the fix is obvious
// from the message alone.
func forbiddenOverrideHeaderError(rule int, field, name, reason string) error {
	switch reason {
	case "credential":
		return fmt.Errorf("request_overrides[%d].%s: %q is credential-owned and cannot be overridden; the proxy injects the provider key itself", rule, field, name)
	case "protocol":
		return fmt.Errorf("request_overrides[%d].%s: %q is protocol-owned and cannot be overridden; the proxy and the HTTP transport manage it", rule, field, name)
	default: // "control": the x-proxy- prefix
		return fmt.Errorf("request_overrides[%d].%s: %q carries the x-proxy- control prefix and cannot be overridden; x-proxy- headers are the proxy's own request channel", rule, field, name)
	}
}

// validateSubConversations is the load-boundary gate for the whole
// sub-conversations list (called by Validate, i.e. by YAML load AND the
// Settings POST). It normalizes safe input before judging it - the client
// and every param name are trimmed - and then enforces the rules: one
// entry per classified client (duplicates rejected, exact spellings only,
// no case folding), 1..4 params per entry, the one JSON-key-segment param
// grammar, and the entry cap. strip is a plain bool, never normalized. A
// list that passes is canonical: every stored name is trimmed.
func validateSubConversations(c *Config) error {
	if len(c.SubConversations) > SubConversationsMax {
		return fmt.Errorf("sub_conversations: %d entries exceeds the cap of %d", len(c.SubConversations), SubConversationsMax)
	}
	clients := make(map[string]int, len(c.SubConversations))
	for i := range c.SubConversations {
		s := &c.SubConversations[i]
		s.Client = strings.TrimSpace(s.Client)
		if s.Client == "" {
			return fmt.Errorf("sub_conversations[%d]: client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry", i)
		}
		if len(s.Params) == 0 {
			return fmt.Errorf("sub_conversations[%d]: no param set - give the entry a request body field to track, or remove the entry", i)
		}
		if len(s.Params) > SubConversationParamsMax {
			return fmt.Errorf("sub_conversations[%d].params: %d params exceeds the cap of %d", i, len(s.Params), SubConversationParamsMax)
		}
		seen := make(map[string]bool, len(s.Params))
		for j := range s.Params {
			s.Params[j] = strings.TrimSpace(s.Params[j])
			if err := checkSubConversationParam(i, j, s.Params[j]); err != nil {
				return err
			}
			if seen[s.Params[j]] {
				return fmt.Errorf("sub_conversations[%d].params: duplicate %q", i, s.Params[j])
			}
			seen[s.Params[j]] = true
		}
		if first, dup := clients[s.Client]; dup {
			return fmt.Errorf("sub_conversations[%d]: duplicate client %q with sub_conversations[%d]; merge the entries or change one client", i, s.Client, first)
		}
		clients[s.Client] = i
	}
	return nil
}

// checkSubConversationParam is the single owner of the tracked-param name
// grammar: one JSON object key segment - non-empty, at most
// subConversationParamMaxBytes bytes, printable ASCII only, with no quote
// and no backslash. Deliberately NOT provPathRE (that owns dotted response
// paths) and deliberately without a regex or case folding: the names are
// exact wire field names, and legal-but-unusual spellings (a literal dot,
// an interior space) stay legal because JSON allows them. entry and param
// locate the value in the error.
func checkSubConversationParam(entry, param int, name string) error {
	key := fmt.Sprintf("sub_conversations[%d].params[%d]", entry, param)
	if name == "" {
		return fmt.Errorf("%s: must not be empty or only whitespace", key)
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7e || b == '"' || b == '\\' {
			return fmt.Errorf("%s: %q is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)", key, name)
		}
	}
	if len(name) > subConversationParamMaxBytes {
		return errRange(key, "1", strconv.Itoa(subConversationParamMaxBytes)+" bytes", strconv.Itoa(len(name)))
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
	case KindRequestOverrides:
		switch items := v.(type) {
		case []RequestOverride:
		case []any:
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok {
					return fmt.Errorf("%s: every item must be a map", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list", field.Key)
		}
	case KindSubConversations:
		switch items := v.(type) {
		case []SubConversation:
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
