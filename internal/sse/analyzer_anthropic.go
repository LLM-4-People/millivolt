package sse

// Anthropic Messages SSE analysis. The Analyzer is the one observability owner
// for every relayed stream; this file teaches it the Anthropic event taxonomy
// without changing the Chat/Responses paths. Anthropic events are decoded
// whole (they are small and structured), then folded into the same analyzer
// state the Chat path fills: token timestamps, tool calls, finish reason,
// terminator, usage and in-band errors.

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type anthropicAnalyzerUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type anthropicAnalyzerEvent struct {
	Type string `json:"type"`
	// Message is raw because it is an object on message_start and a plain
	// string on a standalone error event; a typed object field would reject
	// the whole error frame.
	Message      json.RawMessage `json:"message"`
	Code         string          `json:"code"`
	ContentBlock struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicAnalyzerUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// feedAnthropic classifies one Anthropic data payload. It reports false for
// every non-Anthropic payload so the Chat/Responses path runs unchanged.
func (a *Analyzer) feedAnthropic(payload []byte, now time.Time) bool {
	// Cheap rejection: every Anthropic event carries a typed discriminator.
	// A Chat chunk has no top-level "type"; a Responses event's type starts
	// with "response.". The probe ignores whitespace so pretty-printed SSE
	// still reaches the decode; the decoded type is the real discriminator.
	if !bytes.Contains(payload, []byte(`"type"`)) {
		return false
	}
	var ev anthropicAnalyzerEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	switch ev.Type {
	case "message_start":
		// Every real Anthropic stream opens with message_start; marking the
		// wire here (not on a stray later event) keeps an isolated payload in
		// the corpus from being classified as a truncated stream.
		a.anthropicWire = true
		// The caller owns the chatlike seed: an Anthropic wire is not a
		// Chat/Responses feed, so its legitimate empty or thinking-only turn
		// must never enter the OpenAI degenerate substitution rules. Token,
		// usage, tool and finish accounting below still applies.
		var msg struct {
			ID    string                  `json:"id"`
			Model string                  `json:"model"`
			Usage *anthropicAnalyzerUsage `json:"usage"`
		}
		if json.Unmarshal(ev.Message, &msg) == nil {
			if msg.ID != "" {
				a.reqID = msg.ID
			}
			if msg.Model != "" {
				a.model = msg.Model
			}
			if msg.Usage != nil {
				a.setAnthropicUsage(msg.Usage)
			}
		}
		return true
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			a.toolCalls++
			a.retainToolName(ev.ContentBlock.Name)
			// A tool-only Anthropic stream must still record its first token
			// and a chunk count: the arguments are generated output even
			// though they are not answer text.
			a.recordAnthropicToolToken(now)
		}
		return true
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				a.recordAnthropicToken(now, true)
				if a.CapturePreview && a.preview == nil {
					a.preview = []byte(metrics.TruncatePreview(ev.Delta.Text))
				}
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				a.recordAnthropicToken(now, false)
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				a.recordAnthropicToolToken(now)
			}
		}
		return true
	case "message_delta":
		if ev.Usage != nil {
			a.setAnthropicUsage(ev.Usage)
		}
		if ev.Delta.StopReason != "" {
			a.finishReason = anthropicAnalyzerFinish(ev.Delta.StopReason)
			a.terminatorSeen = true
		}
		return true
	case "message_stop":
		a.terminatorSeen = true
		return true
	case "error":
		if ev.Error != nil {
			a.setStreamError(ev.Error.Type, "", ev.Error.Message)
		} else {
			// A standalone Responses error event has no nested error object:
			// its code and message are top-level siblings of the type. The
			// message is a plain string, decoded from the raw field.
			var msg string
			_ = json.Unmarshal(ev.Message, &msg)
			if ev.Code != "" || msg != "" {
				a.setStreamError("provider_error", ev.Code, msg)
			}
		}
		// An error event is a terminal statement on every wire that carries
		// one; never leave the analyzer waiting for a marker after it.
		a.terminatorSeen = true
		return true
	case "ping":
		return true
	}
	return false
}

func (a *Analyzer) recordAnthropicToken(now time.Time, answer bool) {
	if a.firstTokenAt.IsZero() {
		a.firstTokenAt = now
	}
	a.lastTokenAt = now
	a.chunkCount++
	if answer {
		if a.firstAnswerAt.IsZero() {
			a.firstAnswerAt = now
		}
		return
	}
	if a.firstReasoningAt.IsZero() {
		a.firstReasoningAt = now
	}
}

// recordAnthropicToolToken accounts one tool-call event as generated output:
// it sets TTFT and the chunk count but never marks answer text or reasoning.
func (a *Analyzer) recordAnthropicToolToken(now time.Time) {
	if a.firstTokenAt.IsZero() {
		a.firstTokenAt = now
	}
	a.lastTokenAt = now
	a.chunkCount++
	a.toolChunks++
}

// setAnthropicUsage folds Anthropic's split cache accounting into the
// Chat-shaped usage document Fill already understands. InputTokens is the
// total (input after the last breakpoint plus cache read and write).
func (a *Analyzer) setAnthropicUsage(u *anthropicAnalyzerUsage) {
	if u.InputTokens != nil {
		a.anthropicInput = *u.InputTokens
	}
	if u.OutputTokens != nil {
		a.anthropicOutput = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		a.anthropicCacheRead = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		a.anthropicCacheWrite = *u.CacheCreationInputTokens
	}
	if u.OutputTokensDetails != nil {
		a.anthropicThinking = u.OutputTokensDetails.ThinkingTokens
	}
	input, err := metrics.SumCounts(a.anthropicInput, a.anthropicCacheRead, a.anthropicCacheWrite)
	if err != nil {
		// Leave usage unreported rather than publishing a partial document;
		// the chunk fallback can still recover it from the provider frame.
		return
	}
	total, err := metrics.SumCounts(input, a.anthropicOutput)
	if err != nil {
		return
	}
	chat, err := json.Marshal(map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": a.anthropicOutput,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      a.anthropicCacheRead,
			"cache_write_tokens": a.anthropicCacheWrite,
		},
		"completion_tokens_details": map[string]any{"reasoning_tokens": a.anthropicThinking},
	})
	if err != nil {
		return
	}
	a.usage = chat
}

func anthropicAnalyzerFinish(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return reason
	}
}
