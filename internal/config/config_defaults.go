package config

import (
	"strings"
	"time"
)

// RequestOverridesMax bounds the request_overrides list (internal
// guardrail, same shape as ModelRulesMax): the list is walked once per
// request after routing metadata is known, and an unbounded operator list
// would make that walk unbounded. The dashboard editor mirrors the number
// in its row count and add gate.
const RequestOverridesMax = 64

// storageWriteChanCapMax bounds the buffered-record channel (16 bytes per slot
// at deployment time): a typo must not reserve gigabytes at boot. Internal
// guardrail, not a tunable.
const storageWriteChanCapMax = 1_000_000

// SubConversationsMax bounds the sub_conversations list and
// SubConversationParamsMax bounds each entry's params list (internal
// guardrails, same shape as RequestOverridesMax): the list is consulted on
// the request path after client classification, and an unbounded operator
// list would make that walk unbounded. The dashboard editor mirrors both
// numbers in its row counts and add gates.
const (
	SubConversationsMax      = 16
	SubConversationParamsMax = 4
)

// subConversationParamMaxBytes bounds one tracked param name (internal
// safety constant): the name is one JSON object key segment, and the
// dashboard editor mirrors the bound in its live grammar validation.
const subConversationParamMaxBytes = 64

// DashLogRowsMin/Max are the allowed band for dash_log_rows: Validate,
// overlay, Settings schema, and GET /metrics/agg/log share this pair.
const (
	DashLogRowsMin = 10
	DashLogRowsMax = 500
	// QualityRetriesMax is the band for quality_retries (0 disables).
	QualityRetriesMax = 3
	// ThinkingRetriesMax is the band for thinking_retries (0 disables).
	ThinkingRetriesMax = 3
	// QueueRetryAfterMax is the Retry-After hint cap (HTTP delta-seconds).
	QueueRetryAfterMax = 24 * time.Hour
	// Inbound body band (Validate, overlay, Schema).
	MaxRequestBytesMin = 1024
	MaxRequestBytesMax = 1 << 30
	// Soft runtime memory-limit band (Validate, overlay, Schema). The floor
	// rejects limits below a fresh process's own documented footprint
	// (~20 MiB idle), which could only force continuous collection; the
	// ceiling is a plausible single-process budget, not a physical limit.
	MemoryLimitMin = 32 << 20
	MemoryLimitMax = 1 << 40
	// Debug capture retention / size bands (Validate, overlay, Schema).
	DebugCaptureTTLMin      = time.Hour
	DebugCaptureTTLMax      = 7 * 24 * time.Hour
	DebugCaptureMaxBytesMin = 4 << 10  // 4 KiB
	DebugCaptureMaxBytesMax = 16 << 20 // 16 MiB
	// Total debug sidecar band. Zero means unlimited; nonzero values below
	// the floor could not hold one maximum-size capture.
	DebugStoreMaxBytesMin = 16 << 20 // 16 MiB
	DebugStoreMaxBytesMax = 8 << 30  // 8 GiB
	// Process-wide in-flight body and response-buffer budget bands. Zero means
	// unlimited; a nonzero budget below one maximum request body or one small
	// buffered response cannot admit real work.
	MaxInflightBodyBytesMin     = 1 << 20  // 1 MiB
	MaxInflightBodyBytesMax     = 64 << 30 // 64 GiB
	MaxInflightResponseBytesMin = 1 << 20  // 1 MiB
	MaxInflightResponseBytesMax = 64 << 30 // 64 GiB
	// Ad-hoc query result bands (Validate, overlay, Settings schema).
	StorageQueryMaxBytesMin = 4 << 10
	StorageQueryMaxBytesMax = 64 << 20
	StorageQueryMaxRowsMin  = 1
	StorageQueryMaxRowsMax  = 100000
	// Settings backup archive band (Validate, overlay, Schema).
	BackupMaxBytesMin = 1 << 20 // 1 MiB
	BackupMaxBytesMax = 4 << 30 // 4 GiB
	// ConfigDocumentMaxBytes is the one bound on a configuration document:
	// LoadFile refuses a source file larger than this before parsing, and
	// Validate enforces it against the same WriteYAML output the backup encoder
	// retains. internal/backup reuses it as the config member cap, so a valid
	// config can never produce an archive member the decoder refuses. It is a
	// retention guardrail, not a tunable.
	ConfigDocumentMaxBytes = 16 << 20 // 16 MiB
	// MCP endpoint result-size bands (Validate, Schema, and internal/mcp's
	// Limits.Validate). The same single-owner constants are what the endpoint
	// enforces at service construction, so the config boundary and the endpoint
	// cannot drift apart; internal/mcp limits.go carries no second copy. The
	// query row value is the default used when max_rows is omitted; an explicit
	// max_rows may rise to MCPQueryMaxRowsMax.
	MCPPageSizeMin        = 10
	MCPPageSizeMax        = 500
	MCPQueryMaxRowsMin    = 1
	MCPQueryMaxRowsMax    = 100000
	MCPQueryMaxBytesMin   = 1
	MCPCaptureMaxBytesMin = 1
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

// Translation loss policies (TranslationLoss values; exact lowercase strings).
const (
	// TranslationLossWarn drops an optional field the target cannot represent
	// and reports it in X-Proxy-Warnings.
	TranslationLossWarn = "warn"
	// TranslationLossReject fails the request with 400 before any upstream
	// send when a field cannot be represented.
	TranslationLossReject = "reject"
)

func translationLossPolicies() string { return TranslationLossWarn + ", " + TranslationLossReject }

// Quota pause modes (QuotaPauseMode values; exact lowercase strings).
const (
	QuotaPauseOff    = "off"
	QuotaPauseRetry  = "retry"
	QuotaPauseManual = "manual"
)

func quotaPauseModes() string {
	return strings.Join([]string{QuotaPauseOff, QuotaPauseRetry, QuotaPauseManual}, ", ")
}

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
		MemoryLimit:         0, // 0 = auto cgroup ceiling, else unbounded; GOMEMLIMIT wins

		MaxRequestBytes:         32 << 20, // 32 MiB
		UpstreamTimeout:         5 * time.Minute,
		ModelsDiscoveryTimeout:  30 * time.Second,
		ModelsDiscoveryMaxBytes: 8 << 20,
		ModelsDiscoveryMaxPages: 100,

		MaxConnsPerHost:     512,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,

		UpstreamDialTimeout:         30 * time.Second,
		UpstreamDialKeepAlive:       30 * time.Second,
		UpstreamTLSHandshakeTimeout: 10 * time.Second,
		UpstreamStreamIdleTimeout:   10 * time.Minute,
		WholeDocumentReadTimeout:    2 * time.Minute,
		WholeDocumentReadMax:        10 * time.Minute,

		MaxRetries:               5,
		MaxQueueSize:             1024,
		MaxQueueWait:             10 * time.Minute,
		MaxInflightRequests:      10000,
		MaxInflightBodyBytes:     256 << 20,
		MaxInflightResponseBytes: 256 << 20,
		QueueRetryAfter:          2 * time.Second,
		BaseBackoff:              1 * time.Second, // start at 1s, double each attempt/failed request, cap at MaxBackoff
		MaxBackoff:               2 * time.Minute,

		QualityRetries:  1,
		ThinkingRetries: 1,

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

		QuotaPauseMode:              QuotaPauseOff,
		QuotaPauseRecoverySuccesses: 1,

		AutoTokenRefresh: true,

		// The shipped canonicalization pipeline is data, not semantics: fold
		// case, strip vendor/ and :tag, unify digit dots. Every step is
		// editable/removable in Settings; an explicitly empty list groups by
		// the exact stored spelling (presence-based slice merge).
		ModelRules: DefaultModelRules(),

		DebugCaptureTTL:      24 * time.Hour,
		DebugCaptureMaxBytes: 1 << 20,   // 1 MiB
		DebugStoreMaxBytes:   512 << 20, // 512 MiB total sidecar

		ConversationIdleGap: 30 * time.Minute,
		ConversationMaxOpen: 64,

		AnthropicDefaultMaxTokens:  4096,
		TranslationLoss:            TranslationLossWarn,
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

		// The MCP endpoint's shipped policy: on by default (the route stays
		// reserved for the 404 handler when off), with the result-size
		// policy a model can read. mcp.LimitsFromConfig maps these onto
		// the endpoint's Limits.
		MCPEnabled:         true,
		MCPPageSize:        50,
		MCPQueryMaxRows:    200,
		MCPQueryMaxBytes:   128 << 10,
		MCPCaptureMaxBytes: 256 << 10,
		MCPQueryTimeout:    2 * time.Minute,
		MCPTimeout:         30 * time.Second,

		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,

		RequestBodyIdleTimeout: 30 * time.Second,
		RequestBodyReadTimeout: 10 * time.Minute,
		ResponseWriteTimeout:   10 * time.Minute,

		SSEKeepaliveInterval:    15 * time.Second,
		SSEWriteProgressTimeout: time.Minute,

		DashLogRows:       60,
		DashPollInterval:  5 * time.Second,
		DashChartRefresh:  15 * time.Second,
		DashExplorerStale: 15 * time.Second,

		DashBackgroundRefresh: false,
	}
}

