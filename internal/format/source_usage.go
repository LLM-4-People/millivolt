package format

// Source accounting for translated responses. The provider's usage object and
// cost keys belong to the source document; the translated client wire carries
// a normalized rendering and must never be read with the provider's overrides.

import (
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// SourceUsage extracts the source document's usage with the provider's custom
// keys and normalizes it into the canonical model. Anthropic's split cache
// counts fold into the input total; Chat and Responses already report an input
// total. The boolean reports whether the source carried a usage object.
func SourceUsage(protocol ir.Protocol, body []byte, usageKeys map[string]string) (ir.Usage, bool, error) {
	parsed, ok := metrics.ParseUsageBody(body, usageKeys)
	if !ok {
		return ir.Usage{}, false, nil
	}
	out := ir.Usage{
		OutputTokens:     parsed.OutputTokens,
		CacheReadTokens:  parsed.CacheReadTokens,
		CacheWriteTokens: parsed.CacheWrite,
		ReasoningTokens:  parsed.ReasoningTokens,
	}
	switch protocol {
	case ir.Anthropic:
		input, err := metrics.SumCounts(parsed.InputTokens, parsed.CacheReadTokens, parsed.CacheWrite)
		if err != nil {
			return ir.Usage{}, true, fmt.Errorf("usage: %w", err)
		}
		total, err := metrics.SumCounts(input, parsed.OutputTokens)
		if err != nil {
			return ir.Usage{}, true, fmt.Errorf("usage: %w", err)
		}
		out.InputTokens = input
		out.TotalTokens = total
	default:
		out.InputTokens = parsed.InputTokens
		total := parsed.TotalTokens
		if total == 0 {
			sum, err := metrics.SumCounts(parsed.InputTokens, parsed.OutputTokens)
			if err != nil {
				return ir.Usage{}, true, fmt.Errorf("usage: %w", err)
			}
			total = sum
		}
		out.TotalTokens = total
	}
	if err := validateUsage(out); err != nil {
		return ir.Usage{}, true, err
	}
	return out, true, nil
}
