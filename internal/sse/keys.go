package sse

import (
	"bytes"
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// frameKeys is the analyzer's single key probe: one escape-aware pass over a
// payload records the value evidence the classifier reads - first occurrence
// for most keys, any non-empty occurrence for the reasoning/refusal/
// function_call families, and the last top-level string type. Values alias the
// payload; nothing is copied here. Callers must use one zero-valued frameKeys
// per payload (Feed creates one per frame), so a result is never carried across
// frames.
type frameKeys struct {
	content      []byte
	delta        []byte
	toolCalls    []byte
	finishReason []byte
	usage        []byte
	model        []byte
	requestID    []byte
	choices      []byte
	// text is the first non-empty choices[].text answer string of a legacy
	// /v1/completions prompt stream (no delta wrapper). choicesDepth scopes
	// the capture to the choice object itself, so nested "text" keys (a
	// reasoning part, a Responses content part) never become answer tokens.
	text         []byte
	choicesDepth int
	thought      []byte
	// eventType is the LAST top-level string "type" value. encoding/json
	// resolves duplicate keys last-wins but treats a null value as a no-op and
	// reports non-string values as errors; the candidate decode below preserves
	// those error semantics, so only a string occurrence replaces the value.
	eventType []byte

	hasError bool

	deltaNonEmpty            bool
	refusalNonEmpty          bool
	functionCallNonEmpty     bool
	reasoningContentNonEmpty bool
	reasoningDetailsNonEmpty bool
	reasoningTextNonEmpty    bool
	reasoningNonEmpty        bool
	thinkingNonEmpty         bool
	thinkingDeltaNonEmpty    bool
	thinkingBlocksNonEmpty   bool
}

// scan walks payload once. Every JSON string is classified as a key when the
// next non-space byte is ':', matching the lexical JSONKey locator's
// any-nesting behavior: string contents are skipped, key escapes are decoded
// only when the spelling contains a backslash, and optional whitespace around
// the colon is tolerated. Keys are matched by exact decoded spelling, so
// "reasoning" never matches "reasoning_content" and "reasoning_effort" is not
// a reasoning token; only the Responses event discriminator adds encoding/json's
// case-insensitive "type" spellings at depth 1.
func (k *frameKeys) scan(payload []byte) {
	depth := 0
	for i := 0; i < len(payload); {
		switch payload[i] {
		case '"':
			start := i
			i++
			escaped := false
			for i < len(payload) && payload[i] != '"' {
				if payload[i] == '\\' {
					escaped = true
					i++
				}
				i++
			}
			if i >= len(payload) {
				return
			}
			end := i
			i++
			j := metrics.SkipSpace(payload, i)
			if j >= len(payload) || payload[j] != ':' {
				continue
			}
			j++
			j = metrics.SkipSpace(payload, j)
			if j >= len(payload) {
				return
			}
			value := payload[j:]
			top := depth == 1
			key := payload[start+1 : end]
			if escaped {
				var buf [32]byte
				decoded, ok := decodeJSONKey(payload[start:end+1], buf[:])
				if !ok {
					i = j
					continue
				}
				key = decoded
			}
			id := identifyFrameKey(key)
			// encoding/json matches a struct tag case-insensitively, so the
			// Responses discriminator also accepts "Type"/"TYPE" spellings at
			// the top level. Every other analyzer key stays exact-spelling.
			if id == keyNone && top && typeKeyFold(key) {
				id = keyType
			}
			k.observe(id, value, top, depth)
			// Continue at the value so nested keys and later siblings are
			// still seen; a plain string value is skipped by the quote case.
			i = j
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			if k.choicesDepth > 0 && depth < k.choicesDepth {
				k.choicesDepth = 0
			}
			i++
		default:
			i++
		}
	}
}

// frameKey enumerates the classifier's target keys. The switch on
// string(key) is the single spelling owner for raw and decoded escaped keys;
// Go's compiler recognizes the conversion inside a string switch and does not
// allocate it.
type frameKey uint8

const (
	keyNone frameKey = iota
	keyID
	keyType
	keyDeltaValue
	keyUsage
	keyModel
	keyError
	keyContent
	keyText
	keyThought
	keyRefusal
	keyChoices
	keyThinking
	keyReasoning
	keyToolCalls
	keyFinishReason
	keyFunctionCall
	keyReasoningText
	keyThinkingDelta
	keyThinkingBlocks
	keyReasoningContent
	keyReasoningDetails
)

func identifyFrameKey(key []byte) frameKey {
	switch string(key) {
	case "id":
		return keyID
	case "type":
		return keyType
	case "delta":
		return keyDeltaValue
	case "usage":
		return keyUsage
	case "model":
		return keyModel
	case "error":
		return keyError
	case "content":
		return keyContent
	case "text":
		return keyText
	case "thought":
		return keyThought
	case "refusal":
		return keyRefusal
	case "choices":
		return keyChoices
	case "thinking":
		return keyThinking
	case "reasoning":
		return keyReasoning
	case "tool_calls":
		return keyToolCalls
	case "finish_reason":
		return keyFinishReason
	case "function_call":
		return keyFunctionCall
	case "reasoning_text":
		return keyReasoningText
	case "thinking_delta":
		return keyThinkingDelta
	case "thinking_blocks":
		return keyThinkingBlocks
	case "reasoning_content":
		return keyReasoningContent
	case "reasoning_details":
		return keyReasoningDetails
	}
	return keyNone
}

// decodeJSONKey decodes a quoted JSON key token into buf. It is used only when
// the raw spelling contains a backslash; every target key is an ASCII
// identifier of at most 17 bytes, so a decoded non-ASCII or over-long key can
// never match and reports false instead of allocating. Malformed escapes also
// report false, matching the old decode's no-match behavior.
func decodeJSONKey(raw, buf []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return nil, false
	}
	out := buf[:0]
	for i := 1; i < len(raw)-1; i++ {
		c := raw[i]
		if c != '\\' {
			if c == '"' || c >= 0x80 || len(out) >= len(buf) {
				return nil, false
			}
			out = append(out, c)
			continue
		}
		i++
		if i >= len(raw)-1 {
			return nil, false
		}
		switch raw[i] {
		case '"', '\\':
			if len(out) >= len(buf) {
				return nil, false
			}
			out = append(out, raw[i])
		case 'b', 'f', 'n', 'r', 't', '/':
			// Decoded control/slash characters never appear in a target key.
			return nil, false
		case 'u':
			if i+4 >= len(raw)-1 {
				return nil, false
			}
			v := hex4(raw[i+1 : i+5])
			if v < 0 || v > 0x7f || len(out) >= len(buf) {
				return nil, false
			}
			out = append(out, byte(v))
			i += 4
		default:
			return nil, false
		}
	}
	return out, true
}

