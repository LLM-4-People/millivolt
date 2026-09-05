package format

// Cursor model discovery: agent.v1.AgentService/GetUsableModels → OpenAI
// GET /v1/models shape.
//
// GetUsableModels is a UNARY Connect RPC: the request body is a raw (unframed)
// GetUsableModelsRequest protobuf message with content-type application/proto,
// and the response is a raw GetUsableModelsResponse protobuf message. No
// Connect envelope, no bidi stream. The response's ModelDetails entries are a
// DISPLAY catalog only - they carry the model id, display names, aliases, a
// thinking presence-flag, and a max_mode bool, but NO context-window or
// capability fields (Cursor does not expose those on agent.v1; they live on
// the request-side model spec, or on the separate aiserver.v1 AvailableModels
// RPC which requires the device-fingerprint checksum this path omits).
//
// Field numbers from cursor2api's vendored proto/agent.proto, cross-confirmed
// with cliproxy-cursor-plugin's generated descriptor.

import (
	"fmt"
	"time"
)

// GetUsableModels / ModelDetails field numbers (agent.v1 wire contract).
const (
	// GetUsableModelsRequest.custom_model_ids (repeated string) - optional; an
	// empty request body lists the default usable set.
	fGetUsableModelsRequestCustomModelIDs = 1
	// GetUsableModelsResponse.models (repeated ModelDetails).
	fGetUsableModelsResponseModels = 1

	// ModelDetails fields. (fModelDetailsModelID=1 is declared in cursor.go.)
	fModelDetailsThinkingDetails  = 2 // ThinkingDetails (empty message; presence = can think)
	fModelDetailsDisplayModelID   = 3 // string
	fModelDetailsDisplayName      = 4 // string
	fModelDetailsDisplayNameShort = 5 // string
	fModelDetailsAliases          = 6 // repeated string
	fModelDetailsMaxMode          = 7 // bool (optional)
)

// EncodeGetUsableModelsRequest builds the raw (unframed) unary request body.
// customModelIDs is normally empty (list the default usable set).
func EncodeGetUsableModelsRequest(customModelIDs []string) []byte {
	var body []byte
	for _, id := range customModelIDs {
		body = appendString(body, fGetUsableModelsRequestCustomModelIDs, id)
	}
	return body
}

// CursorModel is one usable model, decoded from a ModelDetails entry.
type CursorModel struct {
	ID          string   // model_id (wire id)
	DisplayName string   // best display name (f4 → f5 → f3 → first alias → id)
	Aliases     []string // alternate ids
	Thinking    bool     // thinking_details present
	MaxMode     bool     // max_mode flag
}

// ParseGetUsableModelsResponse decodes a raw GetUsableModelsResponse body into
// the usable model list. Fail-closed on a malformed body. The response is
// unary raw protobuf; if a server ever wraps it in a Connect envelope, the
// caller strips that before calling here.
func ParseGetUsableModelsResponse(body []byte) ([]CursorModel, error) {
	fields, err := parseProtoFields(body)
	if err != nil {
		return nil, fmt.Errorf("decode GetUsableModelsResponse: %w", err)
	}
	var out []CursorModel
	for _, f := range fields {
		if f.num != fGetUsableModelsResponseModels {
			continue
		}
		md, err := subFields(&f)
		if err != nil {
			return nil, fmt.Errorf("decode ModelDetails: %w", err)
		}
		m := CursorModel{}
		var displayModelID, displayName, displayNameShort string
		for _, mf := range md {
			switch mf.num {
			case fModelDetailsModelID:
				m.ID = string(mf.raw)
			case fModelDetailsThinkingDetails:
				m.Thinking = true // presence flag; the message is empty
			case fModelDetailsDisplayModelID:
				displayModelID = string(mf.raw)
			case fModelDetailsDisplayName:
				displayName = string(mf.raw)
			case fModelDetailsDisplayNameShort:
				displayNameShort = string(mf.raw)
			case fModelDetailsAliases:
				m.Aliases = append(m.Aliases, string(mf.raw))
			case fModelDetailsMaxMode:
				m.MaxMode = mf.n != 0
			}
		}
		if m.ID == "" {
			continue // skip entries without a usable wire id
		}
		// Display-name resolution: display_name → display_name_short →
		// display_model_id → first alias → model_id (matches working clients).
		m.DisplayName = displayName
		if m.DisplayName == "" {
			m.DisplayName = displayNameShort
		}
		if m.DisplayName == "" {
			m.DisplayName = displayModelID
		}
		if m.DisplayName == "" && len(m.Aliases) > 0 {
			m.DisplayName = m.Aliases[0]
		}
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		out = append(out, m)
	}
	return out, nil
}

// CursorModelEntries renders the usable model list as OpenAI model entries
// (the data[] items of a GET /v1/models response; the caller owns the list
// envelope). defaultContext is applied to every model only when > 0 (an
// operator-supplied hint - Cursor's agent.v1 does not expose per-model context
// windows, and we never ship a static per-model table). Reasoning/max-mode map
// to OpenAI-ish capability fields so clients can read them.
func CursorModelEntries(models []CursorModel, defaultContext int) []map[string]any {
	now := time.Now().Unix()
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		e := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": "cursor",
		}
		if m.DisplayName != "" {
			e["display_name"] = m.DisplayName
		}
		if m.Thinking {
			e["reasoning"] = true
		}
		if m.MaxMode {
			e["max_mode"] = true
		}
		if defaultContext > 0 {
			e["context_window"] = defaultContext
		}
		entries = append(entries, e)
	}
	return entries
}
