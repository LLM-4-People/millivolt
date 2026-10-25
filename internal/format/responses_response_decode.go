package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// DecodeResponsesResponse converts one non-streaming Responses body into the
// canonical response.
func DecodeResponsesResponse(body []byte) (*ir.Response, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("responses response body is not valid UTF-8")
	}
	if err := checkResponsesResponseBounds(body); err != nil {
		return nil, err
	}
	var in responsesWireResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	if err := checkNativeIdentity("id", in.ID); err != nil {
		return nil, err
	}
	if err := checkNativeIdentity("model", in.Model); err != nil {
		return nil, err
	}
	switch in.Status {
	case "completed", "incomplete", "failed", "cancelled":
	case "":
		// A provider error envelope may legitimately omit status; anything
		// else without a terminal status is an unfinished response that must
		// not be rendered as a completed one.
		if typ, _, _ := metrics.ParseErrorEnvelope(body); typ == "" {
			return nil, errors.New("responses response has no terminal status")
		}
	default:
		return nil, fmt.Errorf("responses response is not terminal (status %q)", in.Status)
	}
	resp := &ir.Response{
		ID:          in.ID,
		Model:       in.Model,
		Created:     int64(in.CreatedAt),
		Status:      in.Status,
		Metadata:    in.Metadata.values,
		ServiceTier: in.ServiceTier,
	}
	if in.Usage != nil {
		resp.Usage = responsesUsageToIR(in.Usage)
		if err := validateUsage(resp.Usage); err != nil {
			return nil, err
		}
	}
	if in.Status == "failed" {
		e := &ir.Error{Message: "upstream response failed"}
		if typ, code, msg := metrics.ParseErrorComponents(in.Error); typ != "" || code != "" || msg != "" {
			bounded := boundedProviderError(typ, code, msg)
			e.Type, e.Code = bounded.Type, bounded.Code
			if msg != "" {
				e.Message = bounded.Message
			}
		}
		resp.Error = e
		resp.Finish = ir.FinishError
		return resp, nil
	}
	// An in-band provider error on a nominally completed response is decoded
	// through the shared decoy-aware parser: an empty error object is not a
	// failure, while a string or numeric-code envelope still classifies.
	if typ, code, msg := metrics.ParseErrorEnvelope(body); typ != "" {
		resp.Error = boundedProviderError(typ, code, msg)
		resp.Finish = ir.FinishError
		return resp, nil
	}
	// A completed response with no incomplete reason owns a finished turn. Any
	// other terminal can carry the prefix of a function-call argument value the
	// source never completed; drop only that call and keep the source terminal.
	cleanTerminal := in.Status == "completed" && (in.IncompleteDetails == nil || in.IncompleteDetails.Reason == "")
	outputPhaseSeen := false
	for i, item := range in.Output {
		if item.Phase != "" {
			outputPhaseSeen = true
		}
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch {
		case typ == "message":
			blocks, warns, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
			if err != nil {
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, blocks...)
			resp.Warnings = append(resp.Warnings, warns...)
		case typ == "function_call":
			call, warns, err := decodeResponsesFunctionCall(item, !cleanTerminal, nil)
			if err != nil {
				if !cleanTerminal && isToolArgumentsError(err) {
					resp.Warnings.AddOnce(ir.WarningDropped, "function_call.arguments",
						"the source stopped before the function call arguments were complete")
					continue
				}
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			resp.Warnings = append(resp.Warnings, warns...)
		case typ == "reasoning":
			if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
				return nil, err
			}
			var summary []string
			for _, s := range item.Summary {
				if s.Text != "" {
					summary = append(summary, s.Text)
				}
			}
			var reasoningText strings.Builder
			if len(item.Content) > 0 {
				blocks, warns, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
				if err != nil {
					return nil, fmt.Errorf("responses output item %d: %w", i, err)
				}
				resp.Warnings = append(resp.Warnings, warns...)
				for _, block := range blocks {
					if block.Type == ir.BlockText {
						reasoningText.WriteString(block.Text)
					}
				}
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
				ID: item.ID, Text: reasoningText.String(), Summary: summary,
				Encrypted: item.EncryptedContent, Provider: ir.Responses,
			}})
		case sse.ResponsesToolCallItem(typ):
			// A provider-executed tool item (custom, local shell, MCP,
			// computer, ...) has no cross-protocol block. It is still tool
			// activity: count it so the finish is tool_calls and the record
			// does not bill its argument output as answer generation. The
			// translated wire keeps the existing dropped-item warning.
			resp.ProviderToolCalls++
			resp.Warnings.Add(ir.WarningDropped, "output."+typ, "provider-specific item has no cross-protocol equivalent")
		default:
			resp.Warnings.Add(ir.WarningDropped, "output."+typ, "provider-specific item has no cross-protocol equivalent")
		}
	}
	if outputPhaseSeen {
		// One warning per response: phase is a per-item hint, and repeating
		// it for every item would amplify the warning header.
		resp.Warnings.Add(ir.WarningDropped, "output.phase", "output item phase has no cross-protocol equivalent")
	}
	if in.IncompleteDetails != nil && in.IncompleteDetails.Reason != "" {
		resp.IncompleteReason = in.IncompleteDetails.Reason
		switch in.IncompleteDetails.Reason {
		case "max_output_tokens":
			resp.Finish = ir.FinishLength
		case "content_filter":
			resp.Finish = ir.FinishContentFilter
		default:
			// max_tool_calls, steered and provider-specific reasons are not a
			// clean stop and have no exact cross-protocol equivalent.
			resp.Finish = ir.FinishOther
		}
	} else if in.Status == "incomplete" {
		resp.Finish = ir.FinishOther
	} else if in.Status == "cancelled" {
		resp.Finish = ir.FinishCancelled
	} else if hasToolCall(resp.Blocks) || resp.ProviderToolCalls > 0 {
		resp.Finish = ir.FinishToolCalls
	} else {
		resp.Finish = ir.FinishStop
	}
	return resp, nil
}

func hasToolCall(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockToolCall {
			return true
		}
	}
	return false
}

func responsesUsageToIR(in *responsesWireUsage) ir.Usage {
	u := ir.Usage{
		InputTokens:  int64(in.InputTokens),
		OutputTokens: int64(in.OutputTokens),
		TotalTokens:  int64(in.TotalTokens),
	}
	if in.InputTokensDetails != nil {
		u.CacheReadTokens = int64(in.InputTokensDetails.CachedTokens)
		u.CacheWriteTokens = int64(in.InputTokensDetails.CacheWriteTokens)
	}
	if in.OutputTokensDetails != nil {
		u.ReasoningTokens = int64(in.OutputTokensDetails.ReasoningTokens)
	}
	return u
}

// DecodeResponsesError decodes the standard OpenAI error envelope used by the
// Responses HTTP surface. The shared decoy-aware parser owns string error
// values and numeric codes, so a provider envelope this proxy can record is
// never rejected by a narrower local shape.
func DecodeResponsesError(body []byte) (*ir.Error, error) {
	if typ, code, msg := metrics.ParseErrorEnvelope(body); typ != "" {
		return boundedProviderError(typ, code, msg), nil
	}
	return nil, errors.New("not an OpenAI error envelope")
}
