// Package ir defines the protocol-neutral request, response and streaming
// event model that every wire codec translates through. It is deliberately
// independent of transport, persistence and observability: a codec decodes one
// provider wire shape into these types, and another codec encodes them into a
// different wire shape.
//
// The model is not a lowest common denominator. It carries every semantic the
// three supported families can express, including opaque artifacts (Anthropic
// thinking signatures, Responses encrypted reasoning) with provenance, so a
// same-family round trip preserves them byte-for-byte. A cross-family encode
// that cannot express an artifact records a Warning instead of silently
// dropping it.
package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Protocol identifies one wire family.
type Protocol string

const (
	// Chat is the OpenAI Chat Completions wire (/v1/chat/completions).
	Chat Protocol = "openai-chat"
	// Responses is the OpenAI Responses wire (/v1/responses).
	Responses Protocol = "openai-responses"
	// Anthropic is the Anthropic Messages wire (/v1/messages).
	Anthropic Protocol = "anthropic"
	// Cursor is the Cursor agent.v1 Connect wire. It has no public HTTP
	// request/response shape; the proxy adapts it through the Chat codec.
	Cursor Protocol = "cursor"
)

// Valid reports whether p is a known protocol.
func (p Protocol) Valid() bool {
	switch p {
	case Chat, Responses, Anthropic, Cursor:
		return true
	}
	return false
}

// String returns the wire spelling.
func (p Protocol) String() string { return string(p) }

// Role is a conversation role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType identifies one content block.
type BlockType string

const (
	BlockText              BlockType = "text"
	BlockImage             BlockType = "image"
	BlockFile              BlockType = "file"
	BlockAudio             BlockType = "audio"
	BlockToolCall          BlockType = "tool_call"
	BlockToolResult        BlockType = "tool_result"
	BlockReasoning         BlockType = "reasoning"
	BlockRedactedReasoning BlockType = "redacted_reasoning"
	BlockRefusal           BlockType = "refusal"
)

// WarningKind classifies a translation loss.
type WarningKind string

const (
	// WarningDropped means the source field has no representation in the
	// target protocol and was omitted.
	WarningDropped WarningKind = "dropped"
	// WarningApproximated means the field was mapped through a documented
	// approximation (for example a reasoning budget to an effort level).
	WarningApproximated WarningKind = "approximated"
	// WarningOpaque means an opaque artifact was preserved in the IR but the
	// target protocol cannot consume it.
	WarningOpaque WarningKind = "opaque"
)

// Warning records one translation decision that changed the request or
// response. Codecs append warnings; the proxy surfaces them to the client and
// the operator. A warning never replaces a hard error for a structural
// feature the target cannot honor.
type Warning struct {
	Kind   WarningKind `json:"kind"`
	Field  string      `json:"field"`
	Detail string      `json:"detail,omitempty"`
}

// String renders one warning for a header or log line.
func (w Warning) String() string {
	if w.Detail == "" {
		return string(w.Kind) + ":" + w.Field
	}
	return string(w.Kind) + ":" + w.Field + ": " + w.Detail
}

// Warnings is an ordered list with a bounded rendering.
type Warnings []Warning

// Add appends a warning.
func (w *Warnings) Add(kind WarningKind, field, detail string) {
	*w = append(*w, Warning{Kind: kind, Field: field, Detail: detail})
}

// Header renders the warnings as a single header-safe value. It is bounded by
// the caller's header budget; the proxy truncates at the boundary.
func (w Warnings) Header() string {
	if len(w) == 0 {
		return ""
	}
	parts := make([]string, 0, len(w))
	for _, item := range w {
		parts = append(parts, item.String())
	}
	return strings.Join(parts, "; ")
}

// CacheControl is a provider cache breakpoint.
type CacheControl struct {
	Type string `json:"type,omitempty"` // "ephemeral"
	TTL  string `json:"ttl,omitempty"`  // "5m", "1h"
}

