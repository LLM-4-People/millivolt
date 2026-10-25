package sse

import (
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// retainToolName records one distinct tool name in first-seen order. The
// retained list and its dedupe set are bounded by RetainedToolNamesMax, so a
// hostile stream cannot grow either; every wire family feeds this owner.
func (a *Analyzer) retainToolName(name string) {
	name = metrics.RetainToolName(name)
	if name == "" || len(a.toolNames) >= metrics.RetainedToolNamesMax {
		return
	}
	if a.toolNameSet == nil {
		a.toolNameSet = make(map[string]struct{}, metrics.RetainedToolNamesMax)
	}
	if _, duplicate := a.toolNameSet[name]; duplicate {
		return
	}
	a.toolNameSet[name] = struct{}{}
	a.toolNames = append(a.toolNames, name)
}

// Fill populates rec with the analyzer's accumulated state. Call after the
// stream has ended.
func (a *Analyzer) Fill(rec *metrics.Record) {
	// Flush a trailing event that ended at EOF without its blank-line
	// terminator (a stream can close right after the last data: line). This
	// surfaces an in-band error the provider sent as its final event.
	a.flushEvent(time.Now())

	rec.FirstTokenAt = a.firstTokenAt
	rec.LastTokenAt = a.lastTokenAt
	rec.FirstAnswerAt = a.firstAnswerAt
	// First reasoning-token time, persisted as absolute unix ms (0 = the
	// stream carried no reasoning block). The zero time must map to 0, never
	// to time.Time{}.UnixMilli()'s sentinel for year 1.
	if !a.firstReasoningAt.IsZero() {
		rec.FirstReasoningAt = a.firstReasoningAt.UnixMilli()
	}
	// The stream carried answer text content when a content chunk arrived
	// (firstAnswerAt set). This flags content PRESENCE for the tool-call-only
	// detection in FinalizeRecord - which must distinguish it from a mixed
	// content+tool_calls response on every path, streaming or not.
	rec.HadAnswerContent = !a.firstAnswerAt.IsZero()
	rec.FinishReason = a.finishReason
	rec.ToolCalls = a.toolCalls
	// The raw count of content-bearing chunks of this stream, persisted as
	// its own record column; the token fallbacks below already derive from it.
	rec.Chunks = a.chunkCount
	// Generation tokens (reasoning + answer content), excluding tool-call
	// argument chunks. When the provider sends no usage blob, chunkCount is the
	// token fallback - but it counts tool-arg chunks, which are not generation,
	// so subtract them. When usage IS present, decodeUsage fills
	// OutputTokens/ReasoningTokens below and GenTokens is finalized in
	// FinalizeRecord (reasoning + answer); leave it 0 here so we don't clash.
	if a.usage == nil && a.chunkCount > 0 {
		rec.GenTokens = a.chunkCount - a.toolChunks
	}
	if a.chunkCount > 0 && rec.Usage.OutputTokens == 0 {
		rec.Usage.OutputTokens = a.chunkCount
	}
	// Usage: the usage object when the stream carries one; otherwise the
	// terminal chunk root (nano-gpt streams have NO usage object - the token
	// counts live in the sibling pricing object, reachable via usage_keys
	// paths relative to the response root). The chunk-root decode MERGES only
	// what it actually reported, so unresolved paths never clobber the
	// chunk-count fallbacks above with zeros; when real counts land the
	// GenTokens fallback yields to FinalizeRecord's usage-based finalization
	// (the same contract as a present usage object).
	fallbackOutput := rec.Usage.OutputTokens
	restoredOutput := false
	if a.usage != nil {
		decodeUsage(a.usage, rec, a.UsageKeys)
		// A present usage object may omit output_tokens (a truncated
		// Anthropic stream reported only message_start) or be the translated
		// wire's all-zero placeholder. The streamed chunks are then the only
		// output count; restore both the output and the tool-excluded
		// generation count so FinalizeRecord cannot inflate either with
		// tool-argument chunks.
		if rec.Usage.OutputTokens == 0 && fallbackOutput > 0 {
			rec.Usage.OutputTokens = fallbackOutput
			rec.GenTokens = a.chunkCount - a.toolChunks
			restoredOutput = true
		}
		// Provider usage-key paths may live beside the usage object in the
		// raw chunk root; merge them without clobbering normalized counts.
		if len(a.usageChunk) > 0 {
			if m := mergeChunkUsage(a.usageChunk, rec, a.UsageKeys); m.merged {
				if m.outputLanded {
					rec.GenTokens = 0
				}
				a.usageSeen = true
			}
		}
	} else if len(a.usageChunk) > 0 {
		if m := mergeChunkUsage(a.usageChunk, rec, a.UsageKeys); m.merged {
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		}
	}
	if a.anthropicCustomSeen {
		m := mergeUsageValues(&rec.Usage, a.anthropicCustomUsage)
		if a.anthropicCustomTotal > 0 {
			rec.Usage.TotalTokens = a.anthropicCustomTotal
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		} else if total, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens); err != nil {
			rec.Usage = metrics.Usage{}
			a.usageSeen = false
			a.usageInvalid = true
		} else {
			rec.Usage.TotalTokens = total
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		}
	}
	// A restored output count is part of the turn's usage, so a provider
	// total captured before that restoration must not remain below
	// input+restored output. A custom Anthropic total captured at
	// message_start is similarly stale once a completed message_delta reports
	// output, even though that output was not "restored". The custom-total
	// merge above can overwrite the earlier value, so the consistency guard
	// runs once after every usage owner. A zero total stays zero (settlement
	// derives input+output), and a provider total at or above the sum stays
	// authoritative.
	if (restoredOutput || a.anthropicCustomTotal > 0) && rec.Usage.TotalTokens > 0 {
		if total, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens); err != nil {
			a.usageInvalid = true
		} else if rec.Usage.TotalTokens < total {
			rec.Usage.TotalTokens = total
		}
	}
	// Cost is read straight from the provider's response (dynamic), so it
	// reflects what the provider actually charged for this request. Every
	// usage-bearing or terminal chunk is scanned at capture time and the last
	// cost-bearing value is retained; a later usage-less chunk therefore
	// cannot erase an earlier charge. The bare usage object is the fallback,
	// so paths written against usage keep working.
	if a.costSeen {
		rec.Cost = a.cost
	} else if cost, ok := a.extractCost(a.usage); ok {
		rec.Cost = cost
	}
	if a.anthropicCostSeen {
		rec.Cost = a.anthropicCost
	}
	if a.preview != nil && rec.ResponsePreview == "" {
		rec.ResponsePreview = string(a.preview)
	}
	if a.model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = a.model
	}
	if a.reqID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = a.reqID
	}
	if len(a.toolNames) > 0 {
		rec.ToolNames = a.toolNames
	}
	// An in-band error payload is the provider's own statement of failure on
	// this stream - the client received it in-band, so it takes precedence
	// over a subsequent transport-level read error (stream_read_error),
	// which may just be the connection closing after the failure was sent.
	if a.errType != "" {
		rec.ErrorType = a.errType
		rec.ErrorCode = a.errCode
		rec.ErrorMsg = a.errMsg
	}
}

