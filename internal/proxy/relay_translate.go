package proxy

import (
	"io"
	"net/http"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// qualitySpoolReadCap is the most one spool attempt can hold: the kept body
// cap plus the single overflowing read chunk. The overflowing chunk is
// retained so the verbatim streaming passthrough loses no byte, so the
// reservation that bounds the spool must cover it too.
const qualitySpoolReadCap = qualitySpoolMax + nonStreamChunk

// qualitySpoolReserve is the respBudget reservation for one spool attempt:
// the declared Content-Length when the body fits the spool, otherwise the
// full bounded read cap. It sits next to spoolBody so the reservation always
// describes the buffer it protects.
func qualitySpoolReserve(contentLength int64) int64 {
	return responseReadReserve(contentLength, qualitySpoolReadCap)
}

// spoolBody drains body up to qualitySpoolMax (plus one chunk) before
// returning. overflow=true when the cap was hit - the body is certainly not a
// degenerate shell and the caller commits to verbatim relay. err is the
// terminal read error (io.EOF for a complete body).
func spoolBody(body io.Reader) (b []byte, overflow bool, err error) {
	buf := make([]byte, nonStreamChunk)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if len(b)+n > qualitySpoolMax {
				b = append(b, buf[:n]...)
				return b, true, nil
			}
			b = append(b, buf[:n]...)
		}
		if rerr != nil {
			return b, false, rerr
		}
	}
}

// commitSpooledBody writes a fully spooled non-stream body verbatim: response
// headers, the status line, the bytes, then the shared non-stream analysis.
// A failed client write marks the disconnect but does not skip the analysis:
// the upstream delivered and charged the body, so the record keeps its
// payload. The analysis is the caller's one decode of the spooled body, reused
// here instead of decoding again. The gates that choose this epilogue (the
// in-band error envelope passthrough and the healthy commit) stay at their
// call sites.
// applyTranslatedStructure stamps the record fields the Chat-oriented
// non-stream analyzer cannot see when the client-facing wire is Anthropic or
// Responses, translated or native: finish reason, answer presence, preview,
// tool calls and the canonical usage. Model and id stay with the shared
// non-stream analysis when it can read the body.
func applyTranslatedStructure(rec *metrics.Record, resp *ir.Response, preview bool) {
	if rec == nil || resp == nil {
		return
	}
	// The record keeps the canonical source reason (for example Anthropic's
	// pause_turn or refusal); only the Chat wire mapper approximates it.
	rec.FinishReason = string(resp.Finish)
	if resp.Usage != (ir.Usage{}) && !rec.UsageInvalid {
		// The canonical usage is already normalized (Anthropic cache counts
		// folded into input, total derived); the translated wire may omit the
		// total field entirely. An explicitly invalid usage stays zeroed.
		rec.Usage = metrics.Usage{
			InputTokens:     resp.Usage.InputTokens,
			OutputTokens:    resp.Usage.OutputTokens,
			TotalTokens:     resp.Usage.TotalTokens,
			CacheReadTokens: resp.Usage.CacheReadTokens,
			CacheWrite:      resp.Usage.CacheWriteTokens,
			ReasoningTokens: resp.Usage.ReasoningTokens,
		}
	}
	if resp.Model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = resp.Model
	}
	if resp.ID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = resp.ID
	}
	names := make([]string, 0, metrics.RetainedToolNamesMax)
	seen := make(map[string]struct{}, metrics.RetainedToolNamesMax)
	for _, b := range resp.Blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Text != "" {
				rec.HadAnswerContent = true
				if preview && rec.ResponsePreview == "" {
					rec.ResponsePreview = metrics.TruncatePreview(b.Text)
				}
			}
		case ir.BlockRefusal:
			// A refusal is answer-shaped output, but not answer content, on
			// every path (streamed delta and non-stream native alike). Only
			// the preview sees it; marking HadAnswerContent would keep a
			// refusal+tool turn out of the tool-only settlement.
			if b.Refusal != "" {
				if preview && rec.ResponsePreview == "" {
					rec.ResponsePreview = metrics.TruncatePreview(b.Refusal)
				}
			}
		case ir.BlockToolCall:
			rec.ToolCalls++
			name := ""
			if b.ToolCall != nil {
				name = b.ToolCall.Name
			}
			if name == "" || len(names) >= metrics.RetainedToolNamesMax {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		rec.ToolNames = names
	}
	// Provider-executed Responses call items have no block (the translated
	// wire drops them with a warning), but they are model tool invocations:
	// count them so the record's finish and tool-only token settlement agree
	// with the streaming analyzer.
	rec.ToolCalls += resp.ProviderToolCalls
	// A canonical failure carried by the provider stop reason (or a native
	// failed envelope) owns the record classification. The translated body
	// may still be a Chat success shape, so the record must read the canonical
	// response, not only the client wire.
	stampNativeResponseError(rec, resp.Error)
}

