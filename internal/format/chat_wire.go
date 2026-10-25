package format

import (
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
)

// chatWireRequest is the request shape decoded from the Chat wire.
type chatWireRequest struct {
	Model               string             `json:"model"`
	System              string             `json:"system"`
	Messages            []chatWireMessage  `json:"messages"`
	MaxTokens           json.RawMessage    `json:"max_tokens"`
	MaxCompletionTokens json.RawMessage    `json:"max_completion_tokens"`
	Temperature         *float64           `json:"temperature"`
	TopP                *float64           `json:"top_p"`
	Stop                json.RawMessage    `json:"stop"`
	Seed                json.RawMessage    `json:"seed"`
	PresencePenalty     *float64           `json:"presence_penalty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty"`
	LogitBias           map[string]float64 `json:"logit_bias"`
	Logprobs            *bool              `json:"logprobs"`
	TopLogprobs         json.RawMessage    `json:"top_logprobs"`
	Tools               []chatWireTool     `json:"tools"`
	ToolChoice          json.RawMessage    `json:"tool_choice"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls"`
	ResponseFormat      json.RawMessage    `json:"response_format"`
	ReasoningEffort     string             `json:"reasoning_effort"`
	Stream              bool               `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage       bool `json:"include_usage"`
		IncludeObfuscation bool `json:"include_obfuscation"`
	} `json:"stream_options"`
	Metadata             map[string]json.RawMessage `json:"metadata"`
	ServiceTier          string                     `json:"service_tier"`
	User                 string                     `json:"user"`
	Verbosity            string                     `json:"verbosity"`
	SafetyIdentifier     string                     `json:"safety_identifier"`
	PromptCacheKey       string                     `json:"prompt_cache_key"`
	Moderation           *ir.Moderation             `json:"moderation"`
	PromptCacheOptions   *ir.PromptCacheOptions     `json:"prompt_cache_options"`
	PromptCacheRetention string                     `json:"prompt_cache_retention"`
	Store                *bool                      `json:"store"`
	N                    json.RawMessage            `json:"n"`

	// Legacy and provider-specific request fields that the canonical model
	// does not carry. Present values are reported or converted below.
	Functions        json.RawMessage `json:"functions"`
	FunctionCall     json.RawMessage `json:"function_call"`
	Prediction       json.RawMessage `json:"prediction"`
	Audio            json.RawMessage `json:"audio"`
	Modalities       []string        `json:"modalities"`
	WebSearchOptions json.RawMessage `json:"web_search_options"`
}

type chatWireMessage struct {
	Role             string             `json:"role"`
	Content          json.RawMessage    `json:"content"`
	Name             string             `json:"name"`
	ToolCalls        []chatWireToolCall `json:"tool_calls"`
	ToolCallID       string             `json:"tool_call_id"`
	Refusal          *string            `json:"refusal"`
	FunctionCall     json.RawMessage    `json:"function_call"`
	ReasoningContent string             `json:"reasoning_content"`
	Reasoning        string             `json:"reasoning"`
	// ReasoningText and ReasoningDetails are provider-specific Chat
	// compatibility spellings. Only the explicit plain-text member is
	// representable; structured details are reported as dropped. ReasoningText
	// stays raw because the fallback is an extension: a wrong-typed value is
	// ignored and reported, never a reason to abort a body the baseline read.
	ReasoningText    json.RawMessage      `json:"reasoning_text"`
	ReasoningDetails json.RawMessage      `json:"reasoning_details"`
	Annotations      []chatWireAnnotation `json:"annotations"`
	// Audio replays a prior assistant audio output. The canonical model has
	// no audio-output identity, so a present value is reported.
	Audio *struct {
		ID string `json:"id"`
	} `json:"audio"`
}

// chatWireAnnotation is the Chat message-level url_citation wrapper. Responses
// spells the same fields flat on the output_text part.
type chatWireAnnotation struct {
	Type        string `json:"type"`
	URLCitation *struct {
		URL        string        `json:"url"`
		Title      string        `json:"title"`
		StartIndex jsonnum.Int64 `json:"start_index"`
		EndIndex   jsonnum.Int64 `json:"end_index"`
	} `json:"url_citation"`
}

// chatLegacyFunction is one deprecated Chat "functions" entry: the function
// object without the modern tools wrapper.
type chatLegacyFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}
