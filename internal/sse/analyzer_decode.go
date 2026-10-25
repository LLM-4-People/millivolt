package sse

import (
	"bytes"
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// eventDataMax bounds the per-event accumulation buffer. A legit SSE event is
// a few KB; the proxy's own line cap (sseScannerBufMax) is 1 MiB, so this is
// comfortably above any real event while preventing an unbounded buffer from
// a hostile upstream that never sends a blank line.
const eventDataMax = 1 << 20

// completeJSONObject reports whether payload is one whole JSON object: an
// opening brace to a closing brace with nothing but space outside. Only whole
// objects skip event reassembly; anything ambiguous keeps accumulating.
func completeJSONObject(payload []byte) bool {
	i := metrics.SkipSpace(payload, 0)
	if i >= len(payload) || payload[i] != '{' {
		return false
	}
	end := len(payload)
	for end > i+1 && (payload[end-1] == ' ' || payload[end-1] == '\t' || payload[end-1] == '\r' || payload[end-1] == '\n') {
		end--
	}
	return payload[end-1] == '}'
}

// isBlankLine reports whether the line is empty or only whitespace/CR - the
// SSE event terminator.
func isBlankLine(line []byte) bool {
	for _, b := range line {
		if b != '\r' && b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

// TerminalLine is the shared terminal-marker gate for analysis and relay
// hold release. A quoted marker in content is never an event type.
func TerminalLine(line []byte) bool {
	payload, ok := DataPayload(line)
	if !ok {
		return false
	}
	if isDonePayload(payload) || responsesTerminalEvent(responseEventType(payload)) {
		return true
	}
	// Anthropic Messages closes with message_stop. A content string that
	// happens to contain the same text is not a top-level type, so decode the
	// discriminator rather than scanning for the bytes.
	if !bytes.Contains(payload, []byte(`"type"`)) {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &ev) == nil && ev.Type == "message_stop"
}

// Responses event types are top-level, unlike the nested type fields in
// content/tool objects. The frame key probe records that top-level value once;
// ordinary chat frames return on the raw-prefix check without decoding.
func responseEventType(payload []byte) string {
	var keys frameKeys
	keys.scan(payload)
	return keys.responseEventType(payload)
}

// jsonValueString extracts the string text from a raw JSON value (the probe's
// value slice, already positioned after the colon and any whitespace). Returns
// "" when the value is not a string. Unescaped values are copied once; escaped
// values are decoded by encoding/json so every legal escape survives.
func jsonValueString(value []byte) string {
	if len(value) < 2 || value[0] != '"' {
		return ""
	}
	escaped := false
	for i := 1; i < len(value); i++ {
		switch value[i] {
		case '\\':
			escaped = true
			i++
		case '"':
			if !escaped {
				return string(value[1:i])
			}
			var text string
			if json.Unmarshal(value[:i+1], &text) == nil {
				return text
			}
			return ""
		}
	}
	return ""
}

// extractObject returns the {...} portion of raw starting at the opening
// brace, reading until braces balance. It is JSON-string-aware: braces inside
// a string literal do not affect depth, and a quote is escaped iff preceded
// by an odd number of consecutive backslashes. Returns nil if raw does not
// start with '{' (e.g. "usage":null) so a non-object value is never treated
// as a usage blob.
func extractObject(raw []byte) json.RawMessage {
	return extractContainer(raw, '{', '}')
}

func extractContainer(raw []byte, open, close byte) json.RawMessage {
	if len(raw) == 0 || raw[0] != open {
		return nil
	}
	depth := 0
	inString := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			// A quote toggles string state iff not escaped (even number of
			// preceding backslashes).
			if precedingBackslashes(raw, i)%2 == 0 {
				inString = !inString
			}
		case open:
			if !inString {
				depth++
			}
		case close:
			if !inString {
				depth--
				if depth == 0 {
					return raw[:i+1]
				}
			}
		}
	}
	return raw
}

// precedingBackslashes counts consecutive '\\' bytes immediately before i.
func precedingBackslashes(b []byte, i int) int {
	n := 0
	for i-n-1 >= 0 && b[i-n-1] == '\\' {
		n++
	}
	return n
}

// extractPreview extracts a bounded prefix (metrics.PreviewMaxBytes) of the
// first content delta's text from the probe's first content value, decoding
// just that value with a lightweight parse. Called only when CapturePreview is
// enabled.
func extractPreview(value []byte) []byte {
	text := jsonValueString(value)
	if text == "" {
		return nil
	}
	return []byte(metrics.TruncatePreview(text))
}
