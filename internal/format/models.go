package format

// Anthropic GET /v1/models → OpenAI list shape.
//
// Anthropic's models endpoint returns a paginated, differently-shaped body:
//
//	{"data":[{"id","type":"model","display_name","created_at","capabilities":{…},
//	  "max_input_tokens","max_tokens"}], "first_id","last_id","has_more"}
//
// OpenAI expects {"object":"list","data":[{"id","object":"model","created",
// "owned_by",…}]}. This converts one Anthropic page into the OpenAI shape so a
// client speaking OpenAI can list Anthropic models through the proxy. The
// Anthropic-only extras that have an OpenAI-ish meaning are surfaced
// (display_name, context_window from max_input_tokens); pagination cursors are
// followed by the caller (the proxy pages server-side and merges).

import (
	"encoding/json"
	"fmt"
	"time"
)

// anthropicModelsPage is the subset of Anthropic's models response we read.
type anthropicModelsPage struct {
	Data []struct {
		ID             string `json:"id"`
		DisplayName    string `json:"display_name"`
		CreatedAt      string `json:"created_at"` // RFC 3339
		MaxInputTokens int    `json:"max_input_tokens"`
	} `json:"data"`
	FirstID string `json:"first_id"`
	LastID  string `json:"last_id"`
	HasMore bool   `json:"has_more"`
}

// AnthropicModelsPage is the parsed result: the OpenAI-shaped entries plus the
// cursor needed to fetch the next page ("" when there are no more).
type AnthropicModelsPage struct {
	Entries   []map[string]any
	NextAfter string // pass as ?after_id= for the next page; empty when done
}

// TranslateAnthropicModels converts one Anthropic /v1/models page into OpenAI
// model entries, returning the entries and the pagination cursor for the next
// page (empty when has_more is false). Fail-closed on a malformed body.
func TranslateAnthropicModels(body []byte) (AnthropicModelsPage, error) {
	var page anthropicModelsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return AnthropicModelsPage{}, fmt.Errorf("parse anthropic models: %w", err)
	}
	if page.HasMore && page.LastID == "" {
		return AnthropicModelsPage{}, fmt.Errorf("models page has_more requires last_id")
	}
	out := AnthropicModelsPage{}
	for _, m := range page.Data {
		if m.ID == "" {
			continue
		}
		e := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "anthropic",
			"created":  rfc3339ToUnix(m.CreatedAt),
		}
		if m.DisplayName != "" {
			e["display_name"] = m.DisplayName
		}
		if m.MaxInputTokens > 0 {
			e["context_window"] = m.MaxInputTokens
		}
		out.Entries = append(out.Entries, e)
	}
	if page.HasMore && page.LastID != "" {
		out.NextAfter = page.LastID
	}
	return out, nil
}

// rfc3339ToUnix converts an RFC 3339 timestamp to unix seconds (0 on parse
// failure - "created" is cosmetic).
func rfc3339ToUnix(s string) int64 {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// ModelsListJSON renders merged OpenAI model entries as the final
// {"object":"list","data":[...]} body.
func ModelsListJSON(entries []map[string]any) []byte {
	if entries == nil {
		entries = []map[string]any{}
	}
	out, _ := json.Marshal(map[string]any{"object": "list", "data": entries})
	return out
}

// CanonicalModelFields is the fixed vocabulary of model-metadata fields the
// proxy constructs into its served /v1/models list - the single owner shared
// by the enrichment merge (proxy), the config validation docs, and the
// Settings UI's mapping dropdown. Everything is sourced from provider data at
// request time; the proxy never ships a static per-model table.
var CanonicalModelFields = []string{
	"context_length",
	"context_window",
	"max_output_tokens",
	"input_modalities",
	"output_modalities",
}