func hex4(b []byte) int {
	v := 0
	for _, c := range b {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return -1
		}
		v = v*16 + d
	}
	return v
}

// typeKeyFold matches the ASCII case-insensitive "type" spellings that
// encoding/json accepts for the struct tag. Non-ASCII foldings never reach a
// protocol's Responses discriminator.
func typeKeyFold(key []byte) bool {
	if len(key) != 4 {
		return false
	}
	const want = "type"
	for i := 0; i < len(want); i++ {
		c := key[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// observe applies one matched key occurrence at its containing-object depth.
// Single-value fields keep the first occurrence (the same value metrics.JSONKey
// returns); eventType keeps the last top-level occurrence because encoding/json
// resolves duplicate top-level keys last-wins. Reasoning/refusal/function_call
// presence is the union over occurrences: any occurrence with a non-empty value
// qualifies. choices[].text is captured only at the choice object's own depth.
func (k *frameKeys) observe(id frameKey, value []byte, top bool, depth int) {
	switch id {
	case keyID:
		if k.requestID == nil {
			k.requestID = value
		}
	case keyType:
		if top && len(value) > 0 && value[0] == '"' {
			k.eventType = value
		}
	case keyDeltaValue:
		if !k.deltaNonEmpty {
			k.deltaNonEmpty = nonEmptyJSONValue(value)
		}
		if k.delta == nil {
			k.delta = value
		}
	case keyUsage:
		if k.usage == nil {
			k.usage = value
		}
	case keyModel:
		if k.model == nil {
			k.model = value
		}
	case keyError:
		k.hasError = true
	case keyContent:
		if k.content == nil {
			k.content = value
		}
	case keyText:
		// Legacy prompt-completion answer: the choice object itself carries
		// "text". choicesDepth (set by the enclosing top-level "choices"
		// array) is the array's inner depth, so the direct member object sits
		// one level deeper. Anything nested further is not answer text. Keep
		// the first NON-EMPTY choice text: a multi-choice chunk may lead with
		// an empty choice while another choice carries the answer.
		if k.text == nil && k.choicesDepth > 0 && depth == k.choicesDepth+1 &&
			startsNonEmptyJSONString(value) {
			k.text = value
		}
	case keyThought:
		if k.thought == nil {
			k.thought = value
		}
	case keyRefusal:
		if !k.refusalNonEmpty {
			k.refusalNonEmpty = nonEmptyJSONValue(value)
		}
	case keyChoices:
		if k.choices == nil {
			k.choices = value
		}
		// A top-level choices array opens the only scope where a direct
		// member's "text" is a legacy prompt-completion answer.
		if top && len(value) > 0 && value[0] == '[' {
			k.choicesDepth = depth + 1
		}
	case keyThinking:
		if !k.thinkingNonEmpty {
			k.thinkingNonEmpty = nonEmptyJSONValue(value)
		}
	case keyReasoning:
		if !k.reasoningNonEmpty {
			k.reasoningNonEmpty = nonEmptyJSONValue(value)
		}
	case keyToolCalls:
		if k.toolCalls == nil {
			k.toolCalls = value
		}
	case keyFinishReason:
		if k.finishReason == nil {
			k.finishReason = value
		}
	case keyFunctionCall:
		if !k.functionCallNonEmpty {
			k.functionCallNonEmpty = nonEmptyJSONValue(value)
		}
	case keyReasoningText:
		if !k.reasoningTextNonEmpty {
			k.reasoningTextNonEmpty = nonEmptyJSONValue(value)
		}
	case keyThinkingDelta:
		if !k.thinkingDeltaNonEmpty {
			k.thinkingDeltaNonEmpty = nonEmptyJSONValue(value)
		}
	case keyThinkingBlocks:
		if !k.thinkingBlocksNonEmpty {
			k.thinkingBlocksNonEmpty = nonEmptyJSONValue(value)
		}
	case keyReasoningContent:
		if !k.reasoningContentNonEmpty {
			k.reasoningContentNonEmpty = nonEmptyJSONValue(value)
		}
	case keyReasoningDetails:
		if !k.reasoningDetailsNonEmpty {
			k.reasoningDetailsNonEmpty = nonEmptyJSONValue(value)
		}
	}
}

// startsNonEmptyJSONString reports whether value is a JSON string whose
// contents are not empty. No decode is needed for presence.
func startsNonEmptyJSONString(value []byte) bool {
	return len(value) > 1 && value[0] == '"' && value[1] != '"'
}

// contentStart reports whether the first content value is a non-empty string
// (not ""). The analyzer's answer-token gate deliberately reads only the first
// occurrence, so a leading empty/decoy content value is not overridden by a
// later one.
func (k *frameKeys) contentStart() bool {
	return startsNonEmptyJSONString(k.content)
}

// textStart reports whether the probe captured a non-empty legacy
// /v1/completions choices[].text answer string. Scoping to the choice object
// is what keeps a nested "text" key (a reasoning part or a typed content part)
// from being counted as answer content.
func (k *frameKeys) textStart() bool {
	return k.text != nil
}

// reasoningStart reports whether the payload carries reasoning ("thinking")
// content. Providers surface reasoning under many field names, so we match a
// family of key prefixes rather than a single hardcoded one (provider-agnostic,
// per the project rule). These are NOT answer tokens.
//
// Matched (non-empty value required):
//   - "reasoning_content"  DeepSeek R1, Qwen QwQ/Qwen3, Z.AI/GLM, Kimi, xAI legacy, LiteLLM, older vLLM
//   - "reasoning"          OpenRouter normalized, current vLLM (renamed from reasoning_content)
//   - "reasoning_details"  OpenRouter structured array, MiniMax
//   - "reasoning_text"     OpenAI/xAI Responses API (response.reasoning_text.delta)
//   - "thinking" / "thinking_delta" / "thinking_blocks"  Anthropic + LiteLLM passthrough
//   - "thought":true       Gemini generateContent part marker
//
// Deliberately NOT matched: "reasoning_effort" (a request-config echo, not a
// token) and null/empty values (Qwen/DeepSeek send "reasoning_content":"" on
// the initial role chunk - that must not mark TTFT).
func (k *frameKeys) reasoningStart() bool {
	return k.reasoningContentNonEmpty ||
		k.reasoningDetailsNonEmpty ||
		k.reasoningTextNonEmpty ||
		k.reasoningNonEmpty ||
		k.thinkingNonEmpty ||
		k.thinkingDeltaNonEmpty ||
		k.thinkingBlocksNonEmpty ||
		bytes.HasPrefix(k.thought, []byte("true"))
}

// responseEventType returns the top-level type value for a Responses candidate:
// a payload whose top-level "type" value starts with "response." or contains a
// string escape (which could decode to that prefix). Ordinary frames
// (including Anthropic's "type":"content_block_delta") exit after the
// raw-prefix check without decoding or allocating. Candidates use the same
// full-object decode as before, preserving duplicate-key and malformed-frame
// behavior exactly.
func (k *frameKeys) responseEventType(payload []byte) string {
	value := k.eventType
	if len(value) < 2 || value[0] != '"' {
		return ""
	}
	if !bytes.HasPrefix(value, []byte(`"response.`)) && !stringTokenHasEscape(value) {
		return ""
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return ""
	}
	return event.Type
}

// stringTokenHasEscape reports whether the string token starting at buf[0]
// contains a backslash escape before its closing quote. Only the token is
// inspected, so an escaped later field never triggers a needless type decode.
func stringTokenHasEscape(buf []byte) bool {
	for i := 1; i < len(buf); i++ {
		switch buf[i] {
		case '\\':
			return true
		case '"':
			return false
		}
	}
	return false
}

// nonEmptyJSONValue applies the analyzer's value classification to a raw value
// slice: a non-empty string, a non-empty array/object, or any scalar other
// than null/false counts. Bare null and false do not.
func nonEmptyJSONValue(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	switch value[0] {
	case '"':
		return len(value) > 1 && value[1] != '"'
	case '[', '{':
		i := metrics.SkipSpace(value, 1)
		return i < len(value) && value[i] != ']' && value[i] != '}'
	case 'n', 'f':
		return false
	}
	return true
}