// Example returns the shipped configuration, reusing every server default,
// enabling three optional public provider profiles, and enabling the opencode
// sub-conversation tracking exemplar (strip on: the operator's upstream
// rejects the field with a 400). These noncredential headers are
// compatibility snapshots, not verified or stable provider API contracts.
// Operators can edit or remove them in their private config or Settings.
// Built-in defaults remain provider-neutral; loading no config never adds them.
func Example() *Config {
	c := Default()
	const grokVersion = "1.0.13" // Keep the compatibility identity internally consistent.
	// The opencode CLI's composite User-Agent: its own version, the
	// @ai-sdk/openai-compatible provider-utils build, and the bun runtime.
	// Keep the three-part identity internally consistent.
	const (
		opencodeCLI      = "1.18.32"
		opencodeProvider = "4.0.23"
		opencodeRuntime  = "1.3.14"
	)
	c.SubConversations = []SubConversation{
		// opencode identifies its sub-conversations through promptCacheKey in
		// the request body; the tracked value rides the k: conversation
		// identity, and the field is stripped because the operator's upstream
		// rejects it with a 400.
		{Client: "opencode", Params: []string{"promptCacheKey"}, Strip: true},
	}
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
		"opencode.ai": {
			// OpenCode Zen uses the ordinary OpenAI-compatible relay and its
			// standard models list. The CLI's free tier is gated on its wire
			// signature, not on TLS or header order: the request must carry
			// the CLI's per-conversation msg_/ses_ id headers in its 26-char
			// identifier format (shape-validated upstream; the two template
			// placeholders mint fresh ones per request) and a tools array
			// containing the bash and read tool names (ensure_tools injects
			// inert stubs when a client's request lacks them; an absent,
			// null, or auto choice becomes none, while a required or named
			// choice skips injection). A check on
			// 2026-09-24 found the free models then served under the
			// anonymous public bearer with this signature; that is an
			// observation about that day, not an entitlement. The profile is
			// not an account, key or permission, and there is no zen login
			// helper.
			UsageKeys:   map[string]string{},
			ModelsKeys:  map[string]string{},
			EnsureTools: []string{"bash", "read"},
			Headers: map[string]string{
				"User-Agent":         "opencode/" + opencodeCLI + " ai-sdk/provider-utils/" + opencodeProvider + " runtime/bun/" + opencodeRuntime,
				"x-opencode-client":  "cli",
				"x-opencode-project": "global",
				"x-opencode-request": "{{opencode-msg-id}}",
				"x-opencode-session": "{{opencode-ses-id}}",
			},
		},
	}
	return c
}
