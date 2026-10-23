package format

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Protocol translation facade. The proxy selects an inbound protocol from the
// request path and an outbound protocol from X-Proxy-Format; when they differ,
// these functions decode one wire shape into the canonical IR and encode the
// other. Same-protocol requests never reach here: the proxy relays them
// byte-transparently.

// normalizeImageURL folds a base64 data URL into the canonical inline fields.
// Chat and Responses clients commonly send inline images that way, while
// Anthropic's URL image source expects a hosted URL; keeping the bytes and
// media type separate lets every encoder choose the shape its target accepts.
func normalizeImageURL(img *ir.Image) {
	if img == nil || img.URL == "" {
		return
	}
	mediaType, data, ok := splitBase64DataURL(img.URL)
	if !ok {
		return
	}
	img.URL, img.MediaType, img.Data = "", mediaType, data
}

// splitBase64DataURL parses data:[<media-type>][;<param>]*;base64,<payload>.
// A non-base64 or malformed data URL is left for the wire encoder unchanged.
func splitBase64DataURL(raw string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(raw, "data:")
	if !found {
		return "", "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found || payload == "" {
		return "", "", false
	}
	parts := strings.Split(meta, ";")
	if len(parts) < 2 || parts[0] == "" {
		return "", "", false
	}
	base64 := false
	for _, part := range parts[1:] {
		if part == "base64" {
			base64 = true
		}
	}
	if !base64 {
		return "", "", false
	}
	return parts[0], payload, true
}

// normalizeFileData folds a base64 data URL into the canonical inline file
// fields. Chat and Responses spell inline file bytes as a data URL, while
// Anthropic's base64 source keeps the bytes and media type separate.
func normalizeFileData(file *ir.File) {
	if file == nil || file.FileData == "" {
		return
	}
	mediaType, data, ok := splitBase64DataURL(file.FileData)
	if !ok {
		return
	}
	file.FileData = data
	if file.MediaType == "" {
		file.MediaType = mediaType
	}
}

// fileMediaType resolves an inline file's media type from its declared field
// or from a data-URL prefix. It is the shared owner of that question for both
// the Chat and Anthropic document allowlists.
func fileMediaType(file *ir.File) string {
	if file == nil {
		return ""
	}
	if file.MediaType != "" {
		return file.MediaType
	}
	if media, _, ok := splitBase64DataURL(file.FileData); ok {
		return media
	}
	return ""
}

// fileDataURL renders canonical inline file bytes in the data-URL spelling
// Chat and Responses accept. A value that already carries the prefix passes
// through unchanged, as does raw base64 with no declared media type.
func fileDataURL(file *ir.File) string {
	if file == nil || file.FileData == "" {
		return ""
	}
	if strings.HasPrefix(file.FileData, "data:") || file.MediaType == "" {
		return file.FileData
	}
	return "data:" + file.MediaType + ";base64," + file.FileData
}

// imageDataURL renders a canonical inline image as a data URL for wire
// families whose image part accepts a URL. URL wins when present; otherwise the
// validated inline payload is encoded with its declared media type.
func imageDataURL(img *ir.Image) string {
	if img == nil {
		return ""
	}
	if img.URL != "" {
		return img.URL
	}
	if img.Data == "" {
		return ""
	}
	return "data:" + img.MediaType + ";base64," + img.Data
}

// DecodeRequest decodes one inbound request body.
func DecodeRequest(p ir.Protocol, body []byte) (*ir.Request, error) {
	switch p {
	case ir.Chat:
		return DecodeChatRequest(body)
	case ir.Responses:
		return DecodeResponsesRequest(body)
	case ir.Anthropic:
		return DecodeAnthropicRequest(body)
	default:
		return nil, fmt.Errorf("unsupported inbound protocol %q", p)
	}
}

// EncodeRequest encodes the canonical request for one outbound protocol.
func EncodeRequest(p ir.Protocol, req *ir.Request, defaultMaxTokens int, promptCacheKey string) ([]byte, error) {
	switch p {
	case ir.Chat:
		return EncodeChatRequest(req)
	case ir.Responses:
		return EncodeResponsesRequest(req)
	case ir.Anthropic:
		return EncodeAnthropicRequest(req, defaultMaxTokens, promptCacheKey)
	default:
		return nil, fmt.Errorf("unsupported outbound protocol %q", p)
	}
}

