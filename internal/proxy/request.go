package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// discardedJSON accepts every syntactically valid JSON shape as a no-op. The
// enclosing decoder owns syntax validation, and the metadata key scan only
// locates values with the shared lexical scanner, so nothing is materialized
// or re-decoded merely to be discarded.
type discardedJSON struct{}

func (*discardedJSON) UnmarshalJSON([]byte) error { return nil }

type requestRouting struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type requestMessage[C any] struct {
	Role    string `json:"role"`
	Content C      `json:"content"`
}

type requestParameters struct {
	Temperature    *float64          `json:"temperature"`
	TopP           *float64          `json:"top_p"`
	Tools          []discardedJSON   `json:"tools"`
	ToolChoice     requestToolChoice `json:"tool_choice"`
	StreamOptions  *struct{}         `json:"stream_options"`
	N              json.RawMessage   `json:"n"`
	Stop           json.RawMessage   `json:"stop"`
	StopSequences  json.RawMessage   `json:"stop_sequences"`
	Logprobs       bool              `json:"logprobs"`
	PresencePen    *float64          `json:"presence_penalty"`
	FrequencyPen   *float64          `json:"frequency_penalty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	// Text and Include are the native Responses spellings for the response
	// format/verbosity pair and the logprob opt-in. They are raw so a valid
	// but unusual shape cannot trigger a typed-decode failure that erases the
	// rest of the record.
	Text    json.RawMessage `json:"text"`
	Include json.RawMessage `json:"include"`
	// Functions and FunctionCall are the deprecated Chat spellings the codec
	// still converts. Keep them raw so a schema-invalid member records a
	// lexical count instead of failing the whole parameters decode.
	Functions       json.RawMessage          `json:"functions"`
	FunctionCall    json.RawMessage          `json:"function_call"`
	Seed            json.RawMessage          `json:"seed"`
	ParallelTools   *bool                    `json:"parallel_tool_calls"`
	LogitBias       map[string]discardedJSON `json:"logit_bias"`
	TopLogprobs     json.RawMessage          `json:"top_logprobs"`
	ServiceTier     string                   `json:"service_tier"`
	Thinking        json.RawMessage          `json:"thinking"`
	Metadata        requestMetadataKeys      `json:"metadata"`
	ReasoningEffort string                   `json:"reasoning_effort"`
	Verbosity       string                   `json:"verbosity"`
	Reasoning       json.RawMessage          `json:"reasoning"`
	OutputConfig    *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"output_config"`
	Speed string `json:"speed"`
	// System carries the system prompt for Anthropic-shaped bodies (OpenAI
	// puts it in a role:"system"/"developer" message instead). Content only.
	System   requestContent                   `json:"system"`
	Messages []requestMessage[requestContent] `json:"messages"`
}

// requestToolChoice captures the recorded shape of a tool_choice value during
// the single document decode. The string shorthand is kept verbatim. A Chat
// object contributes function.name with the "tool:" prefix; an Anthropic object
// contributes its type (any becomes the Chat required spelling, tool becomes
// tool:name, auto and none stay themselves) and its disable_parallel_tool_use
// flag. Every other or malformed shape records nothing. A typed decode visits
// only the known members and skips unknown or deeply nested ones instead of
// materializing the whole value as a map[string]any per node. Each top-level
// occurrence resets the field, so a duplicate tool_choice key keeps JSON's
// last-wins semantics.
type requestToolChoice struct {
	Value           string
	DisableParallel bool
	DisableSet      bool
}

