package format

import (
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
)

// OpenAI Responses wire codec. Responses is item-oriented: input and output
// are ordered heterogeneous item lists, not a message array. The IR keeps the
// item identity and opaque reasoning payloads so a Responses-to-Responses
// round trip is faithful; cross-protocol encoders flatten items into messages
// and warn when an item has no equivalent.

type responsesWireRequest struct {
	Model             string              `json:"model"`
	Input             json.RawMessage     `json:"input"`
	Instructions      *string             `json:"instructions"`
	MaxOutputTokens   json.RawMessage     `json:"max_output_tokens"`
	Temperature       *float64            `json:"temperature"`
	TopP              *float64            `json:"top_p"`
	TopLogprobs       json.RawMessage     `json:"top_logprobs"`
	Tools             []responsesWireTool `json:"tools"`
	ToolChoice        json.RawMessage     `json:"tool_choice"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls"`
	Reasoning         *struct {
		Effort          string `json:"effort"`
		Summary         string `json:"summary"`
		GenerateSummary string `json:"generate_summary"`
		Context         string `json:"context"`
		Mode            string `json:"mode"`
	} `json:"reasoning"`
	Text *struct {
		Format    json.RawMessage `json:"format"`
		Verbosity string          `json:"verbosity"`
	} `json:"text"`
	Stream               bool                   `json:"stream"`
	Store                *bool                  `json:"store"`
	PreviousResponseID   string                 `json:"previous_response_id"`
	Metadata             boundedStringMap       `json:"metadata"`
	ServiceTier          string                 `json:"service_tier"`
	User                 string                 `json:"user"`
	SafetyIdentifier     string                 `json:"safety_identifier"`
	PromptCacheKey       string                 `json:"prompt_cache_key"`
	Moderation           *ir.Moderation         `json:"moderation"`
	PromptCacheOptions   *ir.PromptCacheOptions `json:"prompt_cache_options"`
	PromptCacheRetention string                 `json:"prompt_cache_retention"`
	Include              []string               `json:"include"`
	Truncation           string                 `json:"truncation"`
	Background           bool                   `json:"background"`
	MaxToolCalls         json.RawMessage        `json:"max_tool_calls"`
	Conversation         json.RawMessage        `json:"conversation"`
	Prompt               json.RawMessage        `json:"prompt"`
	ContextManagement    json.RawMessage        `json:"context_management"`
	AccessPrograms       json.RawMessage        `json:"access_programs"`
	StreamOptions        json.RawMessage        `json:"stream_options"`
}

type responsesWireTool struct {
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Parameters     json.RawMessage `json:"parameters"`
	Strict         *bool           `json:"strict"`
	AllowedCallers json.RawMessage `json:"allowed_callers"`
	Async          *bool           `json:"async"`
	DeferLoading   *bool           `json:"defer_loading"`
	OutputSchema   json.RawMessage `json:"output_schema"`
}

// responsesWireAnnotation is one output_text annotation. The flat url shape
// is also the source field set for a Chat url_citation, so both decoders
// share the same IR mapping.
type responsesWireAnnotation struct {
	Type       string        `json:"type"`
	URL        string        `json:"url"`
	Title      string        `json:"title"`
	StartIndex jsonnum.Int64 `json:"start_index"`
	EndIndex   jsonnum.Int64 `json:"end_index"`
	FileID     string        `json:"file_id"`
	Filename   string        `json:"filename"`
}

type responsesWireItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Phase   string          `json:"phase"`
	Content json.RawMessage `json:"content"`

	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`

	ID               string `json:"id"`
	EncryptedContent string `json:"encrypted_content"`
	Summary          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}
