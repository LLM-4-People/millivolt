package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Inbound protocol detection and outbound protocol selection. The inbound
// protocol is a property of the client's request path (with an explicit
// override header for clients that cannot change paths); the outbound protocol
// is the existing X-Proxy-Format selector. Translation runs only when both are
// known and differ, so a same-protocol request stays byte-transparent.

// inboundProtocol resolves the client's wire protocol. An empty result means
// the path is not a known inference endpoint and the request is relayed
// verbatim. On an invalid override the path-derived protocol is returned with
// the error, so an early rejection still answers in the client's own envelope.
func inboundProtocol(r *http.Request) (ir.Protocol, error) {
	if v := strings.TrimSpace(r.Header.Get(hdrInFormat)); v != "" {
		switch v {
		case "openai", "openai-chat", "chat":
			return ir.Chat, nil
		case "openai-responses", "responses":
			return ir.Responses, nil
		case "anthropic":
			return ir.Anthropic, nil
		default:
			return pathProtocol(r.URL.Path), fmt.Errorf("unsupported %s %q", hdrInFormat, clientEcho(v))
		}
	}
	return pathProtocol(r.URL.Path), nil
}

// pathProtocol maps one client endpoint to its wire protocol.
func pathProtocol(path string) ir.Protocol {
	switch path {
	case "/v1/chat/completions":
		return ir.Chat
	case "/v1/responses":
		return ir.Responses
	case "/v1/messages":
		return ir.Anthropic
	}
	return ""
}

// validOutboundFormat is the single accepted X-Proxy-Format vocabulary.
func validOutboundFormat(format string) bool {
	switch format {
	case "openai", "openai-responses", "anthropic", "cursor":
		return true
	}
	return false
}

// outboundProtocol maps the X-Proxy-Format spelling to a protocol. The empty
// spelling means "relay to the path the client chose" and never translates.
func outboundProtocol(format string) ir.Protocol {
	switch format {
	case "openai":
		return ir.Chat
	case "openai-responses":
		return ir.Responses
	case "anthropic":
		return ir.Anthropic
	case "cursor":
		return ir.Cursor
	}
	return ""
}

// protocolPath is the canonical upstream endpoint for one protocol.
func protocolPath(p ir.Protocol) string {
	switch p {
	case ir.Chat:
		return "/v1/chat/completions"
	case ir.Responses:
		return "/v1/responses"
	case ir.Anthropic:
		return "/v1/messages"
	}
	return ""
}

// warningsHeader bounds the translation-warning header so a pathological
// request cannot turn a response into a header-sized document.
const warningsHeaderMax = 1024

// sseFrameName returns the event name of one complete encoder write. The
// translators emit each event as a single frame whose first line is
// "event: NAME"; a provider-controlled data string is never inspected.
func sseFrameName(p []byte) string {
	const prefix = "event: "
	if !bytes.HasPrefix(p, []byte(prefix)) {
		return ""
	}
	rest := p[len(prefix):]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		return string(rest[:i])
	}
	return ""
}

// frameDataPayload returns the payload of a frame's first data line. The
// translators emit one event per write, so a frame's error object lives behind
// its optional "event:" line; DataPayload alone only matches a raw data line.
func frameDataPayload(p []byte) []byte {
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			line, p = p[:i], p[i+1:]
		} else {
			p = nil
		}
		if payload, ok := sse.DataPayload(bytes.TrimRight(line, "\r")); ok {
			return payload
		}
	}
	return nil
}

// frameIsError reports whether a translated frame already tells the client
// about a provider failure. Chat carries a top-level error object in a bare
// data frame; Anthropic and Responses name the event "error" (the Responses
// payload has no error key, so the event name is the discriminator). An event
// whose payload merely contains a decoy "error":null key (every Responses
// object does) is not an error: only a parseable top-level envelope counts.
func frameIsError(p []byte) bool {
	switch sseFrameName(p) {
	case "error", "response.failed":
		return true
	}
	payload := frameDataPayload(p)
	if payload == nil {
		return false
	}
	typ, _, _ := metrics.ParseErrorEnvelope(payload)
	return typ != ""
}

// terminalFrameFor reports whether one complete encoder write is the inbound
// protocol's terminal frame.
func terminalFrameFor(inbound ir.Protocol) func([]byte) bool {
	switch inbound {
	case ir.Anthropic:
		return func(p []byte) bool { return sseFrameName(p) == "message_stop" }
	case ir.Responses:
		return func(p []byte) bool {
			switch sseFrameName(p) {
			case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
				return true
			}
			return false
		}
	default:
		return func(p []byte) bool { return bytes.Equal(p, []byte(sse.DoneFrame)) }
	}
}

