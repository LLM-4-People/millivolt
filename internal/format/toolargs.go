package format

import (
	"encoding/json"
	"errors"
	"strings"
)

// errToolArgumentsJSON, errToolArgumentsObject and errToolArgumentsMissing
// mark an argument value the source never completed. A non-clean non-stream
// terminal drops only the call carrying one of these; a clean terminal and the
// request path stay strict.
var (
	errToolArgumentsJSON    = errors.New("tool call arguments must be valid JSON")
	errToolArgumentsObject  = errors.New("tool call arguments must be a JSON object")
	errToolArgumentsMissing = errors.New("tool call arguments are missing")
)

// isToolArgumentsError reports whether err is an argument-value failure rather
// than a structural tool-call failure such as a missing name or id.
func isToolArgumentsError(err error) bool {
	return errors.Is(err, errToolArgumentsJSON) ||
		errors.Is(err, errToolArgumentsObject) ||
		errors.Is(err, errToolArgumentsMissing)
}

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
		return nil, errToolArgumentsJSON
	}
	if trimmed[0] != '{' {
		return nil, errToolArgumentsObject
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
		return errToolArgumentsJSON
	}
	if trimmed[0] != '{' {
		return errToolArgumentsObject
	}
	return nil
}