// applyNativeNonChatStructure stamps the canonical structure of a native
// Responses or Anthropic 2xx body. The Chat-oriented non-stream analysis
// cannot see those wires, so finish reason, tool calls, answer presence,
// preview and folded cache usage would stay empty. Best-effort by design: a
// body the codec cannot decode keeps the existing analysis and relays
// unchanged, because metrics must never block the client. Unrepresentable
// source usage is the exception: it is parsed independently so accounting is
// invalidated even when the envelope cannot be decoded into the canonical IR.
func (s *Server) applyNativeNonChatStructure(rec *metrics.Record, t *target, spooled []byte) {
	if rec == nil || t == nil {
		return
	}
	wire := t.wire()
	switch wire {
	case ir.Responses, ir.Anthropic:
	default:
		return
	}
	// The provider's usage object is authoritative and honors custom
	// usage_keys; the canonical decoder folds Anthropic's cache counts and
	// derives the total only for its own documented shape. Parse it before
	// the structural decode so an overflow cannot escape invalidation when
	// DecodeResponse rejects the same envelope.
	usage, ok, usageErr := providerformat.SourceUsage(wire, spooled, s.usageKeysFor(rec.Provider))
	resp, err := providerformat.DecodeResponse(wire, spooled)
	if err != nil || resp == nil {
		// The canonical envelope is unrepresentable, but its parsed usage is
		// still authoritative. Apply the folded source counts here; the later
		// Chat-shaped fallback would otherwise read raw Anthropic counters as
		// Chat input/output and charge the wrong total.
		if usageErr != nil {
			invalidateUsage(rec, usageErr)
		} else if ok && !rec.UsageInvalid {
			applyFoldedUsage(rec, usage)
		}
		return
	}
	applyTranslatedStructure(rec, resp, s.cfg().CaptureBodyPreview)
	if usageErr != nil {
		// Invalidate after the provider error is stamped so its type/code
		// classification is preserved (invalidateUsage owns that rule).
		invalidateUsage(rec, usageErr)
		return
	}
	if ok && !rec.UsageInvalid {
		applyFoldedUsage(rec, usage)
	}
}

// applyFoldedUsage copies the canonical source usage onto the record. One
// owner so the structural-decode success and failure paths cannot diverge.
func applyFoldedUsage(rec *metrics.Record, usage ir.Usage) {
	rec.Usage = metrics.Usage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		TotalTokens:     usage.TotalTokens,
		CacheReadTokens: usage.CacheReadTokens,
		CacheWrite:      usage.CacheWriteTokens,
		ReasoningTokens: usage.ReasoningTokens,
	}
}

// stampNativeResponseError records a terminal failure carried inside a 2xx
// response: a native Responses failed envelope or a canonical error synthesized
// by a translated source codec. A provider error object keeps its own
// classification; a provider code without a type keeps the canonical
// provider_error fallback; a bare failure (error null or absent) is an upstream
// lifecycle failure. Existing error detail is never overwritten, and the body
// and HTTP status stay untouched.
func stampNativeResponseError(rec *metrics.Record, e *ir.Error) {
	if rec == nil || e == nil {
		return
	}
	if rec.ErrorType == "" {
		switch {
		case e.Type != "":
			rec.ErrorType = e.Type
		case e.Code != "":
			rec.ErrorType = "provider_error"
		default:
			rec.ErrorType = sse.TypeUpstreamError
		}
	}
	if rec.ErrorCode == "" {
		rec.ErrorCode = e.Code
	}
	if rec.ErrorMsg == "" {
		rec.ErrorMsg = e.Message
	}
}

func (s *Server) commitSpooledBody(w http.ResponseWriter, resp *http.Response, spooled []byte, rec *metrics.Record, analysis metrics.NonStreamAnalysis) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, werr := writeCounted(w, rec, spooled); werr != nil {
		markClientGone(rec)
	}
	applyNonStreamAnalysis(rec, analysis)
}
