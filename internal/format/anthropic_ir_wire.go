package format

import (
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
)

// Anthropic Messages wire codec. The decoder accepts the request and response
// shapes documented for /v1/messages; the encoder emits the same shapes. The
// IR keeps Anthropic thinking signatures and redacted-thinking payloads opaque
// and tagged with their provider, so an Anthropic-to-Anthropic round trip is
// byte-faithful and a cross-family encode warns instead of silently dropping
// them.

type anthropicWireRequest struct {
	Model         string              `json:"model"`
	Messages      []anthropicWireMsg  `json:"messages"`
	System        json.RawMessage     `json:"system"`
	MaxTokens     json.RawMessage     `json:"max_tokens"`
	Stream        bool                `json:"stream"`
	Temperature   *float64            `json:"temperature"`
	TopP          *float64            `json:"top_p"`
	TopK          json.RawMessage     `json:"top_k"`
	StopSequences []string            `json:"stop_sequences"`
	Tools         []anthropicWireTool `json:"tools"`
	ToolChoice    json.RawMessage     `json:"tool_choice"`
	Thinking      json.RawMessage     `json:"thinking"`
	CacheControl  *ir.CacheControl    `json:"cache_control"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	ServiceTier  string          `json:"service_tier"`
	Speed        string          `json:"speed"`
	OutputConfig json.RawMessage `json:"output_config"`
	// Stateful and data-residency controls the canonical request cannot carry.
	// Present values are reported rather than silently dropped; diagnostics
	// also covers its nested previous_message_id.
	Container    json.RawMessage `json:"container"`
	Diagnostics  json.RawMessage `json:"diagnostics"`
	InferenceGeo string          `json:"inference_geo"`
}

type anthropicWireMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicWireTool struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	InputSchema  json.RawMessage  `json:"input_schema"`
	Strict       *bool            `json:"strict"`
	CacheControl *ir.CacheControl `json:"cache_control"`

	// Provider-only tool knobs. They have no cross-protocol equivalent and
	// are reported rather than silently dropped.
	AllowedCallers      json.RawMessage `json:"allowed_callers"`
	DeferLoading        *bool           `json:"defer_loading"`
	InputExamples       json.RawMessage `json:"input_examples"`
	EagerInputStreaming *bool           `json:"eager_input_streaming"`
}

type anthropicWireBlock struct {
	Type string `json:"type"`

	Text string `json:"text"`
	// Citations is raw because the member is block-type-aware: text, system
	// and tool-result text blocks carry a citation array, while a document
	// block carries the CitationsConfigParam object ({"enabled":true}).
	Citations    json.RawMessage  `json:"citations"`
	CacheControl *ir.CacheControl `json:"cache_control"`

	Source *struct {
		Type      string          `json:"type"`
		MediaType string          `json:"media_type"`
		Data      string          `json:"data"`
		URL       string          `json:"url"`
		FileID    string          `json:"file_id"`
		Content   json.RawMessage `json:"content"`
	} `json:"source"`
	Title string `json:"title"`
	// Context is a document-only hint with no cross-protocol equivalent.
	Context json.RawMessage `json:"context"`

	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// Caller and ToolsetName describe a server-side tool call. They are
	// retained on the wire model so their presence is reported instead of
	// being silently discarded by encoding/json.
	Caller      json.RawMessage `json:"caller"`
	ToolsetName string          `json:"toolset_name"`

	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`

	// Transformations is an image-only knob (oversized_image: downsize or
	// error) and changes request semantics; the canonical model has no
	// equivalent, so its presence is reported.
	Transformations json.RawMessage `json:"transformations"`

	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

// anthropicWireResponse is the non-streaming Anthropic response shape.
type anthropicWireResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicWireBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence string               `json:"stop_sequence"`
	StopDetails  json.RawMessage      `json:"stop_details"`
	// Container and Diagnostics are required nullable response members with
	// no cross-protocol equivalent. Keep them raw so a non-null source value
	// can be reported instead of silently dropped.
	Container   json.RawMessage    `json:"container"`
	Diagnostics json.RawMessage    `json:"diagnostics"`
	Usage       anthropicWireUsage `json:"usage"`
}

type anthropicWireUsage struct {
	InputTokens  jsonnum.Int64 `json:"input_tokens"`
	OutputTokens jsonnum.Int64 `json:"output_tokens"`
	// The Anthropic schema declares both cache counters number or null, so an
	// explicit null must decode as zero rather than fail the whole response.
	// The streaming owner uses the same pointer shape.
	CacheCreationInputTokens *jsonnum.Int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *jsonnum.Int64 `json:"cache_read_input_tokens"`
	// ServiceTier and Speed describe the serving tier of this usage. They
	// exist only on the response side; the request spells the tier through
	// service_tier/speed as well.
	ServiceTier string `json:"service_tier"`
	Speed       string `json:"speed"`
	// CacheCreation, InferenceGeo and ServerToolUse are required nullable
	// members with no canonical usage equivalent. They stay raw so a non-null
	// provider value can be reported as dropped.
	CacheCreation       json.RawMessage `json:"cache_creation"`
	InferenceGeo        json.RawMessage `json:"inference_geo"`
	ServerToolUse       json.RawMessage `json:"server_tool_use"`
	OutputTokensDetails *struct {
		ThinkingTokens jsonnum.Int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}