// Image is an image input.
type Image struct {
	// URL is an http(s) URL or a data: URL. Mutually exclusive with FileID
	// and Data.
	URL string `json:"url,omitempty"`
	// FileID references a provider-hosted file.
	FileID string `json:"file_id,omitempty"`
	// Data is base64-encoded image bytes. MediaType names its type.
	Data      string `json:"data,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	// Detail is "auto", "low", "high" or "original" where supported.
	Detail string `json:"detail,omitempty"`
}

// File is a document input.
type File struct {
	FileID    string `json:"file_id,omitempty"`
	FileData  string `json:"file_data,omitempty"` // base64
	FileURL   string `json:"file_url,omitempty"`
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Audio is an audio input.
type Audio struct {
	Data   string `json:"data,omitempty"`
	Format string `json:"format,omitempty"` // "mp3", "wav"
}

// ToolCall is one model tool invocation. Arguments is always a JSON object;
// codecs parse the wire string at the boundary so no downstream consumer has
// to deal with empty or malformed argument strings.
type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult is one tool result. Content is a block list because Anthropic
// and Responses can carry images and files, not only text.
type ToolResult struct {
	CallID  string  `json:"call_id"`
	Content []Block `json:"content,omitempty"`
	IsError bool    `json:"is_error,omitempty"`
}

// Reasoning is one reasoning artifact. Text and Summary are visible reasoning;
// Signature and Encrypted are opaque provider artifacts that must be replayed
// verbatim to the same provider family. Provider records which family minted
// the artifact so a cross-family encode can warn instead of pretending it is
// usable.
type Reasoning struct {
	ID        string   `json:"id,omitempty"`
	Text      string   `json:"text,omitempty"`
	Summary   []string `json:"summary,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Encrypted string   `json:"encrypted,omitempty"`
	Provider  Protocol `json:"provider,omitempty"`
}

// Citation is one source citation attached to a text block.
type Citation struct {
	Type       string `json:"type,omitempty"`
	CitedText  string `json:"cited_text,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

// Block is one content block. Exactly one payload field is populated according
// to Type.
type Block struct {
	Type BlockType `json:"type"`

	Text string `json:"text,omitempty"`

	Image *Image `json:"image,omitempty"`
	File  *File  `json:"file,omitempty"`
	Audio *Audio `json:"audio,omitempty"`

	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`

	Reasoning *Reasoning `json:"reasoning,omitempty"`
	Refusal   string     `json:"refusal,omitempty"`

	CacheControl *CacheControl `json:"cache_control,omitempty"`
	Citations    []Citation    `json:"citations,omitempty"`
}

// TextBlock builds a text block.
func TextBlock(text string) Block { return Block{Type: BlockText, Text: text} }

// Message is one conversation turn. Tool results are blocks inside a user
// message; the Chat encoder splits them back into role:"tool" messages and the
// Anthropic encoder keeps them first in the user turn, which makes the
// Anthropic ordering constraint structural.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content,omitempty"`
	// Name is the optional OpenAI participant name.
	Name string `json:"name,omitempty"`
}

// Tool is one function tool definition.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Strict       *bool           `json:"strict,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	// Type is "function" for ordinary tools. Provider-specific tool types are
	// rejected by cross-protocol encoders.
	Type string `json:"type,omitempty"`
}

// ToolChoiceMode is the normalized tool-selection mode.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// ToolChoice is the normalized tool selection. Parallel tool use is owned by
// Request.ParallelToolCalls: Chat and Responses carry it at the top level,
// Anthropic carries its inverse on tool_choice, and one owner prevents the two
// spellings from disagreeing.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// ResponseFormat is the normalized structured-output request.
type ResponseFormat struct {
	Type        string          `json:"type"` // "text", "json_object", "json_schema"
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ReasoningConfig is the normalized reasoning request.
type ReasoningConfig struct {
	Effort       string `json:"effort,omitempty"` // minimal, low, medium, high, xhigh, max
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
	Summary      string `json:"summary,omitempty"` // auto, concise, detailed
	// Context and Mode are Responses reasoning controls with no Chat or
	// Anthropic spelling.
	Context string `json:"context,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	Display string `json:"display,omitempty"` // summarized, omitted
}

// Request is one normalized inference request.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages,omitempty"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	// ParallelToolCalls is the top-level parallel tool-use preference. Chat
	// and Responses carry it directly; Anthropic carries its inverse on
	// tool_choice.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// CacheControl is an Anthropic request-level automatic cache marker that
	// applies to the last cacheable block. Chat and Responses have no
	// request-level equivalent.
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	MaxOutputTokens  *int               `json:"max_output_tokens,omitempty"`
	Temperature      *float64           `json:"temperature,omitempty"`
	TopP             *float64           `json:"top_p,omitempty"`
	TopK             *int               `json:"top_k,omitempty"`
	Stop             []string           `json:"stop,omitempty"`
	Seed             *int64             `json:"seed,omitempty"`
	PresencePenalty  *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64           `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs         *bool              `json:"logprobs,omitempty"`
	TopLogprobs      *int               `json:"top_logprobs,omitempty"`

	ResponseFormat *ResponseFormat  `json:"response_format,omitempty"`
	Reasoning      *ReasoningConfig `json:"reasoning,omitempty"`

	Stream             bool              `json:"stream,omitempty"`
	StreamIncludeUsage bool              `json:"stream_include_usage,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	ServiceTier        string            `json:"service_tier,omitempty"`
	User               string            `json:"user,omitempty"`

	// Instructions is the Responses top-level instructions string. Leading
	// system/developer messages are folded into it by the Responses encoder.
	Instructions *string `json:"instructions,omitempty"`

	// Store and PreviousResponseID are Responses statefulness. The proxy is
	// stateless; a non-nil Store=true or a non-empty PreviousResponseID is
	// rejected before translation.
	Store              *bool  `json:"store,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`

	Warnings Warnings `json:"warnings,omitempty"`
}

// FinishReason is the normalized completion cause.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishPause         FinishReason = "pause"
	FinishCancelled     FinishReason = "cancelled"
	FinishError         FinishReason = "error"
	FinishOther         FinishReason = "other"
)

