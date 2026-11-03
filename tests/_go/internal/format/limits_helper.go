package format

// testRequestLimits applies the codec's direct-library request bounds. Tests
// stay on this package's defaults because internal/config imports
// internal/format (the admin Settings handler), so a format test cannot import
// config without a cycle. The config package pins the same default in its own
// contract test.

func testRequestLimits() RequestLimits {
	return RequestLimits{ToolCallsMax: WireToolCallsMax}
}
