package sse

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

// EmitFrame marshals obj, writes it to w as one SSE data frame, and flushes
// when flusher is non-nil: the plain-chunk emit pipeline (one marshal, one
// DataFrame render, one Write, one flush) the two bridges' streaming
// emitters share. The obj itself - chunk shape and created stamping - stays
// with each caller, and so does every other write path: the error-frame
// emitters keep their own write, flush and error handling (ErrorEnvelope).
func EmitFrame(w io.Writer, flusher http.Flusher, obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if _, err := w.Write(DataFrame(b)); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// ErrorEnvelope marshals the client-facing OpenAI error object carried
// in-band on an SSE stream: {"error":{...}} with the request id at the root
// when set. The error object itself is ErrorObject's shape. Callers render it
// with DataFrame and close with DoneFrame; each site keeps its own write,
// flush and error handling.
func ErrorEnvelope(id, typ string, code any, msg string) (json.RawMessage, error) {
	obj := map[string]any{"error": ErrorObject(typ, code, msg)}
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
