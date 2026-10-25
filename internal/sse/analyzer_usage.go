package sse

import (
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// captureUsage records the last usage object of one payload. The enclosing
// chunk is kept too (copied - payload aliases the reused line buffer):
// cost_keys paths are relative to the response root, and cost or token counts
// may live in a sibling of usage.
func (a *Analyzer) captureUsage(keys *frameKeys, payload []byte) {
	raw := extractObject(keys.usage)
	if raw == nil {
		return
	}
	a.usage = append(a.usage[:0], raw...)
	a.usageChunk = append(a.usageChunk[:0], payload...)
	a.usageSeen = true
	a.captureCost(payload)
}

// captureCost retains the last cost-bearing payload. Cost keys are relative to
// the response root and providers may place the charge on an earlier chunk
// than the usage they finish with, so the value is captured when it resolves
// instead of re-read from whichever chunk happens to be last.
func (a *Analyzer) captureCost(payload []byte) {
	if cost, ok := a.extractCost(payload); ok {
		a.cost, a.costSeen = cost, true
	}
}

// extractCost runs the cost-key scan over a raw JSON document (the chunk
// root or the bare usage object). Returns (0, false) when the bytes are
// absent or unparseable - "no cost reported", never a fabricated number.
//
// The generic decode belongs to metrics.ExtractCost, not to this caller: that
// owner gates the decode behind the zero-allocation cost-key scan
// (metrics.hasCostKey), including the \u escape fallback, so a document that
// cannot carry a cost is never decoded at all. Decoding here first would pay
// the whole-document materialization the gate exists to avoid, on every usage
// chunk of every stream.
func (a *Analyzer) extractCost(raw []byte) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	return metrics.ExtractCost(raw, a.CostKeys)
}

// decodeUsage parses the "usage":{...} JSON fragment captured from the final
// chunk into rec.Usage, using the canonical dynamic parser (auto-detects
// OpenAI/Anthropic key names, honors per-provider overrides). This happens
// once per stream, not per event.
func decodeUsage(raw json.RawMessage, rec *metrics.Record, usageKeys map[string]string) {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	rec.Usage = metrics.ParseUsage(obj, usageKeys)
}

// mergeChunkUsage decodes a usage-free terminal chunk (the stream's response
// document) and folds any token counts the provider reported in a sibling
// object (via usage_keys paths) into the record - WITHOUT replacing the
// chunk-count fallbacks: only positive values are merged. Returns whether
// anything merged.
func mergeChunkUsage(raw []byte, rec *metrics.Record, usageKeys map[string]string) usageMerge {
	var obj any
	if json.Unmarshal(raw, &obj) != nil {
		return usageMerge{}
	}
	return mergeUsageValues(&rec.Usage, metrics.ParseUsage(obj, usageKeys))
}

// usageMerge reports what one merge actually landed. OutputLanded gates the
// chunk-derived GenTokens fallback: only a provider output count may displace
// it, so a custom path that reports input alone keeps the tool-excluded chunk
// count.
type usageMerge struct {
	merged       bool
	outputLanded bool
}

// mergeUsageValues folds only positive counts into dst. A custom path that
// resolves to zero never clobbers a standard count or the chunk fallback.
func mergeUsageValues(dst *metrics.Usage, src metrics.Usage) usageMerge {
	var out usageMerge
	for _, f := range []struct {
		got   int64
		dst   *int64
		field string
	}{
		{src.InputTokens, &dst.InputTokens, "input_tokens"},
		{src.OutputTokens, &dst.OutputTokens, "output_tokens"},
		{src.TotalTokens, &dst.TotalTokens, "total_tokens"},
		{src.CacheReadTokens, &dst.CacheReadTokens, "cache_read_tokens"},
		{src.CacheWrite, &dst.CacheWrite, "cache_write_tokens"},
		{src.ReasoningTokens, &dst.ReasoningTokens, "reasoning_tokens"},
	} {
		if f.got > 0 {
			*f.dst = f.got
			out.merged = true
			if f.field == "output_tokens" {
				out.outputLanded = true
			}
		}
	}
	return out
}
