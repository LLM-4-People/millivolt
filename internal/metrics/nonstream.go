package metrics

import (
	"bytes"
	"encoding/json"
	"strings"
)

// nonStreamChoicesMax bounds the top-level choices array before the typed
// envelope decode materializes it. A non-streaming completion needs one choice
// for the client and a handful for n-best or multi-candidate providers; a body
// carrying tens of thousands of tiny choice objects would otherwise expand a
// bounded 1 MiB spool into tens of MiB of typed structs for every concurrent
// spool. Internal safety guardrail, not a wire-format or user-tunable limit.
const nonStreamChoicesMax = 1024

// nonStreamChoicesErrorType classifies an over-bound choices array as a
// response-level error. The class is deliberately not a retryable provider
// availability spelling: the same response would violate the bound again, so
// the relay commits the provider's bytes verbatim and records the error.
const nonStreamChoicesErrorType = "response_too_large"

const nonStreamChoicesErrorMessage = "upstream response choices exceed the non-stream analysis bound"

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
	FinishReason string          `json:"finish_reason"`
	Text         json.RawMessage `json:"text"`
	Message      struct {
		Role          json.RawMessage   `json:"role"`
		Content       json.RawMessage   `json:"content"`
		Refusal       json.RawMessage   `json:"refusal"`
		FunctionCall  json.RawMessage   `json:"function_call"`
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
// overrides), model/id, finish reason, answer presence (message content and
// legacy choices[].text content, plus answer-shaped refusal or deprecated
// function_call declarations), reasoning presence (contentPresent semantics),
// tool calls, bounded preview, cost (the existing ExtractCost key gate) and
// the canonical top-level error envelope. It is the single analysis owner for
// both the buffered quality path and the teeing relay.
//
// The top-level choices array is lexically bounded before the typed decode: an
// over-bound body is classified as an error without materializing the choices
// and without attempting the cost decode, which would otherwise expand the
// same array generically. A body without a decodable top-level choices array
// keeps the historical malformed-body behavior (the typed decode owns it).
func AnalyzeNonStreamBody(body []byte, usageKeys map[string]string, costKeys []string, preview bool) NonStreamAnalysis {
	var a NonStreamAnalysis
	if nonStreamChoicesOverLimit(body) {
		// Outcome deliberately stays empty: this is a response-level analysis
		// error, not one of the degenerate completion classes that the quality
		// retry budgets own.
		a.ErrorType = nonStreamChoicesErrorType
		a.ErrorCode = nonStreamChoicesErrorType
		a.ErrorMsg = nonStreamChoicesErrorMessage
		return a
	}
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
	// choice alone owns the classified content. Legacy /v1/completions puts
	// the answer in choices[].text, and the streaming analyzer counts the
	// first non-empty choice text across the list as answer content, so keep
	// the same cross-choice rule here.
	hadChoiceText := false
	var choiceText json.RawMessage
	for i := range raw.Choices {
		if _, ok := optionalJSONString(raw.Choices[i].Message.Role); !ok {
			a.RecordFieldsOK = false
		}
		if !hadChoiceText && nonEmptyAnswerValue(raw.Choices[i].Text) {
			hadChoiceText = true
			choiceText = raw.Choices[i].Text
		}
	}
	if len(raw.Choices) == 0 {
		return
	}
	ch := raw.Choices[0]
	// A non-empty refusal or deprecated function_call declaration is
	// answer-shaped output (the streaming analyzer's answerishSeen): visible
	// provider bytes that must never be re-run, even though they are not
	// content for HadAnswerContent/FirstAnswerAt accounting.
	hadAnswerish := nonEmptyAnswerValue(ch.Message.Refusal) ||
		nonEmptyAnswerValue(ch.Message.FunctionCall)
	hadAnswer := contentPresent(ch.Message.Content) || hadChoiceText || hadAnswerish
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
	} else if hadChoiceText {
		// Content wins the preview when both shapes appear; a completions
		// body carries only text. Non-string text stays answer-shaped for the
		// outcome but contributes no preview.
		if text, ok := FlattenContent(choiceText); ok && text != "" {
			a.HadAnswerContent = true
			if preview {
				a.Preview = TruncatePreview(text)
			}
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

// nonEmptyAnswerValue applies the streaming analyzer's answer-shaped value
// classification to the buffered body's refusal and deprecated function_call
// fields: a non-empty string, non-empty array/object, or scalar other than
// null/false counts. An empty string, empty object, empty array, null and
// false are decoys, so a refusal-only or function_call-only response is
// answer-shaped while its empty spelling stays a degenerate completion.
func nonEmptyAnswerValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"':
		var s string
		return json.Unmarshal(t, &s) == nil && s != ""
	case '[', '{':
		i := SkipSpace(t, 1)
		return i < len(t) && t[i] != ']' && t[i] != '}'
	case 'n', 'f':
		return false
	}
	return true
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

// nonStreamChoicesOverLimit reports whether any top-level choices member of
// the body is an array with more than nonStreamChoicesMax elements. The scan
// is lexical and stops at the first proof of overflow, so a hostile array
// costs only the prefix needed to reach the bound and no typed allocation.
// Nested choices keys, string contents, and escaped key spellings are handled
// by the same JSON traversal rules as the typed decode. A body without a
// top-level choices array (including malformed input) reports false and leaves
// shape validation to json.Unmarshal, preserving the historical malformed-body
// behavior.
//
// Every top-level occurrence is checked rather than only the first: duplicate
// object keys are ambiguous, and a second occurrence that the standard decoder
// would honor must not bypass the bound. Any over-bound occurrence fails the
// whole body closed.
func nonStreamChoicesOverLimit(body []byte) bool {
	// Keep responses without a choices member (the Responses API shape and
	// other passthrough documents) on the vectorized negative path. The typed
	// decoder matches the struct field case-insensitively and decodes Unicode
	// escapes to the same key, so every spelling it honors must pass this gate:
	// compare the quoted token case-insensitively and fall through to the full
	// traversal for any body carrying an escape.
	if !containsChoicesKeyToken(body) {
		return false
	}
	i := SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return false
	}
	i = SkipSpace(body, i+1)
	for i < len(body) {
		if body[i] == '}' {
			return false
		}
		if body[i] != '"' {
			return false // a top-level member must open with a key string
		}
		keyStart := i
		keyEnd := SkipJSONString(body, i)
		if keyEnd < 0 {
			return false
		}
		i = SkipSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return false
		}
		i = SkipSpace(body, i+1)
		if i >= len(body) {
			return false
		}
		if nonStreamChoicesKey(body[keyStart:keyEnd]) {
			if body[i] == '[' {
				if n, _, ok := CountJSONArrayBytes(body, i, nonStreamChoicesMax); ok && n > nonStreamChoicesMax {
					return true
				}
			}
			// A non-array value is the typed decode's malformed shape; a
			// later duplicate choices member must still be checked.
		}
		next := SkipJSONValue(body, i)
		if next < 0 {
			return false
		}
		i = SkipSpace(body, next)
		if i >= len(body) || body[i] != ',' {
			return false // the object ended or the scan is malformed
		}
		i = SkipSpace(body, i+1)
	}
	return false
}

