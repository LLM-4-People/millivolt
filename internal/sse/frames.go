package sse

import (
	"bytes"
	"encoding/json"
)

// Client-facing OpenAI-compatible SSE frame primitives, the write-side
// mirror of the parsing in analyzer.go: the proxy paths that inject an
// in-band error, emit a usage chunk, or close a stream render through these
// so the wire bytes have one owner.

// DoneFrame is the terminal SSE frame of an OpenAI-compatible stream; every
// chat-completions SDK stops reading after it.
const DoneFrame = "data: [DONE]\n\n"

// DataFrame renders one SSE frame around a marshaled payload: "data: " +
// payload + the blank line that terminates the event.
func DataFrame(payload []byte) []byte {
	b := make([]byte, 0, len("data: ")+len(payload)+2)
	b = append(b, "data: "...)
	b = append(b, payload...)
	b = append(b, '\n', '\n')
	return b
}

// ErrorEnvelope marshals the client-facing OpenAI error object carried
// in-band on an SSE stream: {"error":{"message":...,"type":...,"param":null,
// "code":...}} with the request id at the root when set. code is nil or a
// provider error code string. Callers render it with DataFrame and close
// with DoneFrame; each site keeps its own write, flush and error handling.
func ErrorEnvelope(id, typ string, code any, msg string) (json.RawMessage, error) {
	obj := map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": code,
	}}
	if id != "" {
		obj["id"] = id
	}
	return json.Marshal(obj)
}

// DataPayload extracts the payload of one raw SSE line: ok is false when the
// line is not a data line - the full "data:" prefix is required (never just a
// leading 'd') so a short or garbled upstream line can never slice out of
// range - otherwise payload is the bytes after the prefix with one optional
// leading space consumed. Trailing \r tolerance stays with the terminal
// marker comparisons; ordinary payloads keep their bytes verbatim.
func DataPayload(line []byte) (payload []byte, ok bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload = line[len("data:"):]
	if len(payload) > 0 && payload[0] == ' ' {
		payload = payload[1:]
	}
	return payload, true
}