// Terminated reports whether a documented end-of-stream marker was seen.
func (a *Analyzer) Terminated() bool { return a.terminatorSeen }

// UsageReported reports whether the source actually supplied usage, including
// a custom usage-key path beside the usage object. It distinguishes an
// unreported stream (whose output count is the chunk fallback) from a
// provider-reported zero.
func (a *Analyzer) UsageReported() bool { return a.usageSeen }

// UsageInvalid reports whether a source usage document could not be summed or
// represented. The signal is sticky and independent of UsageReported, so the
// proxy can invalidate the record even when a later partial document did land.
func (a *Analyzer) UsageInvalid() bool { return a.usageInvalid }

// HasInBandError reports whether an in-band error payload was observed (a
// provider failure streamed inside a 200 - no synthetic stream-outcome
// signaling should stack on top of the provider's own).
func (a *Analyzer) HasInBandError() bool { return a.errType != "" }

// ResponsesStream reports whether the feed carried Responses-API typed events.
// A Responses stream needs its own terminal substitution rather than the
// OpenAI Chat error chunk.
func (a *Analyzer) ResponsesStream() bool { return a.responsesStream }

// ResponseID returns the Responses terminal document's id, or "" when the
// feed carried none.
func (a *Analyzer) ResponseID() string { return a.responseID }

// ChatLike reports whether the analyzed feed follows the Chat/Responses
// completion contract. The relay uses it to keep OpenAI degenerate
// substitution off foreign wires.
func (a *Analyzer) ChatLike() bool { return a.chatlike }
