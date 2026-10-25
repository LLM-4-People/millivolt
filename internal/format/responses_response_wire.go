package format

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type responsesWireResponse struct {
	ID        string              `json:"id"`
	Object    string              `json:"object"`
	CreatedAt jsonnum.UnixSeconds `json:"created_at"`
	Status    string              `json:"status"`
	// Error is raw because the shared envelope parser deliberately accepts a
	// string value and a numeric code; a typed object would reject those
	// shapes before the proxy can record the provider's failure.
	Error             json.RawMessage `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Instructions       json.RawMessage     `json:"instructions"`
	Model              string              `json:"model"`
	Metadata           boundedStringMap    `json:"metadata"`
	ServiceTier        string              `json:"service_tier"`
	Output             []responsesWireItem `json:"output"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	PreviousResponseID *string             `json:"previous_response_id"`
	Reasoning          json.RawMessage     `json:"reasoning"`
	Store              *bool               `json:"store"`
	Temperature        *float64            `json:"temperature"`
	Text               json.RawMessage     `json:"text"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	TopP               *float64            `json:"top_p"`
	Truncation         string              `json:"truncation"`
	Usage              *responsesWireUsage `json:"usage"`
}

type responsesWireUsage struct {
	InputTokens        jsonnum.Int64 `json:"input_tokens"`
	OutputTokens       jsonnum.Int64 `json:"output_tokens"`
	TotalTokens        jsonnum.Int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens     jsonnum.Int64 `json:"cached_tokens"`
		CacheWriteTokens jsonnum.Int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens jsonnum.Int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// checkResponsesResponseBounds preflights the array shapes a Responses
// response materializes before the typed decode. It is shared by the
// non-streaming decoder and the streaming created/terminal branches so one
// large document cannot amplify into a large typed allocation in either path.
// The probe only retains raw members; it stops at the first over-bound array.
func checkResponsesResponseBounds(body []byte) error {
	var probe struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("parse responses response: %w", err)
	}
	if !metrics.ContentPartsWithinLimit(probe.Output) {
		return fmt.Errorf("responses output exceeds %d items", metrics.ContentPartMax)
	}
	// A reasoning output item's summary array is materialized by the typed
	// item decode; the bounded output array keeps this probe slice small. An
	// absent or null output is a provider error envelope (or an empty
	// response) and has no items to inspect.
	if len(probe.Output) > 0 && !bytes.Equal(bytes.TrimSpace(probe.Output), []byte("null")) {
		var itemBounds []struct {
			Summary json.RawMessage `json:"summary"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(probe.Output, &itemBounds); err != nil {
			return fmt.Errorf("parse responses response: %w", err)
		}
		summaryTotal := 0
		var nested responsesNestedBudget
		for i, item := range itemBounds {
			if err := checkWireArray(item.Summary, "output.summary", metrics.ContentPartMax); err != nil {
				return err
			}
			if err := addWireArray(&summaryTotal, item.Summary, "output.summary", WireNestedCollectionMax); err != nil {
				return err
			}
			if err := checkResponsesContent(&nested, item.Content, fmt.Sprintf("output[%d].content", i)); err != nil {
				return err
			}
		}
	}
	return nil
}