func (t *requestToolChoice) UnmarshalJSON(raw []byte) error {
	*t = requestToolChoice{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			t.Value = s
		}
		return nil
	}
	var choice struct {
		// Raw members keep JSON's last-wins semantics for duplicate keys and
		// let one malformed member reset the whole record instead of failing
		// the request.
		Function        json.RawMessage `json:"function"`
		Type            string          `json:"type"`
		Name            string          `json:"name"`
		DisableParallel json.RawMessage `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(trimmed, &choice) != nil {
		return nil
	}
	if raw := bytes.TrimSpace(choice.DisableParallel); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var disable bool
		if json.Unmarshal(raw, &disable) != nil {
			return nil
		}
		t.DisableSet = true
		t.DisableParallel = disable
	}
	if raw := bytes.TrimSpace(choice.Function); len(raw) > 0 {
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		var function struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(raw, &function) == nil && function.Name != nil {
			t.Value = "tool:" + *function.Name
		}
		return nil
	}
	switch choice.Type {
	case "any":
		t.Value = "required"
	case "tool", "function":
		// Chat/Anthropic spell the forced tool as type "tool" with name, and
		// native Responses spells it as type "function" with name.
		if choice.Name != "" {
			t.Value = "tool:" + choice.Name
		}
	case "auto", "none":
		t.Value = choice.Type
	}
	return nil
}

// requestStopCount records how many stop sequences a raw stop field carries.
// A string is one sequence and an array contributes its length; null, an
// object and every other shape record zero. The scan is lexical: counting an
// accepted array never materializes a []string, and an over-bound array stops
// at requestMetadataMaxElements+1.
func requestStopCount(raw json.RawMessage) int {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) {
		return 0
	}
	switch raw[i] {
	case '"':
		if metrics.SkipJSONString(raw, i) == i+2 {
			return 0
		}
		return 1
	case '[':
		return requestArrayCount(raw)
	}
	return 0
}

// requestIncludeLogprobs reports whether a raw Responses include array names
// the output-text logprob option. The scan is lexical: it never materializes
// the array, and a malformed or non-array value records nothing.
func requestIncludeLogprobs(raw json.RawMessage) bool {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return false
	}
	const field = "message.output_text.logprobs"
	i = metrics.SkipSpace(raw, i+1)
	for i < len(raw) && raw[i] != ']' {
		if raw[i] == '"' {
			end := skipJSONString(raw, i)
			if end < 0 {
				return false
			}
			// Decode the token so an escaped spelling such as
			// "message.output_text\u002elogprobs" matches the option the
			// codec sees after its own JSON decode.
			var member string
			if json.Unmarshal(raw[i:end], &member) == nil && member == field {
				return true
			}
			i = metrics.SkipSpace(raw, end)
		} else {
			next := skipJSONValue(raw, i)
			if next < 0 {
				return false
			}
			i = metrics.SkipSpace(raw, next)
		}
		if i < len(raw) && raw[i] == ',' {
			i = metrics.SkipSpace(raw, i+1)
		}
	}
	return false
}

// requestArrayCount counts a raw JSON array's members lexically, so a
// schema-invalid member still records how many entries the client offered. A
// non-array or malformed value records zero; the count saturates at the shared
// collection ceiling.
func requestArrayCount(raw json.RawMessage) int {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return 0
	}
	n, _, ok := metrics.CountJSONArrayBytes(raw, i, requestMetadataMaxElements)
	if !ok {
		return 0
	}
	return n
}

// requestFunctionCallChoice maps the deprecated Chat function_call selector to
// the same recorded spelling as tool_choice. The codec gives tool_choice
// precedence, so callers apply this only when tool_choice recorded nothing.
func requestFunctionCallChoice(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var shorthand string
		if json.Unmarshal(trimmed, &shorthand) != nil {
			return ""
		}
		switch shorthand {
		case "auto", "none", "required":
			return shorthand
		}
	case '{':
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(trimmed, &obj) == nil {
			if name := strings.TrimSpace(obj.Name); name != "" {
				return "tool:" + name
			}
		}
	}
	return ""
}

// requestMetadataKeysMax bounds the distinct top-level metadata keys counted
// per request. Metadata is a small client note map; counts below the cap are
// exact, and a count at the cap means the map held at least that many keys.
// Internal safety guardrail, not user-tunable.
const requestMetadataKeysMax = 1024

// requestMetadataKeys counts a metadata object's distinct top-level keys
// without materializing a per-key value map. The enclosing document decode
// validates JSON syntax before this runs, and this scan only locates values
// with the shared lexical scanner before passing them to the no-op
// discardedJSON, so no value is decoded. Duplicate metadata members merge like
// the previous map field, and null clears the accumulated keys. The recorded
// count saturates at requestMetadataKeysMax: a value at the cap means "at
// least this many keys", never that a larger map was fully counted.
type requestMetadataKeys struct {
	keys map[string]struct{}
}

func (m *requestMetadataKeys) UnmarshalJSON(raw []byte) error {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) {
		return nil
	}
	if raw[i] == 'n' {
		// JSON null replaces the previous map value, as map decoding did.
		m.keys = nil
		return nil
	}
	if raw[i] != '{' {
		// Preserve the standard decoder's type error for a non-object value so
		// the split-decode boundary is unchanged.
		var discarded map[string]discardedJSON
		return json.Unmarshal(raw, &discarded)
	}
	if m.keys == nil {
		m.keys = make(map[string]struct{}, 8)
	}
	i = metrics.SkipSpace(raw, i+1)
	for i < len(raw) && raw[i] != '}' {
		keyEnd := skipJSONString(raw, i)
		if keyEnd < 0 {
			return fmt.Errorf("metadata key is not a JSON string")
		}
		if len(m.keys) < requestMetadataKeysMax {
			var key string
			if err := json.Unmarshal(raw[i:keyEnd], &key); err != nil {
				return err
			}
			m.keys[key] = struct{}{}
		}
		i = metrics.SkipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return fmt.Errorf("metadata member has no value")
		}
		i = metrics.SkipSpace(raw, i+1)
		if i >= len(raw) {
			return fmt.Errorf("metadata member has no value")
		}
		valueEnd := skipJSONValue(raw, i)
		if valueEnd < 0 {
			return fmt.Errorf("metadata value is unterminated")
		}
		var discarded discardedJSON
		if err := discarded.UnmarshalJSON(raw[i:valueEnd]); err != nil {
			return err
		}
		i = metrics.SkipSpace(raw, valueEnd)
		if i < len(raw) && raw[i] == ',' {
			i = metrics.SkipSpace(raw, i+1)
			continue
		}
		break
	}
	return nil
}

func (m *requestMetadataKeys) count() int { return len(m.keys) }

// parseLLMRequest is the single document decode for routing and metadata.
// Keep the two independent trust boundaries on type errors: a malformed
// metadata-only field must not erase valid model/stream routing, and vice
// versa. Only that exceptional path needs independent decoding; malformed
// JSON still passes through unchanged with no partially extracted metadata.
// requestMetadataMaxElements bounds the decoded top-level collection sizes and
// the distinct top-level member count before the single metadata decode and
// before the rewrite engine materializes every top-level value. A 32 MiB body
// can otherwise expand into hundreds of MiB of empty objects or raw values;
// this is an internal DoS guardrail, not a wire-format limit, and 10k
// messages/tools/logit_bias/stop entries or top-level keys is far above a
// normal request. The wire codecs share this top-level collection ceiling for
// their own decode preflight. Nested content and duplicate-occurrence bounds
// can still be stricter on either side: the preflight accumulates every
// duplicate top-level system member while the Anthropic codec decodes only the
// last, and the codec's per-array content cap is smaller than the shared
// aggregate.
const requestMetadataMaxElements = providerformat.WireCollectionMax

// requestContentPartsMax bounds the aggregate content-part arrays over every
// message, the top-level system content and every nested Responses input
// content/output array. The per-array metrics.ContentPartMax keeps one message
// from expanding a typed slice, but thousands of messages at that per-array cap
// still compose a request-sized slice of empty parts. It reuses the format
// owner so the preflight and the translated codec accept the same aggregate.
// Internal DoS guardrail, not a wire-format or user-tunable limit; text length
// never counts against it.
const requestContentPartsMax = providerformat.WireContentPartsMax

// requestAnnotationsMax bounds the aggregate annotation members over every
// Responses content part. Each annotation becomes a typed citation and a
// possible warning, so per-array ContentPartMax alone lets thousands of parts
// multiply. It reuses the format owner's aggregate so the proxy preflight and
// the codec reject the same document class.
const requestAnnotationsMax = providerformat.WireNestedCollectionMax

// requestAnnotationArrayMax bounds one Responses annotation array. It matches
// the codec's per-array check in checkResponsesContent, so a single oversized
// array fails in the preflight instead of after translation begins.
const requestAnnotationArrayMax = metrics.ContentPartMax

// requestToolCallsMax bounds the aggregate tool_calls array members over every
// message. Each member expands into a typed tool-call value and, on the
// translated Anthropic or Cursor wire, a content block/history part. A 300 KiB
// body of empty calls therefore materializes six figures of typed values and
// over a hundred MiB of transient allocation. Internal DoS guardrail, not a
// wire-format or user-tunable limit; real requests carry a handful of calls
// per turn.
const requestToolCallsMax = providerformat.WireToolCallsMax

// boundRequestMetadata is a lexical preflight over the top-level document. It
// rejects only a valid JSON document whose messages/tools/stop array,
// logit_bias object, distinct top-level member count, aggregate content parts,
// or aggregate tool calls exceed their bounds. Invalid JSON returns nil so the
// existing passthrough behavior is preserved; collection members are skipped
// with the shared byte scanner. One small distinct-key set per top-level object
// records only the keys it sees; escaped and non-ASCII keys decode once per
// member and that result feeds both the distinct set and field comparisons.
func boundRequestMetadata(body []byte) error {
	i := metrics.SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil
	}
	i = metrics.SkipSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		return nil
	}
	// logit_bias is a map field: duplicate top-level objects merge in
	// encoding/json, so distinct keys accumulate across every occurrence.
	// Slice fields overwrite for the final value, but every duplicate
	// occurrence still reaches its decoder, so each is counted below.
	var logitSeen map[string]struct{}
	logitOver := false
	// metadata is also a map field. Duplicate metadata objects merge in
	// encoding/json, so distinct keys accumulate across every occurrence. A
	// single occurrence needs only a raw member count: distinct keys cannot
	// exceed raw members, and the typed decode caps its retained keys at the
	// same ceiling. The distinct-key set is built only once a second object
	// makes merge semantics relevant, so a hostile single object never
	// retains a decoded key map.
	var metadataSeen map[string]struct{}
	metadataObjects := 0
	metadataFirstStart := 0
	metadataFirstOver := false
	metadataOver := false
	// memberSeen counts distinct top-level keys because the rewrite engine and
	// metadata decode both keep one entry per distinct key; duplicate members
	// must stay cheap and accepted. Encoding/json retains the last duplicate
	// value, so raw member count is not the decoded map size.
	var memberSeen map[string]struct{}
	// contentParts accumulates message content-part array lengths, the
	// top-level system content and every nested Responses input content/output
	// part. annotations accumulates the annotation members inside each of
	// those parts. toolCalls accumulates every message tool_calls member. All
	// stop growing at the first over-budget array, so the rejection costs one
	// bounded prefix scan.
	contentParts := 0
	annotationParts := 0
	toolCalls := 0
	// stream_options is a RawMessage field: duplicate top-level occurrences
	// overwrite, so only the last object reaches the typed decode. Retain its
	// span and bound that occurrence after the scan, matching the codec.
	streamOptionsStart, streamOptionsEnd := -1, -1
	streamOptionsSeen := false
	for i < len(body) {
		keyStart := i
		end := skipJSONString(body, i)
		if end < 0 {
			return nil
		}
		key, ok := decodeRequestKey(body[keyStart:end])
		if !ok {
			return nil
		}
		if memberSeen == nil {
			memberSeen = make(map[string]struct{})
		}
		n := countDistinctJSONKey(memberSeen, key, requestMetadataMaxElements)
		if n > requestMetadataMaxElements {
			return requestMetadataOverflow(body, fmt.Errorf("request top-level members exceed %d", requestMetadataMaxElements))
		}
		i = metrics.SkipSpace(body, end)
		if i >= len(body) || body[i] != ':' {
			return nil
		}
		i = metrics.SkipSpace(body, i+1)
		if i >= len(body) {
			return nil
		}
		collection := key.collection()
		switch collection {
		case "messages":
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			if n > 0 {
				parts, ok := countRequestMessagesContentParts(body, i, requestContentPartsMax-contentParts)
				if !ok {
					return nil
				}
				if parts > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				contentParts += parts
				calls, ok := countRequestMessagesToolCalls(body, i, requestToolCallsMax-toolCalls)
				if !ok {
					return nil
				}
				if calls > requestToolCallsMax-toolCalls {
					return requestMetadataOverflow(body, fmt.Errorf("request tool calls exceed %d", requestToolCallsMax))
				}
				toolCalls += calls
			}
			i = next
		case "tools", "functions", "modalities":
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			i = next
		case "input":
			// Responses input is an item array whose nested content/output
			// arrays are decoded by every translated request. Count both the
			// items and their parts/annotations here so the preflight rejects
			// an amplification body before the codec materializes it.
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			if n > 0 {
				parts, annotations, nestedNext, annotationArrayOver, nestedOK := countResponsesInputNested(
					body, i, requestContentPartsMax-contentParts, requestAnnotationsMax-annotationParts)
				if !nestedOK {
					return nil
				}
				if annotationArrayOver {
					return requestMetadataOverflow(body, fmt.Errorf("request annotation array exceeds %d", requestAnnotationArrayMax))
				}
				if parts > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				if annotations > requestAnnotationsMax-annotationParts {
					return requestMetadataOverflow(body, fmt.Errorf("request annotations exceed %d", requestAnnotationsMax))
				}
				contentParts += parts
				annotationParts += annotations
				next = nestedNext
			}
			i = next
		case "metadata":
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			trimmed := metrics.SkipSpace(body, i)
			switch {
			case trimmed < next && body[trimmed] == 'n':
				// encoding/json sets the map field to nil for null, so every
				// key merged before this member is discarded.
				clear(metadataSeen)
				metadataSeen = nil
				metadataObjects = 0
				metadataFirstStart = 0
				metadataFirstOver, metadataOver = false, false
			case trimmed < next && body[trimmed] == '{':
				metadataObjects++
				rawOK := metrics.ObjectWithinLimit(body[i:next], requestMetadataMaxElements)
				if metadataObjects == 1 {
					metadataFirstStart = i
					metadataFirstOver = !rawOK
				} else if !metadataOver {
					if metadataSeen == nil {
						// Merge semantics now need the first object's
						// distinct keys; it was counted but not decoded.
						metadataSeen = make(map[string]struct{}, 8)
						n, _, ok := countJSONObjectKeys(body, metadataFirstStart, requestMetadataMaxElements, metadataSeen)
						if !ok {
							return nil
						}
						if n > requestMetadataMaxElements {
							metadataOver = true
						}
					}
					if !metadataOver {
						n, _, ok := countJSONObjectKeys(body, i, requestMetadataMaxElements, metadataSeen)
						if !ok {
							return nil
						}
						if n > requestMetadataMaxElements {
							metadataOver = true
						}
					}
				}
			}
			i = next
		case "stream_options":
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			streamOptionsStart, streamOptionsEnd = i, next
			streamOptionsSeen = true
			i = next
		case "stop":
			// stop is decoded into []string only to count it. A string is one
			// value and every non-array shape is tolerated by the metadata
			// decoder, so only an array is bounded here.
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request stop exceeds %d values", requestMetadataMaxElements))
			}
			i = next
		case "logit_bias":
			if logitOver {
				next := skipJSONValue(body, i)
				if next < 0 {
					return nil
				}
				i = next
				break
			}
			if logitSeen == nil {
				logitSeen = make(map[string]struct{})
			}
			n, next, ok := countJSONObjectKeys(body, i, requestMetadataMaxElements, logitSeen)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				logitOver = true
			}
			i = next
		default:
			if key.equal("system") {
				// Anthropic bodies carry a top-level system field whose array
				// shape composes through the same requestContent decoder as a
				// message. Count it against the aggregate budget; a string or
				// other shape contributes no parts.
				n, next, ok := metrics.CountJSONArrayBytes(body, i, requestContentPartsMax-contentParts)
				if !ok {
					return nil
				}
				if n > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				contentParts += n
				i = next
				break
			}
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			i = next
		}
		i = metrics.SkipSpace(body, i)
		if i >= len(body) {
			return nil
		}
		if body[i] == '}' {
			break
		}
		if body[i] != ',' {
			return nil
		}
		i = metrics.SkipSpace(body, i+1)
	}
	if logitOver {
		return requestMetadataOverflow(body, fmt.Errorf("request logit_bias exceeds %d entries", requestMetadataMaxElements))
	}
	if metadataOver || (metadataObjects == 1 && metadataFirstOver) {
		return requestMetadataOverflow(body, fmt.Errorf("request metadata exceeds %d members", requestMetadataMaxElements))
	}
	if streamOptionsSeen &&
		!metrics.ObjectWithinLimit(body[streamOptionsStart:streamOptionsEnd], requestMetadataMaxElements) {
		return requestMetadataOverflow(body, fmt.Errorf("request stream_options exceeds %d members", requestMetadataMaxElements))
	}
	return nil
}

// requestKey is one raw JSON object key decoded at most once per member. The
// common plain ASCII spelling keeps only the source slice, so its comparisons
// and duplicate lookups allocate nothing; text holds the canonical string
// when the source needs JSON unescaping, invalid UTF-8 replacement or Unicode
// case folding. One top-level member used to decode an escaped key once for
// the distinct-key set and again for every candidate field name.
type requestKey struct {
	raw     []byte
	text    string
	textSet bool
}

// decodeRequestKey validates one raw JSON string key and decodes it at most
// once. ok is false on an undecodable key, which leaves the document on the
// existing malformed-JSON passthrough path.
func decodeRequestKey(raw []byte) (requestKey, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return requestKey{}, false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		ascii := true
		for _, c := range inner {
			if c >= utf8.RuneSelf {
				ascii = false
				break
			}
		}
		if ascii {
			return requestKey{raw: inner}, true
		}
		// Valid non-ASCII UTF-8 needs no unescaping, but struct-field
		// matching still folds Unicode, so retain one canonical string.
		return requestKey{raw: inner, text: string(inner), textSet: true}, true
	}
	var decoded string
	if json.Unmarshal(raw, &decoded) != nil {
		return requestKey{}, false
	}
	return requestKey{raw: inner, text: decoded, textSet: true}, true
}

// equal reports whether the key names the given struct field. encoding/json
// matches struct fields case-insensitively, while map keys stay exact.
func (k requestKey) equal(name string) bool {
	if k.textSet {
		return strings.EqualFold(k.text, name)
	}
	return asciiKeyEqual(k.raw, name)
}

// collection returns the canonical bounded collection name for this top-level
// key, or "" when the key is not one of them. encoding/json matches struct
// fields case-insensitively, so "Messages"/"MESSAGES" reach messages; it does
// not fold '-' or '_', so "Mes-sages" stays unknown.
func (k requestKey) collection() string {
	switch {
	case k.equal("messages"):
		return "messages"
	case k.equal("tools"):
		return "tools"
	case k.equal("functions"):
		return "functions"
	case k.equal("input"):
		return "input"
	case k.equal("metadata"):
		return "metadata"
	case k.equal("stream_options"):
		return "stream_options"
	case k.equal("logit_bias"):
		return "logit_bias"
	case k.equal("stop"), k.equal("stop_sequences"):
		return "stop"
	case k.equal("modalities"):
		return "modalities"
	}
	return ""
}

// requestKeyEqual reports whether one raw JSON object key names the given
// struct field. It serves the nested key scans that decode one key per call;
// the top-level preflight decodes once and reuses the requestKey instead.
func requestKeyEqual(raw []byte, name string) bool {
	key, ok := decodeRequestKey(raw)
	return ok && key.equal(name)
}

// countDistinctJSONKey records one decoded top-level object key in seen and
// returns the distinct count. Plain ASCII keys allocate only when a new key
// is inserted; duplicate lookups stay allocation-free. Escaped, non-ASCII or
// invalid UTF-8 keys reuse the member's one canonical decode, so they compare
// with the exact string the rewrite map will hold. Once max distinct keys are
// seen, a new key reports max+1 without recording it, so the caller can reject
// without growing the set.
func countDistinctJSONKey(seen map[string]struct{}, key requestKey, max int) int {
	if key.textSet {
		if _, exists := seen[key.text]; exists {
			return len(seen)
		}
		if len(seen) >= max {
			return max + 1
		}
		seen[key.text] = struct{}{}
		return len(seen)
	}
	if _, exists := seen[string(key.raw)]; exists {
		return len(seen)
	}
	if len(seen) >= max {
		return max + 1
	}
	seen[string(key.raw)] = struct{}{}
	return len(seen)
}

// countRequestMessagesContentParts counts the aggregate content-part array
// lengths over the message objects of the messages array at start, including
// every duplicate content occurrence in a message. budget is the remaining
// aggregate allowance; a count above it means the aggregate bound is exceeded.
// Non-object elements and non-array content contribute zero, matching the
// typed decoder's shape tolerance. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countRequestMessagesContentParts(data []byte, start, budget int) (int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '[' {
		return 0, true
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != ']' {
		n, next, ok := countRequestMessageContentParts(data, i, budget-total)
		if !ok {
			return 0, false
		}
		if n > budget-total {
			return total + n, true
		}
		total += n
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, false
	}
	return total, true
}

// countRequestMessageContentParts counts one message object's content arrays.
// Duplicate content members each reach requestContent.UnmarshalJSON before
// encoding/json's last-wins field assignment, so every occurrence is charged
// against the budget; only a non-array occurrence contributes zero. Counting
// stops at the first occurrence that crosses the remaining budget, so the
// rejection costs one bounded prefix scan.
func countRequestMessageContentParts(data []byte, start, budget int) (int, int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		next := metrics.SkipJSONValue(data, i)
		return 0, next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	parts := 0
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		if requestKeyEqual(key, "content") {
			n, next, ok := metrics.CountJSONArrayBytes(data, i, budget-parts)
			if !ok {
				return 0, 0, false
			}
			parts += n
			if parts > budget {
				return parts, 0, true
			}
			i = next
		} else {
			next := skipJSONValue(data, i)
			if next < 0 {
				return 0, 0, false
			}
			i = next
		}
		i = metrics.SkipSpace(data, i)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	return parts, i + 1, true
}

// countResponsesInputNested counts the aggregate content-part and annotation
// members of one Responses input array without materializing it. Every item
// object's content and output arrays contribute parts; every part object's
// annotations array contributes annotation members. budget is the remaining
// aggregate allowance; a count above it means the bound is exceeded. Duplicate
// members are each charged because every occurrence reaches the decoder.
// Non-object items and non-array values contribute zero, matching the typed
// decoder's shape tolerance. annotationArrayOver means one annotation array
// exceeded requestAnnotationArrayMax. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countResponsesInputNested(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, 0, false, false
	}
	if data[i] != '[' {
		next := skipJSONValue(data, i)
		return 0, 0, next, false, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != ']' {
		if data[i] == '{' {
			itemParts, itemAnnotations, itemNext, itemArrayOver, itemOK := countResponsesInputItemNested(data, i, partBudget-parts, annotationBudget-annotations)
			if !itemOK {
				return 0, 0, 0, false, false
			}
			parts += itemParts
			annotations += itemAnnotations
			if itemArrayOver {
				return parts, annotations, 0, true, true
			}
			if parts > partBudget || annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, itemNext)
		} else {
			itemNext := skipJSONValue(data, i)
			if itemNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, itemNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, 0, 0, false, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesInputItemNested counts one item object's content and output
// arrays. A string output or content value contributes no parts.
func countResponsesInputItemNested(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '{' {
		return 0, 0, 0, false, false
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, 0, false, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, 0, false, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, 0, false, false
		}
		if requestKeyEqual(key, "content") || requestKeyEqual(key, "output") {
			partCount, annotationCount, partNext, partArrayOver, partOK := countResponsesPartArray(data, i, partBudget-parts, annotationBudget-annotations)
			if !partOK {
				return 0, 0, 0, false, false
			}
			parts += partCount
			annotations += annotationCount
			if partArrayOver {
				return parts, annotations, 0, true, true
			}
			if parts > partBudget || annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, partNext)
		} else {
			valueNext := skipJSONValue(data, i)
			if valueNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, valueNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesPartArray counts one content-part array's members and the
// annotation members inside each part object. A string value contributes no
// parts, matching function_call_output's string-or-array contract.
func countResponsesPartArray(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, 0, false, false
	}
	if data[i] != '[' {
		next := skipJSONValue(data, i)
		return 0, 0, next, false, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != ']' {
		parts++
		if parts > partBudget {
			return parts, annotations, 0, false, true
		}
		if data[i] == '{' {
			annotationCount, partNext, partArrayOver, partOK := countResponsesPartAnnotations(data, i, annotationBudget-annotations)
			if !partOK {
				return 0, 0, 0, false, false
			}
			annotations += annotationCount
			if partArrayOver {
				return parts, annotations, 0, true, true
			}
			if annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, partNext)
		} else {
			partNext := skipJSONValue(data, i)
			if partNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, partNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, 0, 0, false, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesPartAnnotations counts one part object's annotations array.
// Duplicate annotations members are each charged for the same reason as
// duplicate content members. One array above requestAnnotationArrayMax sets
// annotationArrayOver before the aggregate check.
func countResponsesPartAnnotations(data []byte, start, budget int) (total, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '{' {
		return 0, 0, false, false
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false, false
		}
		if requestKeyEqual(key, "annotations") {
			n, valueNext, countOK := metrics.CountJSONArrayBytes(data, i, requestAnnotationArrayMax)
			if !countOK {
				return 0, 0, false, false
			}
			if n > requestAnnotationArrayMax {
				return total + n, 0, true, true
			}
			total += n
			if total > budget {
				return total, 0, false, true
			}
			i = metrics.SkipSpace(data, valueNext)
		} else {
			valueNext := skipJSONValue(data, i)
			if valueNext < 0 {
				return 0, 0, false, false
			}
			i = metrics.SkipSpace(data, valueNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false, false
	}
	return total, i + 1, false, true
}

// countRequestMessagesToolCalls counts the aggregate tool_calls array members
// over the message objects of the messages array at start. budget is the
// remaining aggregate allowance; a count above it means the aggregate bound is
// exceeded. Non-object elements and non-array values contribute zero, matching
// the typed decoder's shape tolerance. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countRequestMessagesToolCalls(data []byte, start, budget int) (int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '[' {
		return 0, true
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != ']' {
		n, next, ok := countRequestMessageToolCalls(data, i, budget-total)
		if !ok {
			return 0, false
		}
		if n > budget-total {
			return total + n, true
		}
		total += n
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, false
	}
	return total, true
}

// countRequestMessageToolCalls counts one message object's tool_calls array
// members. Every occurrence accumulates: encoding/json decodes each duplicate
// tool_calls member into a fresh typed slice before the field's last-wins
// assignment, so each one allocates even though only the final slice survives.
// Once the budget is crossed, counting stops and the over-count is returned so
// the caller rejects before any translation decoder materializes the calls.
func countRequestMessageToolCalls(data []byte, start, budget int) (int, int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		next := metrics.SkipJSONValue(data, i)
		return 0, next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		if requestKeyEqual(key, "tool_calls") {
			n, next, ok := metrics.CountJSONArrayBytes(data, i, budget-total)
			if !ok {
				return 0, 0, false
			}
			total += n
			if total > budget {
				return total, 0, true
			}
			i = next
		} else {
			next := metrics.SkipJSONValue(data, i)
			if next < 0 {
				return 0, 0, false
			}
			i = next
		}
		i = metrics.SkipSpace(data, i)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	return total, i + 1, true
}

func asciiKeyEqual(raw []byte, name string) bool {
	if len(raw) != len(name) {
		return false
	}
	for i, c := range raw {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		w := name[i]
		if w >= 'A' && w <= 'Z' {
			w += 'a' - 'A'
		}
		if c != w {
			return false
		}
	}
	return true
}

// countJSONObjectKeys accumulates the distinct keys of one JSON map value
// into seen. encoding/json reuses the non-nil map field across duplicate
// top-level map members (metadata, logit_bias), so distinct keys, not raw
// members, are the decoded size. Duplicate members of one object therefore do
// not consume the bound. Plain ASCII keys decode from their source slice and
// allocate only the map key; escaped or non-ASCII keys decode once through the
// shared requestKey owner. Once seen exceeds max, the map is cleared and the
// rest of the object is skipped without decoding keys; the caller preserves
// the overflow state and skips later occurrences entirely.
func countJSONObjectKeys(data []byte, i, max int, seen map[string]struct{}) (int, int, bool) {
	i = metrics.SkipSpace(data, i)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		// encoding/json sets a map field to nil when the value is null, so a
		// null between duplicate objects resets the accumulated map rather
		// than leaving the previous keys counted. Other non-object values are
		// type errors handled by the main decode; leave the accumulator alone.
		if len(data)-i >= 4 && bytes.Equal(data[i:i+4], []byte("null")) {
			clear(seen)
			return 0, i + 4, true
		}
		next := skipJSONValue(data, i)
		return len(seen), next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	over := false
	for i < len(data) && data[i] != '}' {
		keyStart := i
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		if !over {
			key, ok := decodeRequestKey(data[keyStart:end])
			if !ok {
				return 0, 0, false
			}
			if countDistinctJSONKey(seen, key, max) > max {
				over = true
				clear(seen)
			}
		}
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		next := skipJSONValue(data, i)
		if next <= i {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	if over {
		return max + 1, i + 1, true
	}
	return len(seen), i + 1, true
}

// requestMetadataOverflow rejects only a fully valid JSON document. The
// lexical counter can have seen an over-cap prefix before invalid trailing
// bytes; malformed input must keep its unchanged passthrough contract, so
// validation happens only on the rejection path.
func requestMetadataOverflow(body []byte, err error) error {
	if !json.Valid(body) {
		return nil
	}
	return err
}

// requestStringExpansionMax is the raw-byte headroom allowed for decoded JSON
// string bytes beyond the request's own size. Valid UTF-8 and JSON escapes
// decode to no more bytes than their spelling, so only raw invalid UTF-8 can
// consume this allowance: encoding/json replaces each invalid byte with
// U+FFFD (three bytes). 4 KiB keeps the documented drop-not-reject small-value
// cases bounded (a tracked identity is at most 512 bytes) while rejecting the
// body-sized replacement expansion. Internal safety guardrail, not
// user-tunable.
const requestStringExpansionMax = 4 << 10

// requestStringExpansionExceeded reports whether a syntactically valid JSON
// document's decoded strings would exceed the body size by more than
// requestStringExpansionMax. Without this preflight a body-sized string of raw
// invalid UTF-8 triples across the metadata decode and translation in memory
// before any configured size bound sees the result. The scan is lexical and
// allocation-free. Malformed documents return false to keep the established
// passthrough contract; encoding/json validates their syntax before decoding
// any string, so they cannot expand either.
func requestStringExpansionExceeded(body []byte) bool {
	// Ordinary requests are valid UTF-8, so every JSON string's decoded form
	// is no larger than its raw spelling and no string scan is needed. Only a
	// body carrying raw invalid bytes can expand.
	if utf8.Valid(body) {
		return false
	}
	decoded, ok := decodedJSONStringBytes(body)
	if !ok || decoded <= len(body)+requestStringExpansionMax {
		return false
	}
	return json.Valid(body)
}

// decodedJSONStringBytes sums the decoded UTF-8 byte length of every JSON
// string in data. Escapes are bounded by their raw spelling: a two-byte escape
// decodes to one byte, and a six-byte \uXXXX escape is counted as at most
// three, so a valid document's decoded strings never exceed its raw size
// unless raw invalid UTF-8 is present. An invalid byte is counted as its
// three-byte U+FFFD replacement.
func decodedJSONStringBytes(data []byte) (int, bool) {
	total := 0
	for i := 0; i < len(data); {
		if data[i] != '"' {
			i++
			continue
		}
		end := metrics.SkipJSONString(data, i)
		if end < 0 {
			return 0, false
		}
		n, ok := decodedJSONStringLength(data[i:end])
		if !ok {
			return 0, false
		}
		total += n
		i = end
	}
	return total, true
}

// decodedJSONStringLength returns the decoded UTF-8 byte length of one raw
// JSON string span. It never allocates and accepts malformed escapes only far
// enough to keep the caller on its passthrough path.
func decodedJSONStringLength(raw []byte) (int, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, false
	}
	s := raw[1 : len(raw)-1]
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\\' {
			i++
			if i >= len(s) {
				return 0, false
			}
			if s[i] == 'u' {
				if i+4 >= len(s) {
					return 0, false
				}
				// Six raw bytes decode to at most three UTF-8 bytes; a
				// surrogate pair is two escapes (twelve raw bytes) and
				// decodes to four, still bounded by its spelling.
				n += 3
				i += 5
				continue
			}
			n++
			i++
			continue
		}
		if c < utf8.RuneSelf {
			n++
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size == 1 {
			n += utf8.RuneLen(utf8.RuneError)
			i++
			continue
		}
		n += size
		i += size
	}
	return n, true
}

func parseLLMRequest(body []byte, rec *metrics.Record, preview bool) error {
	// The common minimal body is fully recognized by a lexical pre-pass. Every
	// other shape, including preview capture, keeps the typed decode below
	// unchanged.
	if !preview && parseLLMRequestFast(body, rec) {
		return nil
	}
	return parseLLMRequestSlow(body, rec, preview, true)
}

// parseGeneratedLLMRequest decodes the proxy's own translated request body.
// It passes bound=false, so parseLLMRequestSlow skips ALL of
// boundRequestMetadata: the messages/tools/stop array limits, the logit_bias,
// metadata and distinct top-level-member counts, the aggregate content-part
// and annotation counts, and the 1,024 aggregate tool-call bound. That is
// deliberate: translation legitimately changes composition (one tool result
// becomes a content block, media parts split, lifted system fragments add
// members), and re-applying the client-facing aggregate would reject a body
// the codec just built. The broad skip is safe because the source body was
// already bounded by parseLLMRequest, the codec validates the IR and its own
// wire collections, and the translated document is capped by
// max_request_bytes before this decode runs. UTF-8 expansion, the typed
// metadata decode and the token-cap recheck remain active.
func parseGeneratedLLMRequest(body []byte, rec *metrics.Record, preview bool) error {
	if !preview && parseLLMRequestFast(body, rec) {
		return nil
	}
	return parseLLMRequestSlow(body, rec, preview, false)
}

func parseLLMRequestSlow(body []byte, rec *metrics.Record, preview, bound bool) error {
	// Reject replacement expansion before the first typed decode so raw
	// invalid UTF-8 cannot multiply the request in memory.
	if requestStringExpansionExceeded(body) {
		return fmt.Errorf("request body contains invalid UTF-8 that expands beyond the request size")
	}
	if bound {
		if err := boundRequestMetadata(body); err != nil {
			return err
		}
	}
	var req struct {
		requestRouting
		requestParameters
	}
	if err := json.Unmarshal(body, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil
		}
		req.requestRouting = requestRouting{}
		if json.Unmarshal(body, &req.requestRouting) != nil {
			req.requestRouting = requestRouting{}
		}
		req.requestParameters = requestParameters{}
		if json.Unmarshal(body, &req.requestParameters) != nil {
			// The cap is the one extracted field the trust boundary rejects on
			// (hostileTokenCap reads rec.ReqMaxTokens): a type error anywhere
			// else in the parameters block - including tool parameters carried
			// as raw JSON by the Anthropic translator - must not blind that
			// check. Recover the cap alone with applyTokenCaps' minimal decode
			// of the same body, applying the same max_completion_tokens-wins
			// precedence as the clean merge below (a cap pair that still does
			// not decode leaves the record's cap untouched).
			applyTokenCaps(body, rec)
			rec.Model, rec.Stream = retainedText(req.Model, retainedLabelMax), req.Stream
			return nil
		}
	}
	rec.Model, rec.Stream = retainedText(req.Model, retainedLabelMax), req.Stream
	// The occurrence scan owns the cap value, so the record describes the same
	// occurrence the trust boundary validated. Prefer max_completion_tokens,
	// matching the Anthropic translator's pick.
	applyTokenCaps(body, rec)
	// Integral spellings (1.0, 1e1) are accepted by every codec and relayed
	// verbatim, but a typed int decode fails them and erased the rest of the
	// record with it. Carry these three members as raw JSON and decode them
	// with the shared exact parser after the merged decode; a malformed
	// spelling records nothing here (the wire codec owns its own validation)
	// without discarding the other fields.
	n, errN := jsonnum.OptionalInt(req.N, "n")
	seed, errSeed := jsonnum.OptionalInt64(req.Seed, "seed")
	topLogprobs, errTop := jsonnum.OptionalInt(req.TopLogprobs, "top_logprobs")
	if errN == nil {
		rec.ReqN = n
	}
	if errSeed == nil {
		rec.ReqSeed = seed
	}
	if errTop == nil {
		rec.ReqTopLogprobs = topLogprobs
	}
	rec.ReqTemperature = req.Temperature
	rec.ReqTopP = req.TopP
	rec.ReqToolsCount = len(req.Tools) + requestArrayCount(req.Functions)
	rec.ReqPresencePen = req.PresencePen
	rec.ReqFrequencyPen = req.FrequencyPen
	rec.ReqLogprobs = req.Logprobs || requestIncludeLogprobs(req.Include)
	if req.StreamOptions != nil {
		rec.ReqStreamOpts = true
	}
	rec.ReqToolChoice = req.ToolChoice.Value
	if rec.ReqToolChoice == "" {
		rec.ReqToolChoice = requestFunctionCallChoice(req.FunctionCall)
	}
	if len(req.Stop) > 0 {
		rec.ReqStop = requestStopCount(req.Stop)
	} else if len(req.StopSequences) > 0 {
		rec.ReqStop = requestStopCount(req.StopSequences)
	}
	// Additional non-content params: capture shape only, never values that
	// could carry content (logit_bias keys are token ids, metadata is counted
	// not copied).
	// Native Responses text governs both the response format and verbosity.
	// Decode it after the merged pass and ignore a malformed shape for
	// metadata only; a scalar must not erase the rest of the record.
	var textFormat, textVerbosity string
	if len(req.Text) > 0 {
		var text struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
			Verbosity string `json:"verbosity"`
		}
		if json.Unmarshal(req.Text, &text) == nil {
			if text.Format != nil {
				textFormat = text.Format.Type
			}
			textVerbosity = text.Verbosity
		}
	}
	if req.ResponseFormat != nil {
		rec.ReqResponseFormat = req.ResponseFormat.Type
	} else if req.OutputConfig != nil && req.OutputConfig.Format != nil {
		rec.ReqResponseFormat = req.OutputConfig.Format.Type
	} else {
		rec.ReqResponseFormat = textFormat
	}
	if req.ParallelTools != nil {
		rec.ReqParallelTools = req.ParallelTools
	} else if req.ToolChoice.DisableSet {
		parallel := !req.ToolChoice.DisableParallel
		rec.ReqParallelTools = &parallel
	}
	if req.ServiceTier != "" {
		rec.ReqServiceTier = req.ServiceTier
	} else {
		rec.ReqServiceTier = req.Speed
	}
	rec.ReqLogitBias = len(req.LogitBias)
	rec.ReqMetadataKeys = req.Metadata.count()
	if len(req.Thinking) > 0 {
		// Record extended thinking only when the object asks for it. The
		// codec decodes type "disabled" as Enabled=false, so the presence of
		// a thinking object alone is not a request.
		var thinking struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.Thinking, &thinking) == nil {
			switch thinking.Type {
			case "enabled", "adaptive", "between_tools":
				rec.ReqThinking = true
			}
		}
	}
	if tok := configToken(req.ReasoningEffort); tok != "" {
		rec.ReqReasoningEffort = tok
	} else {
		var effort string
		if req.OutputConfig != nil {
			effort = configToken(req.OutputConfig.Effort)
		}
		if effort == "" && len(req.Reasoning) > 0 {
			var obj struct {
				Effort string `json:"effort"`
			}
			if json.Unmarshal(req.Reasoning, &obj) == nil {
				effort = configToken(obj.Effort)
			}
		}
		rec.ReqReasoningEffort = effort
	}
	rec.ReqVerbosity = configToken(req.Verbosity)
	if rec.ReqVerbosity == "" {
		rec.ReqVerbosity = configToken(textVerbosity)
	}
	// Prompt composition: per-role character sizes, multimodal counts, and the
	// role of the last turn (what the model is actually responding to). Sizes
	// count text characters only - never the text itself, and never inline
	// base64 payloads (an image part contributes its text siblings, not its
	// data). System size comes from a role:"system"/"developer" message
	// (OpenAI) and/or a top-level "system" field (Anthropic).
	rec.CharsSystem = req.System.chars
	rec.LastTurnRole = ""
	for _, m := range req.Messages {
		n := m.Content.chars
		switch m.Role {
		case "user":
			rec.TurnsUser++
			rec.CharsUser += n
		case "assistant":
			rec.TurnsAssistant++
			rec.CharsAssistant += n
		case "tool", "function":
			rec.TurnsTool++
			rec.CharsTool += n
		case "system", "developer":
			rec.CharsSystem += n
		}
		rec.Images += m.Content.images
		rec.Attachments += m.Content.attachments
		if m.Role != "" {
			rec.LastTurnRole = m.Role
		}
	}
	if preview && len(req.Messages) > 0 {
		// Content is never retained on the default path. Opt-in previews use
		// the same message shape with a bounded string-only content decoder.
		var captured struct {
			Messages []requestMessage[requestPreview] `json:"messages"`
		}
		if json.Unmarshal(body, &captured) != nil {
			return nil
		}
		for i := len(captured.Messages) - 1; i >= 0; i-- {
			if captured.Messages[i].Role == "user" {
				rec.PromptPreview = string(captured.Messages[i].Content)
				break
			}
		}
	}
	return nil
}

// configToken accepts a short alphanumeric request-config token (effort,
// verbosity, …). Deny by default: anything with spaces, punctuation other
// than _/-, or a long payload is dropped rather than stored.
func configToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return s
}

// requestContent measures one message content field, retaining only its text
// character count plus the number of image and non-image attachment parts.
// Content may be a plain string or an array of typed parts; the part "type"
// discriminator is matched by substring so both OpenAI ("image_url",
// "input_image", "input_audio", "file") and Anthropic ("image", "document",
// "tool_result") shapes - and future input_* variants - bucket consistently.
// Text is not retained, and base64 payloads are never decoded or retained.
type requestContent struct {
	chars, images, attachments int
}

func (c *requestContent) UnmarshalJSON(content []byte) error {
	*c = requestContent{}
	if len(content) > 0 && content[0] == '"' {
		var size requestStringSize
		if size.UnmarshalJSON(content) == nil {
			c.chars = int(size)
		}
		return nil
	}
	if i := metrics.SkipSpace(content, 0); i < len(content) && content[i] == '[' {
		if n, _, ok := metrics.CountJSONArrayBytes(content, i, metrics.ContentPartMax); !ok || n > metrics.ContentPartMax {
			return nil // best-effort composition, never a routing/type failure
		}
	}
	var parts []struct {
		Type string            `json:"type"`
		Text requestStringSize `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return nil // content is best-effort, never a routing/type failure
	}
	for _, p := range parts {
		switch {
		case strings.Contains(p.Type, "image"):
			c.images++
		case p.Type == "text" || strings.HasPrefix(p.Type, "input_text"):
			c.chars += int(p.Text)
		case p.Type != "" && p.Type != "thinking" && p.Type != "redacted_thinking" && p.Type != "tool_use":
			// Any other non-text part is an attachment: file/document/audio/
			// video or an embedded tool result. Model-authored reasoning and
			// tool-call blocks are excluded - they are not user attachments.
			c.attachments++
		}
	}
	return nil
}

