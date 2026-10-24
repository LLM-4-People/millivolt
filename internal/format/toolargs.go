package format

import (
	"encoding/json"
	"errors"
	"strings"
)

// parseToolArguments normalizes a tool-call argument string into a JSON
// object. Empty and null both become the empty object; any other value must
// be valid JSON whose first non-space byte is an object opener. The non-stream
// decoders and the streaming terminal/reconciliation owners share this one
// validator so every target enforces the same IR invariant.
func parseToolArguments(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, errors.New("tool call arguments must be valid JSON")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("tool call arguments must be a JSON object")
	}
	return json.RawMessage(trimmed), nil
}
