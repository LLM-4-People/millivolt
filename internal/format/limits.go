package format

// RequestLimits carries the operator-selected inbound request bounds into the
// codec. internal/format must not import internal/config, so the proxy passes
// the configured values explicitly on every request. Every field must be
// positive: a zero value is a wiring bug and fails closed rather than turning
// an unbounded document loose on the translator.
type RequestLimits struct {
	// ToolCallsMax bounds the aggregate tool_calls members across every
	// message. WireToolCallsMax is the direct-library default; the proxy
	// always passes config.RequestToolCallsMax.
	ToolCallsMax int
}
