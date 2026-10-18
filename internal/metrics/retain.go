package metrics

import "unicode/utf8"

// Exported client-label bounds are the single runtime owner shared with the
// proxy capture producers (classifyClient, remoteIP, clientLang and the user
// agent assignment). The producer and the record boundary must never diverge.
const (
	RetainedClientMax    = 256
	RetainedUserAgentMax = 1024
	RetainedClientIPMax  = 128
	// RetainedClientLangMax shares the classified client-label bound; language
	// is a client label with the same real-world size.
	RetainedClientLangMax = RetainedClientMax
)

// Record-retention bounds. These are internal guardrails, not configuration:
// they keep client-controlled paths/methods/labels/conversation identities,
// request metadata and provider-controlled metadata from carrying
// HTTP/body-sized text into the ring, the pending snapshots and the durable
// row. Real identifiers are far below every bound, so normal records pass
// through unchanged.
const (
	retainedRecordPathMax         = 1024
	retainedRecordMethodMax       = 32
	retainedRecordProviderTextMax = 512
	retainedRecordErrorMsgMax     = 512
	retainedRecordRequestMetaMax  = 512
	// retainedRecordConversationMax is the proxy's 512-byte declared-session
	// bound plus the two-byte s:/k: namespace prefix.
	retainedRecordConversationMax = 514
	retainedRecordToolNameMax     = 128
	// RetainedToolNamesMax is the maximum retained distinct tool-name count.
	// It is exported because the SSE analyzer applies the same cap while the
	// stream is still running, so its dedupe set can never grow past what the
	// record boundary would retain.
	RetainedToolNamesMax          = 64
	retainedRecordDebugSessionMax = 128
)

// recordTruncationSuffix marks a text value cut at a record-retention bound.
// It is part of the bound: a truncated value is never longer than max bytes.
const recordTruncationSuffix = "…"

// RetainRecord applies the record-boundary text bounds to a finalized record
// in place. It is the exported entry point for the durable enqueue boundary:
// Store.Record has no ring, so it cannot rely on Buffer.Record applying the
// shared retention pass. Both paths delegate to retainRecord, the one owner of
// the bounds.
func RetainRecord(r *Record) {
	if r == nil {
		return
	}
	retainRecord(r)
}

// retainRecord applies the record-boundary text bounds to a finalized record
// in place, before it enters the ring and durable queue. The record and its
// attempt slice are owned by the finalization path, so both are bounded in
// place; tool names are re-sliced only when the caller's list exceeds the
// count or byte bound, leaving the caller's backing array untouched.
func retainRecord(r *Record) {
	retainRecordTextFields(r)
	for i := range r.Attempts {
		retainAttemptText(&r.Attempts[i])
	}
}

// retainRecordSnapshot applies the same bounds to a PublishLive begin/update
// snapshot. The live record may still be streaming, so an oversized attempt is
// bounded on a copy instead of mutating a caller-owned attempt slice.
func retainRecordSnapshot(r *Record) {
	retainRecordTextFields(r)
	r.Attempts = retainedAttempts(r.Attempts)
}

