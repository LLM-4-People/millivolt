// Package format implements the wire codecs for the supported client and
// upstream protocols and translates between them through the canonical IR. It
// is used only when a client explicitly opts in (X-Proxy-Format header); the
// default proxy path is byte-transparent.
package format

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const (
	// sseLineBufInit / sseLineBufMax bound a single SSE event line while
	// scanning an upstream Anthropic stream (initial buffer / hard cap). The
	// small initial buffer fits the ordinary event and grows on demand, so a
	// stream that never exceeds it retains 4 KiB instead of 64. Safety
	// guardrail against pathological lines, not user-tunable.
	sseLineBufInit = 4 * 1024
	sseLineBufMax  = 1024 * 1024

	// nativeIdentityMaxBytes bounds provider-controlled identity strings
	// (message id/model and tool-call id/name) at the translation boundary
	// before they are echoed into every emitted frame. It matches the record
	// retention bound for provider text and model names (512 bytes), so a
	// normal identifier passes unchanged while a body-sized value cannot
	// multiply across a stream. Internal safety guardrail, not user-tunable.
	nativeIdentityMaxBytes = 512

	// wireJSONSourceByteMax is the largest encoded size of one accepted
	// source byte after translation: a raw invalid UTF-8 byte becomes the
	// three-byte U+FFFD replacement, U+2028/U+2029 expand from three bytes to
	// six, and a C0 control only appears in accepted JSON as an escape whose
	// six-byte output spelling is no larger than its six-byte source spelling.
	// sse.MarshalWireJSON disables HTML escaping, so <, > and & stay one byte.
	wireJSONSourceByteMax = 3

	// wireJSONDecodedByteMax is the largest wire encoding of one decoded
	// identity byte: a C0 control becomes a six-byte \u00XX escape.
	wireJSONDecodedByteMax = 6

	// nativeStreamFrameEnvelopeMax covers the fixed chunk-envelope text
	// around one event: the id/object/created/model skeleton, one
	// choice/delta/finish skeleton, and the largest fixed extra object (usage
	// or error keys). Provider-controlled text inside those objects is
	// counted separately.
	nativeStreamFrameEnvelopeMax = 2 * 1024

	// NativeStreamFrameMaxBytes bounds one SSE data line emitted by
	// StreamToOpenAI from a source line the translator accepts. The scanner
	// admits at most sseLineBufMax bytes per source line; re-encoding a
	// decoded string or retained raw tool input can at most triple each
	// source byte, and the retained message id/model are echoed into every
	// chunk, each bounded at nativeIdentityMaxBytes and escaped to at most
	// wireJSONDecodedByteMax bytes per byte. The envelope headroom covers the
	// fixed chunk skeleton around those values. The proxy's translated relay
	// sizes its line scanner from this bound, so every accepted source frame
	// relays instead of failing as a translation_error. Safety guardrail, not
	// user-tunable.
	NativeStreamFrameMaxBytes = wireJSONSourceByteMax*sseLineBufMax +
		2*wireJSONDecodedByteMax*nativeIdentityMaxBytes + nativeStreamFrameEnvelopeMax
)

var (
	// errNativeIdentityTooLong is the stable fail-closed reason for an
	// over-bound provider identity field.
	errNativeIdentityTooLong = errors.New("anthropic: identity field exceeds limit")
	// errNativeToolBlocks is the stable fail-closed reason for opening more
	// concurrent tool blocks than toolBlocksMax.
	errNativeToolBlocks = errors.New("anthropic: concurrent tool block limit exceeded")
	// errNativeToolInitialBytes is the stable fail-closed reason for retaining
	// more initial tool input than toolInitialBytesMax across open blocks.
	errNativeToolInitialBytes = errors.New("anthropic: retained tool input limit exceeded")
	// errNativeContentParts is the stable fail-closed reason for a response
	// content-block array over metrics.ContentPartMax.
	errNativeContentParts = errors.New("anthropic: content block limit exceeded")
)

// checkNativeIdentity enforces nativeIdentityMaxBytes on one provider-controlled
// identity value before the translator echoes or retains it.
func checkNativeIdentity(field, value string) error {
	if len(value) <= nativeIdentityMaxBytes {
		return nil
	}
	return fmt.Errorf("%w (%s is %d bytes, limit %d)",
		errNativeIdentityTooLong, field, len(value), nativeIdentityMaxBytes)
}

// MergeTokenCaps owns the Chat wire's token-ceiling precedence:
// max_completion_tokens wins over max_tokens, and neither present means no
// ceiling. The Chat decoder and the proxy's request decoders call it, so every
// reader of the pair resolves the same ceiling.
func MergeTokenCaps(maxCompletion, maxTokens *int) *int {
	switch {
	case maxCompletion != nil:
		return maxCompletion
	case maxTokens != nil:
		return maxTokens
	}
	return nil
}

// TranslateRequest converts an OpenAI Chat Completions request body into the
// Anthropic Messages request body. It is the Chat-to-Anthropic specialization
// of TranslateRequestBetween; the canonical IR owns the field mapping and
// records every unrepresentable field as a warning. defaultMaxTokens is sent
// when the client provides neither max_tokens nor max_completion_tokens
// (Anthropic requires max_tokens); it comes from config. promptCacheKey, when
// non-empty, is injected as a top-level prompt_cache_key - the proxy's
// sub-conversation tracking value, extracted from the client's own spelling
// before translation. It is not an Anthropic cache field (Anthropic uses
// cache_control); an upstream must tolerate unknown top-level fields. The
// whole body must be valid UTF-8: a raw invalid byte is rejected before the
// typed decode so the standard decoder's three-byte U+FFFD replacement cannot
// expand a body-sized string in memory.
func TranslateRequest(openaiBody []byte, defaultMaxTokens int, promptCacheKey string) ([]byte, error) {
	out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, openaiBody, defaultMaxTokens, promptCacheKey)
	return out, err
}

// TranslateResponse converts an Anthropic Messages (non-streaming) response
// into an OpenAI Chat Completions response. An Anthropic error body
// ({"type":"error",...}) is passed through as a JSON object with an error key.
// It is the Anthropic-to-Chat specialization of TranslateResponseBetween.
func TranslateResponse(anthropicBody []byte) ([]byte, error) {
	out, _, err := TranslateResponseBetween(ir.Chat, ir.Anthropic, anthropicBody)
	return out, err
}

// StreamToOpenAI reads an Anthropic SSE stream and writes an OpenAI-compatible
// SSE stream to dst. It is the Chat-from-Anthropic specialization of
// TranslateStreamBetween; observe receives every raw upstream data payload for
// source-only cost accounting and must not retain the borrowed bytes. The
// event name is dropped here because this wrapper's callers account the
// payload only; TranslateStreamBetween callers that need terminal
// classification receive it.
func StreamToOpenAI(dst io.Writer, src io.Reader, flusher interface{ Flush() }, observe func([]byte)) error {
	var f http.Flusher
	if flusher != nil {
		f = flusher
	}
	var typed func(string, []byte)
	if observe != nil {
		typed = func(_ string, payload []byte) { observe(payload) }
	}
	// This wrapper has no client request to consult, so it keeps its
	// historical contract of emitting the usage chunk whenever the source
	// reported usage.
	return TranslateStreamBetween(ir.Chat, ir.Anthropic, dst, src, f, typed, StreamOptions{IncludeUsage: true})
}
