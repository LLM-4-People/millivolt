package format

// Connect-RPC (https://connectrpc.com/docs/protocol/) stream framing.
//
// A Connect streaming body is a sequence of Enveloped-Messages, each with a
// 5-byte prefix: 1 flag byte + 4-byte big-endian length, then the payload.
//   - flag bit 0 (0x01): payload is compressed (we never set this on write and
//     reject it on read - see below).
//   - flag bit 1 (0x02): the final EndStreamResponse frame, whose payload is
//     JSON metadata ({} on success, {"error":{...}} on failure), not protobuf.
// Data frames carry the protobuf message; the terminal frame carries JSON.
//
// These are internal, non-tunable implementation details.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	connectFlagCompressed = 0x01
	connectFlagEndStream  = 0x02
	// connectFrameHeader is the fixed 5-byte envelope prefix size.
	connectFrameHeader = 5
	// connectMaxFrame bounds a single enveloped message. LLM deltas are tiny;
	// this is a safety guardrail against a corrupt/malicious length prefix,
	// not a user-tunable.
	connectMaxFrame = 32 << 20 // 32 MiB
)

// connectEnvelope is one decoded frame.
type connectEnvelope struct {
	endStream bool   // terminal EndStreamResponse frame (payload is JSON)
	payload   []byte // protobuf message (data frame) or JSON (end frame)
}

// AppendConnectEnvelope frames msg as a single uncompressed data envelope
// (exported for the proxy's bidi path, which pre-primes the request body).
func AppendConnectEnvelope(dst, msg []byte) []byte {
	var hdr [connectFrameHeader]byte
	// hdr[0] (flags) stays 0: uncompressed, not end-of-stream.
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	dst = append(dst, hdr[:]...)
	return append(dst, msg...)
}

// connectReader decodes a Connect stream frame-by-frame from a bufio.Reader.
type connectReader struct {
	r   *bufio.Reader
	hdr [connectFrameHeader]byte
}

func newConnectReader(r *bufio.Reader) *connectReader { return &connectReader{r: r} }

// next reads the next envelope. io.EOF is returned only when the stream ends
// cleanly on a frame boundary (no partial frame). A compressed data frame is
// rejected: the proxy strips Accept-Encoding and never advertises
// connect-accept-encoding, so a conformant server never gzips - a compressed
// frame here means a misbehaving upstream, and failing closed beats emitting
// garbage.
func (c *connectReader) next() (connectEnvelope, error) {
	var env connectEnvelope
	if _, err := io.ReadFull(c.r, c.hdr[:]); err != nil {
		// EOF with zero bytes read is a clean end; anything else is a
		// truncated frame header.
		return env, err
	}
	flags := c.hdr[0]
	length := binary.BigEndian.Uint32(c.hdr[1:])
	if length > connectMaxFrame {
		return env, fmt.Errorf("connect: frame length %d exceeds cap", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return env, fmt.Errorf("connect: truncated frame body: %w", err)
	}
	if flags&connectFlagCompressed != 0 {
		return env, fmt.Errorf("connect: unexpected compressed frame (flags %#x)", flags)
	}
	env.endStream = flags&connectFlagEndStream != 0
	env.payload = payload
	if env.endStream {
		if err := endStreamError(payload); err != nil {
			return env, err
		}
	}
	return env, nil
}

// EndStreamResponse is a single JSON object regardless of message encoding.
// Unknown extension fields are ignored; malformed JSON, wrong
// types, and explicit upstream errors must never become a successful EOF.
func endStreamError(payload []byte) error {
	var end *struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &end); err != nil {
		return fmt.Errorf("connect: invalid EndStreamResponse: %w", err)
	}
	if end == nil {
		return fmt.Errorf("connect: EndStreamResponse must be an object")
	}
	if len(end.Error) > 0 {
		var upstream *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(end.Error, &upstream); err != nil {
			return fmt.Errorf("connect: invalid EndStreamResponse error: %w", err)
		}
		if upstream == nil || upstream.Code == "" {
			return fmt.Errorf("connect: EndStreamResponse error requires a code")
		}
		return fmt.Errorf("connect upstream error (%s): %s", upstream.Code, upstream.Message)
	}
	return nil
}
