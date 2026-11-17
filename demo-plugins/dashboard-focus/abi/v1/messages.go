// Package v1 is the versioned wire contract for millivolt plugin ABI v1.
//
// The types here are the source of truth for message shapes. Host and guest
// adapters must encode exactly these JSON field names; schema.json is a
// readable projection checked for drift by the conformance suite.
package v1

// ABI is the string a manifest must declare to use this contract.
const ABI = "v1"

// Wire bounds. A host must refuse a message larger than MaxMessageBytes before
// decoding it, and must clamp every repeated field to its own cap.
const (
	MaxMessageBytes = 256 << 10
	MaxLogs         = 64
	MaxMetrics      = 64
	MaxTools        = 16
	MaxPanels       = 16
	MaxLogBytes     = 4 << 10
	MaxNameBytes    = 128
)

// CapabilityHTTPFetch names the generic, operator-allowlisted outbound fetch
// capability. Product-specific URLs and parsing belong to the guest.
const CapabilityHTTPFetch = "http:fetch"

// EventType names the lifecycle events a guest may receive.
type EventType string

const (
	EventRequestRecorded EventType = "request.recorded"
	EventConfigChanged   EventType = "config.changed"
	EventOperatorCommand EventType = "operator.command"
)

// Event is the input envelope passed to mv_handle. Exactly one payload field
// is set, matching Type.
type Event struct {
	Type    EventType        `json:"type"`
	Seq     uint64           `json:"seq"`
	Record  *Record          `json:"record,omitempty"`
	Config  *ConfigView      `json:"config,omitempty"`
	Command *OperatorCommand `json:"command,omitempty"`
}

// Record is the redacted request summary. It never contains request or
// response bodies, upstream keys, or the operator token.
type Record struct {
	ID                 string  `json:"id"`
	Start              string  `json:"start"`
	Status             int     `json:"status"`
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	Client             string  `json:"client"`
	DurationMs         int64   `json:"duration_ms"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	Cost               float64 `json:"cost,omitempty"`
	RateLimitRemaining int     `json:"rate_limit_remaining,omitempty"`
	ErrorType          string  `json:"error_type,omitempty"`
}

// ConfigView is a read-only projection of non-secret configuration.
type ConfigView struct {
	SchemaVersion int               `json:"schema_version"`
	Values        map[string]string `json:"values"`
}

// HTTPFetchRequest is a bounded outbound fetch request. The host, not the
// guest, enforces destination allowlists, timeouts, and response size caps.
type HTTPFetchRequest struct {
	URL              string            `json:"url"`
	Method           string            `json:"method,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	TimeoutMs        int               `json:"timeout_ms,omitempty"`
	CredentialID     string            `json:"credential_id,omitempty"`
	CredentialHeader string            `json:"credential_header,omitempty"`
	MaxResponseBytes int               `json:"max_response_bytes,omitempty"`
}

// HTTPFetchResponse is the bounded result of one outbound fetch. Status is
// zero when the request failed before an HTTP response was received.
type HTTPFetchResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
}

// OperatorCommand is an operator-triggered instruction. Arguments are bounded
// strings; no command may carry credentials.
type OperatorCommand struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// Response is the guest's output envelope. Each list is independently capped
// by the host; a guest that exceeds a cap is rejected, not silently truncated.
type Response struct {
	Logs    []LogEntry       `json:"logs,omitempty"`
	Metrics []Metric         `json:"metrics,omitempty"`
	Tools   []ToolDescriptor `json:"tools,omitempty"`
	Panels  []UIPanel        `json:"panels,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// LogEntry is a host log line. Level is one of debug, info, warn, error.
type LogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Metric is one bounded numeric observation.
type Metric struct {
	Name   string            `json:"name"`
	Value  float64           `json:"value"`
	Labels map[string]string `json:"labels,omitempty"`
}

// ToolDescriptor declares an MCP tool the guest wants exposed. It is a
// declaration only; the core host decides whether the plugin's capability
// permits the declaration.
type ToolDescriptor struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
}

// UIPanel declares a core-rendered panel. Guests never supply HTML or JS.
type UIPanel struct {
	ID    string            `json:"id"`
	Title string            `json:"title"`
	Data  map[string]string `json:"data,omitempty"`
}
