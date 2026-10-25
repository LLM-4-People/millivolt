package sse

import (
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// captureStreamError decodes one in-band error payload into errType/errCode/
// errMsg via the canonical envelope decoder (metrics.ParseErrorEnvelope). The
// first real error wins (later frames of a failing stream are usually shutdown
// noise). Non-errors - a null/empty error value or a decoy "error" key nested
// in a normal chunk - leave no mark.
func (a *Analyzer) captureStreamError(payload []byte) {
	if a.errType != "" {
		return
	}
	a.setStreamError(metrics.ParseErrorEnvelope(payload))
}

// captureStandaloneError classifies one reassembled standalone error event:
// a document whose code and message are top-level siblings of "type":"error"
// (the Responses error shape) instead of a nested error object. A per-line
// scan cannot see a pretty-printed event, and the document has no top-level
// "error" key for captureStreamError, so only the event-boundary owner can
// classify it. An error event is a terminal statement on every wire that
// carries one; the first real error still wins.
func (a *Analyzer) captureStandaloneError(payload []byte) {
	var ev struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "error" {
		return
	}
	a.recordStandaloneError(ev.Code, ev.Message)
}

// recordStandaloneError registers the top-level code/message of an error
// event that carries no nested error object. The error event itself is the
// stream's terminal statement even when it names neither field.
func (a *Analyzer) recordStandaloneError(code, message json.RawMessage) {
	a.terminatorSeen = true
	if a.errType != "" {
		return
	}
	var msg string
	_ = json.Unmarshal(message, &msg)
	codeString := errorCodeString(code)
	if codeString == "" && msg == "" {
		// The event itself is the provider's terminal statement even when it
		// names neither field (an absent or null nested object, or an empty
		// Responses error event). Treat it as an upstream failure rather than
		// leaving a healthy record behind.
		a.setStreamError(TypeUpstreamError, "", "upstream response failed")
		return
	}
	a.setStreamError("provider_error", codeString, msg)
}

// SetLocalFailure records a proxy-minted failure as an upstream_error with the
// specific internal code. The synthetic frames the proxy substitutes for a
// withheld terminal are not provider statements, so the standalone-error
// classifier must not relabel them provider_error. The first-real-error rule
// still applies: a provider error that already owns the record is never
// overwritten.
func (a *Analyzer) SetLocalFailure(code, msg string) {
	if a.errType != "" {
		return
	}
	a.setStreamError(TypeUpstreamError, code, msg)
}

// captureResponseFailedError records the nested provider error of a Responses
// response.failed event. The same first-real-error rule applies.
func (a *Analyzer) captureResponseFailedError(payload []byte) {
	if a.errType != "" {
		return
	}
	a.setStreamError(metrics.ParseResponseFailedEnvelope(payload))
}

func (a *Analyzer) setStreamError(typ, code, msg string) {
	if typ == "" {
		return
	}
	a.errType, a.errCode, a.errMsg = typ, code, msg
}
