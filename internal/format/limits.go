package format

// RequestLimits carries the operator-selected inbound request bounds into the
// codec. internal/format must not import internal/config, so the proxy passes
// the configured values explicitly on every request. The Chat decoder rejects a
// zero tool-call bound; protocols without a field use their own fixed caps.
type RequestLimits struct {
	// ToolCallsMax bounds the aggregate tool_calls members across every
	// message. WireToolCallsMax is the direct-library default; the proxy
	// always passes config.RequestToolCallsMax.
	ToolCallsMax int
}