// Usage is normalized token accounting. Cache reads and writes stay separate
// because collapsing them breaks cost accounting.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
}

// Response is one normalized completion.
type Response struct {
	ID      string `json:"id,omitempty"`
	Model   string `json:"model,omitempty"`
	Created int64  `json:"created,omitempty"`

	Blocks []Block `json:"blocks,omitempty"`

	Finish       FinishReason `json:"finish"`
	StopSequence string       `json:"stop_sequence,omitempty"`

	Usage Usage `json:"usage"`

	// Status is the Responses lifecycle status when the source carried one.
	Status           string `json:"status,omitempty"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`

	// Error is set when the provider returned a terminal failure inside a
	// successful HTTP response (Responses status:"failed"). The proxy renders
	// it as a protocol-native error instead of a completion.
	Error *Error `json:"error,omitempty"`

	Warnings Warnings `json:"warnings,omitempty"`
}

// EventType identifies one normalized stream event.
type EventType string

const (
	EventStart        EventType = "start"
	EventBlockStart   EventType = "block_start"
	EventBlockDelta   EventType = "block_delta"
	EventBlockEnd     EventType = "block_end"
	EventMessageDelta EventType = "message_delta"
	EventDone         EventType = "done"
	EventError        EventType = "error"
)

// Delta is one incremental block update.
type Delta struct {
	Text      string    `json:"text,omitempty"`
	Reasoning string    `json:"reasoning,omitempty"`
	Signature string    `json:"signature,omitempty"`
	Arguments string    `json:"arguments,omitempty"`
	Citation  *Citation `json:"citation,omitempty"`
}

// Error is a normalized stream or response error.
type Error struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// Event is one normalized stream event. A stream is a sequence of
// EventStart, zero or more block lifecycles, EventMessageDelta, EventDone. An
// EventError terminates abnormally.
type Event struct {
	Type EventType `json:"type"`

	Index int    `json:"index,omitempty"`
	Block *Block `json:"block,omitempty"`
	Delta *Delta `json:"delta,omitempty"`

	Response *Response `json:"response,omitempty"`
	Usage    *Usage    `json:"usage,omitempty"`
	Finish   *Finish   `json:"finish,omitempty"`
	Error    *Error    `json:"error,omitempty"`

	// Abort marks a BlockEnd produced by a fail-closed decoder flush. Encoders
	// close the block but must not flush payload they retained for it: the
	// stream is being terminated by an error, and retained data may exceed the
	// budget that caused the failure. It is never set by a provider event.
	Abort bool `json:"abort,omitempty"`
}

// Finish is the terminal cause carried by EventMessageDelta.
type Finish struct {
	Reason       FinishReason `json:"reason"`
	StopSequence string       `json:"stop_sequence,omitempty"`
}