// TranslateRequestBetweenIR converts one request body between two protocols
// and returns the canonical request so the caller can stamp request metadata
// without re-parsing the translated wire document.
func TranslateRequestBetweenIR(inbound, outbound ir.Protocol, body []byte, defaultMaxTokens int, promptCacheKey string) (*ir.Request, []byte, ir.Warnings, error) {
	req, err := DecodeRequest(inbound, body)
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := EncodeRequest(outbound, req, defaultMaxTokens, promptCacheKey)
	if err != nil {
		return req, nil, req.Warnings, err
	}
	return req, out, req.Warnings, nil
}

// TranslateRequestBetween converts one request body between two protocols.
func TranslateRequestBetween(inbound, outbound ir.Protocol, body []byte, defaultMaxTokens int, promptCacheKey string) ([]byte, ir.Warnings, error) {
	_, out, warnings, err := TranslateRequestBetweenIR(inbound, outbound, body, defaultMaxTokens, promptCacheKey)
	return out, warnings, err
}

// validateUsage rejects negative or overflowing token counts at the
// translation boundary. A provider that reports an impossible count must not
// reach the record or a client envelope.
func validateUsage(u ir.Usage) error {
	for _, v := range []int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens, u.TotalTokens} {
		if v < 0 {
			return fmt.Errorf("usage: %w", metrics.ErrMetricRange)
		}
	}
	if _, err := metrics.SumCounts(u.InputTokens, u.OutputTokens); err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	// InputTokens is the full prompt count, so the cached subsets cannot
	// exceed it. Anthropic's encoder subtracts them; keeping the invariant
	// here makes that subtraction non-negative on every path.
	cached, err := metrics.SumCounts(u.CacheReadTokens, u.CacheWriteTokens)
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	if cached > u.InputTokens {
		return fmt.Errorf("usage: %w", metrics.ErrMetricRange)
	}
	return nil
}

// DecodeResponse decodes one non-streaming upstream response body.
func DecodeResponse(p ir.Protocol, body []byte) (*ir.Response, error) {
	switch p {
	case ir.Chat:
		return DecodeChatResponse(body)
	case ir.Responses:
		return DecodeResponsesResponse(body)
	case ir.Anthropic:
		return DecodeAnthropicResponse(body)
	default:
		return nil, fmt.Errorf("unsupported outbound protocol %q", p)
	}
}

// EncodeResponse encodes the canonical response for one inbound protocol.
func EncodeResponse(p ir.Protocol, resp *ir.Response) ([]byte, error) {
	switch p {
	case ir.Chat:
		return EncodeChatResponse(resp)
	case ir.Responses:
		return EncodeResponsesResponse(resp)
	case ir.Anthropic:
		return EncodeAnthropicResponse(resp)
	default:
		return nil, fmt.Errorf("unsupported inbound protocol %q", p)
	}
}

// TranslateResponseBetween converts one non-streaming response body between
// two protocols.
func TranslateResponseBetween(inbound, outbound ir.Protocol, body []byte) ([]byte, ir.Warnings, error) {
	out, _, warnings, err := TranslateResponseBetweenIR(inbound, outbound, body, nil)
	return out, warnings, err
}

// TranslateResponseBetweenIR converts one non-streaming response body between
// two protocols and returns the canonical response so the caller can stamp
// record fields the client-facing wire no longer exposes as Chat-shaped
// choices. sourceUsage, when non-nil, is the source document's accounting
// (parsed with the provider's custom keys) and replaces the usage the target
// codec could decode from standard names alone. A provider error envelope
// returns a nil response: the encoded error body is the whole result.
func TranslateResponseBetweenIR(inbound, outbound ir.Protocol, body []byte, sourceUsage *ir.Usage) ([]byte, *ir.Response, ir.Warnings, error) {
	if outbound == ir.Anthropic {
		if e, err := DecodeAnthropicError(body); err == nil {
			out, err := EncodeError(inbound, e)
			return out, nil, nil, err
		}
	}
	resp, err := DecodeResponse(outbound, body)
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.Error != nil {
		out, err := EncodeError(inbound, resp.Error)
		return out, nil, resp.Warnings, err
	}
	if sourceUsage != nil {
		resp.Usage = *sourceUsage
	}
	out, err := EncodeResponse(inbound, resp)
	return out, resp, resp.Warnings, err
}