// containsChoicesKeyToken reports whether body can contain a top-level choices
// key under the typed decoder's matching rules: ASCII case variants are matched
// without allocating, and any Unicode escape forces the full traversal because
// a key letter may be spelled as \uXXXX. Nested keys and string contents still
// fail the real traversal below.
func containsChoicesKeyToken(body []byte) bool {
	const token = `choices`
	// The overwhelmingly common exact spelling keeps the vectorized whole-body
	// search instead of paying the quote-by-quote scan below. Any Unicode escape
	// may spell a key letter, so it also forces the full traversal.
	if bytes.Contains(body, []byte(`"choices"`)) || bytes.Contains(body, []byte(`\u`)) {
		return true
	}
	for offset := 0; offset < len(body); {
		quote := bytes.IndexByte(body[offset:], '"')
		if quote < 0 {
			return false
		}
		quote += offset
		closing := quote + len(token) + 1
		if closing < len(body) && choicesKeyFold(body[quote+1:quote+len(token)+1]) && body[closing] == '"' {
			return true
		}
		offset = quote + 1
	}
	return false
}

// choicesKeyFold is the allocation-free ASCII case fold used by the mixed-case
// key scan. JSON keys are UTF-8; a non-ASCII byte cannot equal an ASCII letter,
// so the fold is exact for this token.
func choicesKeyFold(candidate []byte) bool {
	const token = "choices"
	if len(candidate) != len(token) {
		return false
	}
	for i := range candidate {
		c := candidate[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != token[i] {
			return false
		}
	}
	return true
}

// nonStreamChoicesKey compares one quoted top-level key token with "choices".
// The ordinary spelling takes the allocation-free byte path; escaped or
// non-ASCII spellings decode through the standard JSON string rules.
func nonStreamChoicesKey(raw []byte) bool {
	if len(raw) == len(`"choices"`) && bytes.Equal(raw, []byte(`"choices"`)) {
		return true
	}
	var name string
	// encoding/json matches a struct field case-insensitively when no exact
	// tag match exists, so the typed decode honors "Choices"/"CHOICES" too.
	return json.Unmarshal(raw, &name) == nil && strings.EqualFold(name, "choices")
}
