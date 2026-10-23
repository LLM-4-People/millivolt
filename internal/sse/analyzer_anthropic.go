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
	Type    string `json:"type"`
	Message struct {
		ID    string                  `json:"id"`
		Model string                  `json:"model"`
		Usage *anthropicAnalyzerUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"content_block"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
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
	// with "response.".
	if !bytes.Contains(payload, []byte(`"type":"`)) {
		return false
	}
	var ev anthropicAnalyzerEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	switch ev.Type {
	case "message_start":
		a.chatlike = true
		if ev.Message.ID != "" {
			a.reqID = ev.Message.ID
		}
		if ev.Message.Model != "" {
			a.model = ev.Message.Model
		}
		if ev.Message.Usage != nil {
			a.setAnthropicUsage(ev.Message.Usage)
		}
		return true
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			a.toolCalls++
			a.retainToolName(ev.ContentBlock.Name)
		}
		return true
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			a.recordAnthropicToken(now, true)
		case "thinking_delta":
			a.recordAnthropicToken(now, false)
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
		}
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
	input := a.anthropicInput + a.anthropicCacheRead + a.anthropicCacheWrite
	chat, err := json.Marshal(map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": a.anthropicOutput,
		"total_tokens":      input + a.anthropicOutput,
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
