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
	"errors"
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/sse"
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

// NewModelEntry builds the canonical OpenAI model-entry base: id,
// object:"model", created, owned_by. Both format translators (Anthropic
// models, Cursor GetUsableModels) start from it and add their
// provider-specific extras; the proxy's list normalization reuses the same
// base as its fill-if-absent defaults (existing upstream fields always win).
func NewModelEntry(id, ownedBy string, created int64) map[string]any {
	return map[string]any{
		"id":       id,
		"object":   "model",
		"created":  created,
		"owned_by": ownedBy,
	}
}

// errParseOpenAIBody wraps a failed OpenAI request-body decode: the single
// wording owner shared by the Anthropic and Cursor request translators.
func errParseOpenAIBody(err error) error {
	return fmt.Errorf("parse openai body: %w", err)
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
		e := NewModelEntry(m.ID, "anthropic", rfc3339ToUnix(m.CreatedAt))
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

// ErrModelsListTooLarge reports that rendering a model list would exceed the
// caller's byte bound. ModelsListJSON fails closed, returning no partial
// document.
var ErrModelsListTooLarge = errors.New("model list output exceeds byte limit")

// ModelsListJSON renders merged OpenAI model entries as the final
// {"data":[...],"object":"list"} body. Entries are encoded one at a time with
// the package's wire encoder (SetEscapeHTML(false)), so provider content such
// as <, > and & keeps its literal one-byte JSON form instead of expanding up to
// six-fold, and the output never exceeds limit bytes (limit <= 0 is unbounded).
// A document past the limit fails before anything is returned or written; the
// caller owns the bound and surfaces the error. Normal output is byte-identical
// to encoding the same envelope and entries with encoding/json's sorted keys.
func ModelsListJSON(entries []map[string]any, limit int) ([]byte, error) {
	if entries == nil {
		entries = []map[string]any{}
	}
	const (
		open   = `{"data":[`
		suffix = `],"object":"list"}`
	)
	out := []byte(open)
	for i, e := range entries {
		if i > 0 {
			out = append(out, ',')
		}
		item, err := sse.MarshalWireJSON(e)
		if err != nil {
			return nil, err
		}
		if limit > 0 && len(out)+len(item)+len(suffix) > limit {
			return nil, fmt.Errorf("%w (%d bytes)", ErrModelsListTooLarge, limit)
		}
		out = append(out, item...)
	}
	if limit > 0 && len(out)+len(suffix) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", ErrModelsListTooLarge, limit)
	}
	return append(out, suffix...), nil
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
