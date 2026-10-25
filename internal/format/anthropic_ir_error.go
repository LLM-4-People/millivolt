package format

import (
	"encoding/json"
	"errors"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// DecodeAnthropicError decodes the Anthropic error envelope carried by a
// non-2xx body or an SSE error event.
func DecodeAnthropicError(body []byte) (*ir.Error, error) {
	var in struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	typ, code, msg := metrics.ParseErrorComponents(in.Error)
	if typ == "" && code == "" && msg == "" {
		if !errorValuePresent(in.Error) {
			return nil, errors.New("not an anthropic error envelope")
		}
		typ, msg = "api_error", genericProviderErrorMessage
	}
	return boundedProviderError(typ, code, msg), nil
}

// EncodeAnthropicError renders the canonical error as an Anthropic error
// envelope.
func EncodeAnthropicError(e *ir.Error) ([]byte, error) {
	typ := anthropicErrorType(e.Type)
	return sse.MarshalWireJSON(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": e.Message,
		},
	})
}

// anthropicErrorType maps the shared OpenAI error vocabulary onto Anthropic's
// documented type set. Shared names pass through; an unknown or provider-only
// class becomes api_error, Anthropic's catch-all, rather than leaking a type
// its clients do not recognize.
func anthropicErrorType(typ string) string {
	switch typ {
	case "":
		return "api_error"
	case "server_error", "upstream_error", "content_policy_violation":
		return "api_error"
	case "insufficient_quota", "insufficient_credits":
		return "billing_error"
	case "context_length_exceeded":
		return "invalid_request_error"
	}
	switch typ {
	case "invalid_request_error", "authentication_error", "permission_error",
		"not_found_error", "request_too_large", "rate_limit_error",
		"api_error", "overloaded_error", "timeout_error", "billing_error":
		return typ
	}
	return "api_error"
}
