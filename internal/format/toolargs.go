package format

import (
	"encoding/json"
	"errors"
	"strings"
)

// parseToolArguments normalizes a tool-call argument string into a JSON
// object. Empty and null both become the empty object; any other value must
// be valid JSON whose first non-space byte is an object opener. The non-stream
// decoders and the streaming terminal-only path share this validator; bytes
// already streamed use validateStreamedToolArguments, which refuses to
// normalize wire values.
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

// validateStreamedToolArguments enforces the IR object invariant on argument
// bytes that are already on the client wire. Unlike parseToolArguments it must
// not normalize: streaming cannot rewrite bytes the client already received, so
// a completed streamed value that is null, an array, or another non-object is a
// translation failure. Empty is allowed because no bytes were emitted.
func validateStreamedToolArguments(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if !json.Valid([]byte(trimmed)) {
		return errors.New("tool call arguments must be valid JSON")
	}
	if trimmed[0] != '{' {
		return errors.New("tool call arguments must be a JSON object")
	}
	return nil
}
