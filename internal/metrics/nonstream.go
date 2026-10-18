package metrics

import (
	"bytes"
	"encoding/json"
)

// NonStreamAnalysis is one decoded view of a fully buffered non-streaming
// OpenAI-shaped response body. The relay's quality gate and its record commit
// both read this value, so a spooled body is decoded once instead of once for
// classification and again for usage/model/finish/tool/preview extraction.
//
// Outcome is the ClassifyOutcome result for the body. RecordFieldsOK reports
// whether the record-facing fields decoded with the same success as the
// historical typed decode: when a non-classification field (usage, model, id,
// message role, tool-call detail) has the wrong JSON type, classification
// still proceeds but no record fields are applied, exactly like the legacy
// all-or-nothing extraction. Usage/Model/ID are derived from the same decode; Cost and the
// top-level error envelope keep the existing ExtractCost and
// ParseErrorEnvelope semantics.
type NonStreamAnalysis struct {
	Outcome          string
	Usage            Usage
	HasUsage         bool
	Model            string
	ID               string
	FinishReason     string
	HadAnswerContent bool
	ToolCalls        int
	ToolNames        []string
	Preview          string
	Cost             float64
	HasCost          bool
	ErrorType        string
	ErrorCode        string
	ErrorMsg         string
	RecordFieldsOK   bool
}

// nonStreamEnvelope is the single typed decode of a buffered body. Fields the
// classifier ignores (usage/model/id, message role and tool-call detail) are
// kept raw so a malformed value cannot change the classification the legacy
// classifier produced; they are decoded afterwards into the record-facing
// view. Role stays raw because the legacy typed extractor's `Role string`
// failed the whole decode on a non-string value while the raw classifier did
// not care about it.
type nonStreamEnvelope struct {
	Usage   json.RawMessage   `json:"usage"`
	Model   json.RawMessage   `json:"model"`
	ID      json.RawMessage   `json:"id"`
	Choices []nonStreamChoice `json:"choices"`
	Error   json.RawMessage   `json:"error"`
}

type nonStreamChoice struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Role          json.RawMessage   `json:"role"`
		Content       json.RawMessage   `json:"content"`
		Reasoning     json.RawMessage   `json:"reasoning_content"`
		ReasoningBare json.RawMessage   `json:"reasoning"`
		ReasoningDet  json.RawMessage   `json:"reasoning_details"`
		ReasoningText json.RawMessage   `json:"reasoning_text"`
		ToolCalls     []json.RawMessage `json:"tool_calls"`
	} `json:"message"`
}

type nonStreamToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// AnalyzeNonStreamBody decodes body once and derives the complete non-stream
// metrics view: the quality outcome, usage (ParseUsage with provider
// overrides), model/id, finish reason, answer presence (FlattenContent
// semantics), reasoning presence (contentPresent semantics), tool calls,
// bounded preview, cost (the existing ExtractCost key gate) and the canonical
// top-level error envelope. It is the single analysis owner for both the
// buffered quality path and the teeing relay.
func AnalyzeNonStreamBody(body []byte, usageKeys map[string]string, costKeys []string, preview bool) NonStreamAnalysis {
	var a NonStreamAnalysis
	var raw nonStreamEnvelope
	decodeErr := json.Unmarshal(body, &raw)
	if decodeErr == nil {
		a.RecordFieldsOK = true
		a.analyzeRecordFields(raw, usageKeys, preview)
	}
	// Cost is independent of the typed record extraction: the existing
	// extractor owns the cost-key gate and the USD/ticks rules, and a valid
	// body with a malformed unrelated field still reported cost historically.
	if cost, ok := ExtractCost(body, costKeys); ok {
		a.Cost, a.HasCost = cost, true
	}
	// The error envelope is authoritative only through the top-level key. The
	// typed decode already captured that raw value; a body whose typed decode
	// failed (for example a non-object choices member) still gets the same
	// independent envelope attempt the legacy extraction made.
	if decodeErr == nil {
		a.ErrorType, a.ErrorCode, a.ErrorMsg = parseErrorValue(raw.Error)
	} else {
		a.ErrorType, a.ErrorCode, a.ErrorMsg = ParseErrorEnvelope(body)
	}
	return a
}

// analyzeRecordFields derives the record-facing fields from the one typed
// decode. A malformed usage/model/id/role/tool-call detail clears
// RecordFieldsOK while leaving Outcome intact: the legacy classifier ignored
// those fields but the legacy extractor abandoned every assignment on a decode
// error.
func (a *NonStreamAnalysis) analyzeRecordFields(raw nonStreamEnvelope, usageKeys map[string]string, preview bool) {
	if rawValuePresent(raw.Usage) {
		var usage map[string]any
		if json.Unmarshal(raw.Usage, &usage) != nil {
			a.RecordFieldsOK = false
		} else if usage != nil {
			a.Usage = ParseUsage(usage, usageKeys)
			a.HasUsage = true
		}
	}
	if model, ok := optionalJSONString(raw.Model); !ok {
		a.RecordFieldsOK = false
	} else {
		a.Model = model
	}
	if id, ok := optionalJSONString(raw.ID); !ok {
		a.RecordFieldsOK = false
	} else {
		a.ID = id
	}
	// The legacy typed extractor decoded every choice, so a non-string role in
	// any choice cleared its record fields. Validate all of them; the first
	// choice alone owns the classified content.
	for i := range raw.Choices {
		if _, ok := optionalJSONString(raw.Choices[i].Message.Role); !ok {
			a.RecordFieldsOK = false
		}
	}
	if len(raw.Choices) == 0 {
		return
	}
	ch := raw.Choices[0]
	hadAnswer := contentPresent(ch.Message.Content)
	hadReasoning := contentPresent(ch.Message.Reasoning) ||
		contentPresent(ch.Message.ReasoningBare) ||
		contentPresent(ch.Message.ReasoningDet) ||
		contentPresent(ch.Message.ReasoningText)
	toolCalls := len(ch.Message.ToolCalls)
	a.Outcome = ClassifyOutcome(ch.FinishReason, true, hadAnswer, hadReasoning, toolCalls, true)
	a.FinishReason = ch.FinishReason
	if text, ok := FlattenContent(ch.Message.Content); ok && text != "" {
		a.HadAnswerContent = true
		if preview {
			a.Preview = TruncatePreview(text)
		}
	}
	if toolCalls > 0 {
		a.ToolCalls = toolCalls
		names := make([]string, 0, toolCalls)
		for _, element := range ch.Message.ToolCalls {
			if !rawValuePresent(element) {
				// A JSON null member decodes as a zero tool call in the
				// historical typed slice; keep counting it, contribute no name.
				continue
			}
			var tc nonStreamToolCall
			if json.Unmarshal(element, &tc) != nil {
				a.RecordFieldsOK = false
				continue
			}
			if tc.Function.Name != "" {
				names = append(names, tc.Function.Name)
			}
		}
		a.ToolNames = names
	}
}

// rawValuePresent reports whether a raw field carries a non-null JSON value.
// Absent fields, null, and whitespace-only values are all "not present".
func rawValuePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// optionalJSONString decodes an optional string field. Absent/null is the
// empty string; a non-string JSON value is a decode failure (the historical
// typed extraction abandoned all assignments on it).
func optionalJSONString(raw json.RawMessage) (string, bool) {
	if !rawValuePresent(raw) {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