// TranslateStreamBetween converts one upstream SSE stream into the inbound
// protocol's SSE stream. observe, when non-nil, receives every raw upstream
// data payload (used for source-only cost accounting).
func TranslateStreamBetween(inbound, outbound ir.Protocol, dst io.Writer, src io.Reader, flusher http.Flusher, observe func([]byte)) error {
	var dec StreamDecoder
	switch outbound {
	case ir.Chat:
		d := NewChatStreamDecoder(src)
		d.observe = observe
		dec = d
	case ir.Responses:
		d := NewResponsesStreamDecoder(src)
		d.observe = observe
		dec = d
	case ir.Anthropic:
		dec = NewAnthropicStreamDecoder(src, observe)
	default:
		return fmt.Errorf("unsupported outbound protocol %q", outbound)
	}
	var enc StreamEncoder
	switch inbound {
	case ir.Chat:
		enc = NewChatStreamEncoder(dst, flusher)
	case ir.Responses:
		enc = NewResponsesStreamEncoder(dst, flusher)
	case ir.Anthropic:
		enc = NewAnthropicStreamEncoder(dst, flusher)
	default:
		return fmt.Errorf("unsupported inbound protocol %q", inbound)
	}
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Close every block the decoder still holds before the abnormal
			// terminal event: a target client must never receive an error or
			// terminal frame for a block that is still open.
			if errors.Is(err, ErrTruncatedStream) {
				for _, pending := range dec.FlushBlocks() {
					if writeErr := enc.Write(pending); writeErr != nil {
						return writeErr
					}
				}
				// Close the client stream with the protocol-native error event
				// so a truncated upstream never leaves the client waiting. A
				// truncation is a completed translation with an in-band error,
				// not a translator failure: the caller must not append a second
				// error.
				_ = enc.Write(&ir.Event{Type: ir.EventError, Error: &ir.Error{
					Type: sse.TypeUpstreamError, Code: metrics.CodeTruncated,
					Message: metrics.DegenerateMessage(metrics.CodeTruncated),
				}})
				_ = enc.Close()
				return nil
			}
			// A hard decoder failure is fail-closed: close the target blocks
			// with the abort flag so the encoder does not flush payload that
			// the same failure budget refused. The proxy's protocol-native
			// error frame terminates the stream.
			for _, pending := range dec.AbortBlocks() {
				if writeErr := enc.Write(pending); writeErr != nil {
					return writeErr
				}
			}
			return err
		}
		if err := enc.Write(ev); err != nil {
			return err
		}
	}
	return enc.Close()
}

// DecodeError decodes one upstream error body.
func DecodeError(p ir.Protocol, body []byte) (*ir.Error, error) {
	switch p {
	case ir.Chat, ir.Responses:
		return DecodeResponsesError(body)
	case ir.Anthropic:
		return DecodeAnthropicError(body)
	default:
		return nil, fmt.Errorf("unsupported outbound protocol %q", p)
	}
}

// EncodeError encodes the canonical error for one inbound protocol.
func EncodeError(p ir.Protocol, e *ir.Error) ([]byte, error) {
	switch p {
	case ir.Chat, ir.Responses:
		return EncodeOpenAIError(e)
	case ir.Anthropic:
		return EncodeAnthropicError(e)
	default:
		return nil, fmt.Errorf("unsupported inbound protocol %q", p)
	}
}

// TranslateErrorBetween converts one upstream error body between two
// protocols. An unparseable body becomes a bounded generic error carrying the
// HTTP status.
func TranslateErrorBetween(inbound, outbound ir.Protocol, status int, body []byte) ([]byte, error) {
	e, err := DecodeError(outbound, body)
	if err != nil {
		e = &ir.Error{
			Type:    "api_error",
			Message: fmt.Sprintf("upstream HTTP %d", status),
			Status:  status,
		}
	}
	if e.Status == 0 {
		e.Status = status
	}
	return EncodeError(inbound, e)
}

// EncodeOpenAIError renders the canonical error as the OpenAI error envelope
// shared by Chat Completions and Responses.
func EncodeOpenAIError(e *ir.Error) ([]byte, error) {
	typ := e.Type
	if typ == "" {
		typ = "api_error"
	}
	code := any(nil)
	if e.Code != "" {
		code = e.Code
	}
	return sse.MarshalWireJSON(map[string]any{
		"error": map[string]any{
			"message": e.Message,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	})
}
