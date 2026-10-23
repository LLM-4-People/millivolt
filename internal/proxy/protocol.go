package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
// verbatim.
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
			return "", fmt.Errorf("unsupported %s %q", hdrInFormat, clientEcho(v))
		}
	}
	switch r.URL.Path {
	case "/v1/chat/completions":
		return ir.Chat, nil
	case "/v1/responses":
		return ir.Responses, nil
	case "/v1/messages":
		return ir.Anthropic, nil
	}
	return "", nil
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

// terminalFrameFor reports whether one complete encoder write is the inbound
// protocol's terminal frame.
func terminalFrameFor(inbound ir.Protocol) func([]byte) bool {
	switch inbound {
	case ir.Anthropic:
		return func(p []byte) bool {
			return bytes.Contains(p, []byte("event: message_stop"))
		}
	case ir.Responses:
		return func(p []byte) bool {
			return bytes.Contains(p, []byte("event: response.completed")) ||
				bytes.Contains(p, []byte("event: response.incomplete")) ||
				bytes.Contains(p, []byte("event: response.failed"))
		}
	default:
		return func(p []byte) bool { return bytes.Equal(p, []byte(sse.DoneFrame)) }
	}
}

// writeProtocolError writes one protocol-native error frame.
func writeProtocolError(w io.Writer, rec *metrics.Record, inbound ir.Protocol, id, typ, code, msg string) error {
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
		obj := sse.Chunk(id, time.Now().Unix(), rec.Model, []map[string]any{}, map[string]any{
			"error": sse.ErrorObject(typ, code, msg),
		})
		payload, err := sse.MarshalWireJSON(obj)
		if err != nil {
			return err
		}
		frame = sse.DataFrame(payload)
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	rec.ResponseBytes += int64(len(frame))
	return nil
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
	if _, err := w.Write(frame); err != nil {
		return err
	}
	rec.ResponseBytes += int64(len(frame))
	return nil
}

// translationLossFields lists the distinct fields a strict translation
// policy must reject. Every warning kind is a recorded loss, including an
// opaque provider artifact that cannot be replayed across families.
func translationLossFields(warnings ir.Warnings) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, w := range warnings {
		if _, ok := seen[w.Field]; ok {
			continue
		}
		seen[w.Field] = struct{}{}
		out = append(out, w.Field)
	}
	return out
}

func setWarningsHeader(h http.Header, warnings ir.Warnings) {
	if len(warnings) == 0 {
		return
	}
	add := headerSafe(warnings.Header())
	prev := h.Get("X-Proxy-Warnings")
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
	h.Set("X-Proxy-Warnings", add)
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