// retainRecordTextFields bounds the record-owned text fields. Each field is
// only assigned when it actually exceeds its bound, so a normal record is not
// rewritten and no whole-record copy is needed.
func retainRecordTextFields(r *Record) {
	retainTextField(&r.Path, retainedRecordPathMax)
	retainTextField(&r.Method, retainedRecordMethodMax)
	retainTextField(&r.Provider, retainedRecordProviderTextMax)
	retainTextField(&r.Model, retainedRecordProviderTextMax)
	retainTextField(&r.ProviderModel, retainedRecordProviderTextMax)
	retainTextField(&r.ProviderRequestID, retainedRecordProviderTextMax)
	retainTextField(&r.ProviderServer, retainedRecordProviderTextMax)
	retainTextField(&r.FinishReason, retainedRecordProviderTextMax)
	retainTextField(&r.ErrorType, retainedRecordProviderTextMax)
	retainTextField(&r.ErrorCode, retainedRecordProviderTextMax)
	retainTextField(&r.ErrorMsg, retainedRecordErrorMsgMax)
	retainTextField(&r.Client, RetainedClientMax)
	retainTextField(&r.UserAgent, RetainedUserAgentMax)
	retainTextField(&r.ClientIP, RetainedClientIPMax)
	retainTextField(&r.ClientLang, RetainedClientLangMax)
	retainTextField(&r.ConversationID, retainedRecordConversationMax)
	retainTextField(&r.ParentConversationID, retainedRecordConversationMax)
	retainTextField(&r.ReqToolChoice, retainedRecordRequestMetaMax)
	retainTextField(&r.ReqResponseFormat, retainedRecordRequestMetaMax)
	retainTextField(&r.ReqServiceTier, retainedRecordRequestMetaMax)
	retainTextField(&r.LastTurnRole, retainedRecordRequestMetaMax)
	retainTextField(&r.DebugSessionID, retainedRecordDebugSessionMax)
	r.ToolNames = retainedToolNames(r.ToolNames)
}

// retainTextField bounds one string field when it exceeds max and reports
// whether it changed.
func retainTextField(dst *string, max int) bool {
	if len(*dst) <= max {
		return false
	}
	*dst = retainRecordText(*dst, max)
	return true
}

// retainRecordText cuts s to max bytes, backing off to a rune boundary and
// marking the cut. Values at or below max are returned unchanged.
func retainRecordText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return truncateUTF8(s, max-len(recordTruncationSuffix))
}

// truncateUTF8 cuts s at cut bytes, backs off to the prior rune boundary and
// appends the retention marker. Callers own the bound: TruncatePreview keeps
// its historical max-plus-marker shape, while retainRecordText subtracts the
// marker so its result never exceeds max.
func truncateUTF8(s string, cut int) string {
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + recordTruncationSuffix
}

// retainedToolNames keeps at most the first RetainedToolNamesMax names, each
// at most retainedRecordToolNameMax bytes, in order. It returns the input
// slice unchanged when every name fits, and otherwise returns a new slice so a
// caller-owned list is never mutated.
func retainedToolNames(in []string) []string {
	keep := len(in)
	if keep > RetainedToolNamesMax {
		keep = RetainedToolNamesMax
	}
	bounded := keep != len(in)
	if !bounded {
		for i := range in {
			if len(in[i]) > retainedRecordToolNameMax {
				bounded = true
				break
			}
		}
	}
	if !bounded {
		return in
	}
	out := make([]string, keep)
	for i := range out {
		out[i] = retainRecordText(in[i], retainedRecordToolNameMax)
	}
	return out
}

// retainAttemptText bounds one attempt's provider/error metadata in place and
// reports whether a field changed. Attempt order is untouched.
func retainAttemptText(a *RetryAttempt) bool {
	changed := retainTextField(&a.ErrorMsg, retainedRecordErrorMsgMax)
	if retainTextField(&a.ErrorType, retainedRecordProviderTextMax) {
		changed = true
	}
	if retainTextField(&a.ErrorCode, retainedRecordProviderTextMax) {
		changed = true
	}
	if retainTextField(&a.ProviderRequestID, retainedRecordProviderTextMax) {
		changed = true
	}
	if retainTextField(&a.ProviderServer, retainedRecordProviderTextMax) {
		changed = true
	}
	if retainTextField(&a.ProviderModel, retainedRecordProviderTextMax) {
		changed = true
	}
	return changed
}

// retainedAttempts bounds a caller-owned attempt slice on a copy, allocated
// only when at least one field exceeds its bound. Normal attempts keep the
// original slice header.
func retainedAttempts(in []RetryAttempt) []RetryAttempt {
	var out []RetryAttempt
	for i := range in {
		attempt := in[i]
		if !retainAttemptText(&attempt) {
			continue
		}
		if out == nil {
			out = make([]RetryAttempt, len(in))
			copy(out, in)
		}
		out[i] = attempt
	}
	if out == nil {
		return in
	}
	return out
}