// requestStringBytes only borrows bytes during UnmarshalJSON. The enclosing
// encoding/json call already validated quoting/control bytes. Invalid UTF-8
// stays on the standard decoder's Unicode replacement path.
func requestStringBytes(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return nil, false
	}
	s := raw[1 : len(raw)-1]
	return s, utf8.Valid(s)
}

type requestStringSize int

func (n *requestStringSize) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return nil // a repeated null leaves a string field unchanged
	}
	if s, ok := requestStringBytes(raw); ok {
		size := len(s)
		for {
			i := bytes.IndexByte(s, '\\')
			if i < 0 {
				*n = requestStringSize(size)
				return nil
			}
			if s[i+1] == 'u' {
				break // the standard decoder owns Unicode/surrogate semantics
			}
			// Every other validated JSON escape is two bytes representing one
			// ASCII byte (newline, quote, slash, ...); no string allocation.
			size--
			s = s[i+2:]
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	*n = requestStringSize(len(s))
	return nil
}

type requestPreview string

func (p *requestPreview) UnmarshalJSON(raw []byte) error {
	*p = ""
	if s, ok := requestStringBytes(raw); ok && bytes.IndexByte(s, '\\') < 0 {
		// A few bytes beyond the cap let TruncatePreview own rune boundaries
		// and the ellipsis without allocating an entire large prompt string.
		s = s[:min(len(s), metrics.PreviewMaxBytes+utf8.UTFMax)]
		*p = requestPreview(metrics.TruncatePreview(string(s)))
	} else if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			*p = requestPreview(metrics.TruncatePreview(s))
		}
	}
	return nil
}
