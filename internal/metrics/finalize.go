package metrics

// FinalizeRecord populates the derived fields (TTFT/throughput/duration) on a
// record after all measurements are complete. Cost is NOT computed here: it is
// read directly from the provider's response by the usage extraction in
// internal/proxy, so it always reflects what the provider actually charged.
func FinalizeRecord(r *Record) {
	// TTFT is captured whenever any token arrived, but a fast upstream can
	// deliver sub-millisecond, which .Milliseconds() would truncate to 0 -
	// indistinguishable from "no tokens". Round a positive sub-ms TTFT up to
	// 1ms so "captured" is never reported as "absent".
	ttft := r.TTFT()
	r.TTFTMs = ttft.Milliseconds()
	if ttft > 0 && r.TTFTMs == 0 {
		r.TTFTMs = 1
	}
	r.OverallTPS = r.OverallThroughput()
	r.DurationMs = r.End.Sub(r.Start).Milliseconds()
	// Answer tokens = total output - reasoning (thinking) tokens. A tool-call-ONLY
	// response (finish_reason "tool_calls" with NO answer content - content:null)
	// bills all its completion_tokens as tool-call arguments, which are not
	// generation, so its answer count is zero. Gate on content PRESENCE
	// (HadAnswerContent), not answer timing: FirstAnswerAt is streaming-only and
	// would wrongly zero a non-streaming mixed content+tool_calls response. A
	// mixed response (content AND a tool call) keeps its real answer tokens; the
	// tool-arg tokens in completion_tokens stay inseparable - a documented floor.
	toolOnly := r.FinishReason == "tool_calls" && !r.HadAnswerContent
	if toolOnly {
		r.AnswerTokens = 0
	} else {
		r.AnswerTokens = max(0, r.Usage.OutputTokens-r.Usage.ReasoningTokens)
	}
	// Generation tokens (reasoning + answer content) - the gen-TPS numerator,
	// with tool-call argument tokens excluded. Two sources, in precedence order:
	//  1. The analyzer's chunk-derived GenTokens (set when the provider sent no
	//     usage blob): content+reasoning chunks, tool-arg chunks already removed.
	//  2. Provider usage: reasoning + answer (answer already zeroed for a
	//     tool-call-only response, so only reasoning counts as generation there).
	// A mixed content+tool-call response keeps tool-arg tokens in the provider's
	// inseparable completion_tokens - a documented floor, not separable wire-side.
	// Derived BEFORE DecodeTPS so GenThroughput reads the final value.
	if r.GenTokens == 0 {
		r.GenTokens = r.Usage.ReasoningTokens + r.AnswerTokens
	}
	r.DecodeTPS = r.GenThroughput()
}