// writeCounted writes one complete frame and books it on the record. A short
// write is an error: the record must describe the bytes the client actually
// received, not the frame the proxy intended to send.
func writeCounted(w io.Writer, rec *metrics.Record, frame []byte) error {
	n, err := w.Write(frame)
	if n > 0 && rec != nil {
		rec.ResponseBytes += int64(n)
	}
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// writeProtocolError writes one protocol-native error frame.
func writeProtocolError(w io.Writer, rec *metrics.Record, inbound ir.Protocol, id, typ, code, msg string) error {
	if typ == "" {
		typ = sse.TypeUpstreamError
	}
	var frame []byte
	switch inbound {
	case ir.Anthropic:
		payload, err := providerformat.EncodeAnthropicError(&ir.Error{Type: typ, Message: msg})
		if err != nil {
			return err
		}
		frame = append([]byte("event: error\ndata: "), payload...)
		frame = append(frame, '\n', '\n')
	case ir.Responses:
		payload, err := sse.MarshalWireJSON(map[string]any{
			"type": "error", "code": code, "message": msg, "param": nil,
		})
		if err != nil {
			return err
		}
		frame = append([]byte("event: error\ndata: "), payload...)
		frame = append(frame, '\n', '\n')
	default:
		var c any
		if code != "" {
			c = code
		}
		payload, err := sse.ErrorEnvelope(id, typ, c, msg)
		if err != nil {
			return err
		}
		frame = sse.DataFrame(payload)
	}
	return writeCounted(w, rec, frame)
}

// writeProtocolTerminal writes the inbound protocol's terminal frame.
func writeProtocolTerminal(w io.Writer, rec *metrics.Record, inbound ir.Protocol, id, model string, typ, code, msg string) error {
	var frame []byte
	switch inbound {
	case ir.Anthropic:
		frame = []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case ir.Responses:
		obj := map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed",
				"error":  map[string]any{"code": code, "message": msg},
				"output": []any{},
			},
		}
		payload, err := sse.MarshalWireJSON(obj)
		if err != nil {
			return err
		}
		frame = append([]byte("event: response.failed\ndata: "), payload...)
		frame = append(frame, '\n', '\n')
	default:
		frame = []byte(sse.DoneFrame)
	}
	return writeCounted(w, rec, frame)
}

// writeProtocolStreamError writes the inbound protocol's in-band error frame
// followed by its terminal frame. Every committed-socket failure goes through
// this one sink so an Anthropic or Responses client never receives an OpenAI
// error object or a stray [DONE].
func writeProtocolStreamError(w io.Writer, rec *metrics.Record, inbound ir.Protocol, id, model, typ, code, msg string) error {
	if err := writeProtocolError(w, rec, inbound, id, typ, code, msg); err != nil {
		return err
	}
	return writeProtocolTerminal(w, rec, inbound, id, model, typ, code, msg)
}

// translationLossFields lists the distinct fields a strict translation
// policy must reject. Every warning kind is a recorded loss, including an
// opaque provider artifact that cannot be replayed across families. Each field
// is bounded because a decoder can interpolate a body-controlled name.
func translationLossFields(warnings ir.Warnings) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, w := range warnings {
		field := retainedText(w.Field, translationLossFieldMax)
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
	}
	return out
}

const (
	// translationLossFieldMax bounds one echoed translation-loss field, and
	// protocolErrorEchoMax bounds the whole message a pre-commit error writer
	// may echo from a decoder or translator.
	translationLossFieldMax = 128
	protocolErrorEchoMax    = 1024
)

// writeProtocolHTTPError writes a pre-commit JSON error in the client's
// protocol. Chat and unknown paths keep the historical OpenAI envelope; a
// known Anthropic or Responses client gets its own shape. The message is
// bounded at this one sink because decoders interpolate body-controlled names.
func writeProtocolHTTPError(w http.ResponseWriter, inbound ir.Protocol, status int, typ, msg string) {
	writeProtocolHTTPErrorHdr(w, inbound, status, typ, msg, nil)
}

func writeProtocolHTTPErrorHdr(w http.ResponseWriter, inbound ir.Protocol, status int, typ, msg string, extra http.Header) {
	msg = retainedText(msg, protocolErrorEchoMax)
	body, err := providerformat.EncodeError(inbound, &ir.Error{Type: typ, Message: msg, Status: status})
	if err != nil {
		body = []byte(errJSON(typ, msg))
	}
	if extra != nil {
		copyResponseHeaders(w.Header(), extra)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func setWarningsHeader(h http.Header, warnings ir.Warnings) {
	if len(warnings) == 0 {
		return
	}
	add := headerSafe(warnings.Header())
	prev := h.Get(hdrWarnings)
	if prev != "" {
		add = prev + "; " + add
	}
	if len(add) > warningsHeaderMax {
		// Keep the value valid UTF-8 and leave room for the marker: a field
		// boundary is more useful than a cut mid-rune.
		cut := warningsHeaderMax - len("; truncated")
		if cut < 0 {
			cut = 0
		}
		add = strings.ToValidUTF8(add[:cut], "") + "; truncated"
	}
	h.Set(hdrWarnings, add)
}

// headerSafe keeps a warning value printable ASCII. Warning fields can carry
// provider-controlled type names, so a raw control byte must not reach the
// response header writer.
func headerSafe(v string) string {
	b := []byte(v)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}