// ValidateRequest enforces the invariants every codec relies on. It is called
// after decoding and before encoding so a malformed IR cannot reach a wire.
func ValidateRequest(r *Request) error {
	if r == nil {
		return fmt.Errorf("ir: nil request")
	}
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("ir: model is required")
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens < 0 {
		return fmt.Errorf("ir: max_output_tokens must not be negative")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("ir: temperature must be between 0 and 2")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return fmt.Errorf("ir: top_p must be between 0 and 1")
	}
	if r.TopK != nil && *r.TopK < 0 {
		return fmt.Errorf("ir: top_k must not be negative")
	}
	if r.TopLogprobs != nil && (*r.TopLogprobs < 0 || *r.TopLogprobs > 20) {
		return fmt.Errorf("ir: top_logprobs must be between 0 and 20")
	}
	// Store and PreviousResponseID are Responses statefulness. They are
	// preserved in the IR so a Responses target can honor them; a stateless
	// target's encoder rejects previous_response_id and store=true.
	for i, m := range r.Messages {
		if m.Role != RoleSystem && m.Role != RoleDeveloper && m.Role != RoleUser && m.Role != RoleAssistant {
			return fmt.Errorf("ir: message %d has unsupported role %q", i, m.Role)
		}
		for j, b := range m.Content {
			if err := validateBlock(b); err != nil {
				return fmt.Errorf("ir: message %d block %d: %w", i, j, err)
			}
		}
	}
	for i, t := range r.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("ir: tool %d has no name", i)
		}
		if t.Type != "" && t.Type != "function" {
			return fmt.Errorf("ir: tool %d has unsupported type %q", i, t.Type)
		}
		if params := bytes.TrimSpace(t.Parameters); len(params) > 0 {
			if !json.Valid(params) || params[0] != '{' {
				return fmt.Errorf("ir: tool %d parameters must be a JSON object", i)
			}
		}
	}
	if r.ToolChoice != nil {
		switch r.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		case ToolChoiceTool:
			// A non-string wire name is rejected by the decoder; a tool
			// choice without a real name would broaden access to every tool.
			if strings.TrimSpace(r.ToolChoice.Name) == "" {
				return fmt.Errorf("ir: tool_choice mode %q requires a name", r.ToolChoice.Mode)
			}
		default:
			return fmt.Errorf("ir: unsupported tool_choice mode %q", r.ToolChoice.Mode)
		}
	}
	if r.ResponseFormat != nil {
		switch r.ResponseFormat.Type {
		case "", "text", "json_object":
		case "json_schema":
			if len(r.ResponseFormat.Schema) == 0 {
				return fmt.Errorf("ir: json_schema response format requires a schema")
			}
		default:
			return fmt.Errorf("ir: unsupported response format %q", r.ResponseFormat.Type)
		}
	}
	return nil
}

func validateBlock(b Block) error {
	switch b.Type {
	case BlockText:
		return nil
	case BlockImage:
		if b.Image == nil {
			return fmt.Errorf("image block has no image payload")
		}
		if b.Image.URL == "" && b.Image.FileID == "" && b.Image.Data == "" {
			return fmt.Errorf("image block has no url, file_id or data")
		}
		if b.Image.Data != "" && b.Image.MediaType == "" {
			return fmt.Errorf("inline image data has no media_type")
		}
	case BlockFile:
		if b.File == nil {
			return fmt.Errorf("file block has no file payload")
		}
		if b.File.FileID == "" && b.File.FileData == "" && b.File.FileURL == "" {
			return fmt.Errorf("file block has no file_id, file_data or file_url")
		}
		if b.File.FileData != "" && b.File.MediaType == "" {
			return fmt.Errorf("inline file data has no media_type")
		}
	case BlockAudio:
		if b.Audio == nil || b.Audio.Data == "" {
			return fmt.Errorf("audio block has no data")
		}
	case BlockToolCall:
		if b.ToolCall == nil || strings.TrimSpace(b.ToolCall.Name) == "" {
			return fmt.Errorf("tool_call block has no name")
		}
		if args := bytes.TrimSpace(b.ToolCall.Arguments); len(args) > 0 {
			if !json.Valid(args) {
				return fmt.Errorf("tool_call arguments are not valid JSON")
			}
			if args[0] != '{' {
				return fmt.Errorf("tool_call arguments must be a JSON object")
			}
		}
	case BlockToolResult:
		if b.ToolResult == nil || strings.TrimSpace(b.ToolResult.CallID) == "" {
			return fmt.Errorf("tool_result block has no call_id")
		}
	case BlockReasoning, BlockRedactedReasoning:
		if b.Reasoning == nil {
			return fmt.Errorf("reasoning block has no payload")
		}
	case BlockRefusal:
		return nil
	default:
		return fmt.Errorf("unsupported block type %q", b.Type)
	}
	return nil
}

// ValidateResponse enforces the response invariants.
func ValidateResponse(r *Response) error {
	if r == nil {
		return fmt.Errorf("ir: nil response")
	}
	for i, b := range r.Blocks {
		if err := validateBlock(b); err != nil {
			return fmt.Errorf("ir: response block %d: %w", i, err)
		}
	}
	return nil
}
