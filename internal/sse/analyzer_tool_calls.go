package sse

import (
	"bytes"
	"encoding/json"

	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// toolCallFragment is one lexically extracted tool_calls array element. The
// start/end pairs bound the raw JSON token of the LAST occurrence of that key
// in the element (the struct decode's last-wins duplicate rule, where a null
// value is a no-op); a zero-length pair is an absent key. Offsets are relative
// to the scanned block, which keeps a hostile many-element frame's reusable
// buffer to five bytes per key instead of two slice headers. id records whether
// the last id occurrence was a non-empty string.
type toolCallFragment struct {
	indexStart, indexEnd uint32
	nameStart, nameEnd   uint32
	id                   bool
}

// feedToolCalls accounts one Chat tool_calls block. A well-formed
// array-of-objects block is scanned once, lexically: every element yields its
// index, id and function.name without the slice decode, the RawMessage copy or
// the per-element struct decode. Anything else - an unclosed array, a
// non-object element, a target key of the wrong type, an escaped object key -
// falls back to the exact typed decode, so the extracted shape can never
// diverge from encoding/json's verdict on the same block.
//
// A malformed index in one element must not discard its healthy siblings or
// turn the whole frame into an empty tool call. The index is an exact
// non-negative integer; 0.0 and 1e3 are valid, while a string, fraction,
// negative or out-of-range value falls back to id presence.
func (a *Analyzer) feedToolCalls(toolBlock []byte) {
	fragments, ok := scanToolCallFragments(toolBlock, a.toolCallScratch[:0])
	a.toolCallScratch = fragments[:0]
	if !ok {
		a.feedToolCallsDecoded(toolBlock)
		return
	}
	for _, fragment := range fragments {
		index := 0
		hasIndex := false
		if fragment.indexEnd > fragment.indexStart {
			if n, err := jsonnum.Parse(toolBlock[fragment.indexStart:fragment.indexEnd]); err == nil && n >= 0 {
				index, hasIndex = n, true
			}
		}
		name := toolBlock[fragment.nameStart:fragment.nameEnd]
		if !hasIndex {
			a.retainToolNameToken(name)
			if fragment.id {
				a.toolCalls++
			}
			continue
		}
		if _, counted := a.toolCallIndexes[index]; counted {
			a.retainToolNameToken(name)
			continue
		}
		// A fragment with neither a non-empty id nor a non-empty name is
		// not a call announcement.
		if !fragment.id && len(name) <= 2 {
			continue
		}
		if a.toolCallIndexes == nil {
			a.toolCallIndexes = make(map[int]struct{}, 4)
		}
		if len(a.toolCallIndexes) < metrics.RetainedToolCallIDsMax {
			a.toolCallIndexes[index] = struct{}{}
		}
		a.toolCalls++
		a.retainToolNameToken(name)
	}
}

// feedToolCallsDecoded is the typed decode fallback. Each element decodes
// independently so a malformed element never affects its siblings.
func (a *Analyzer) feedToolCallsDecoded(toolBlock []byte) {
	var calls []json.RawMessage
	if json.Unmarshal(toolBlock, &calls) != nil {
		return
	}
	for _, raw := range calls {
		var call struct {
			Index    json.RawMessage `json:"index"`
			ID       string          `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &call) != nil {
			continue
		}
		index := 0
		hasIndex := false
		if trimmed := bytes.TrimSpace(call.Index); len(trimmed) > 0 {
			if n, err := jsonnum.Parse(trimmed); err == nil && n >= 0 {
				index, hasIndex = n, true
			}
		}
		if !hasIndex {
			a.retainToolName(call.Function.Name)
			if call.ID != "" {
				a.toolCalls++
			}
			continue
		}
		if _, counted := a.toolCallIndexes[index]; counted {
			a.retainToolName(call.Function.Name)
			continue
		}
		if call.ID == "" && call.Function.Name == "" {
			continue
		}
		if a.toolCallIndexes == nil {
			a.toolCallIndexes = make(map[int]struct{}, 4)
		}
		if len(a.toolCallIndexes) < metrics.RetainedToolCallIDsMax {
			a.toolCallIndexes[index] = struct{}{}
		}
		a.toolCalls++
		a.retainToolName(call.Function.Name)
	}
}

// scanToolCallFragments validates a tool_calls block and extracts one
// fragment per element, appending to the caller's reusable buffer. ok reports
// whether the block had the exact array-of-objects JSON shape; on false the
// fragments must not be used.
func scanToolCallFragments(block []byte, fragments []toolCallFragment) ([]toolCallFragment, bool) {
	i := metrics.SkipSpace(block, 0)
	if i >= len(block) || block[i] != '[' {
		return fragments, false
	}
	i = metrics.SkipSpace(block, i+1)
	if i < len(block) && block[i] == ']' {
		i++
	} else {
		for {
			fragment, next, ok := scanToolCallFragment(block, i)
			if !ok {
				return fragments, false
			}
			fragments = append(fragments, fragment)
			if next >= len(block) {
				return fragments, false
			}
			switch block[next] {
			case ']':
				i = next + 1
			case ',':
				i = metrics.SkipSpace(block, next+1)
				continue
			default:
				return fragments, false
			}
			break
		}
	}
	return fragments, metrics.SkipSpace(block, i) == len(block)
}

// scanToolCallFragment extracts one element object starting at its '{'. The
// returned index is one past the closing '}'.
func scanToolCallFragment(block []byte, i int) (toolCallFragment, int, bool) {
	var fragment toolCallFragment
	i = metrics.SkipSpace(block, i)
	if i >= len(block) || block[i] != '{' {
		return fragment, 0, false
	}
	i = metrics.SkipSpace(block, i+1)
	if i < len(block) && block[i] == '}' {
		return fragment, i + 1, true
	}
	for {
		key, next, ok := scanToolKey(block, i)
		if !ok {
			return fragment, 0, false
		}
		i = next
		switch {
		case toolKeyIs(key, "index"):
			start := i
			next, ok = skipToolValue(block, i)
			if !ok {
				return fragment, 0, false
			}
			fragment.indexStart, fragment.indexEnd = uint32(start), uint32(next)
			i = next
		case toolKeyIs(key, "id"):
			switch {
			case i < len(block) && block[i] == '"':
				start := i
				next, ok = skipToolString(block, i)
				if !ok {
					return fragment, 0, false
				}
				fragment.id = next-start > 2
				i = next
			case hasToolLiteral(block, i, "null"):
				// encoding/json leaves a string field untouched for null.
				i += len("null")
			default:
				return fragment, 0, false
			}
		case toolKeyIs(key, "function"):
			switch {
			case i < len(block) && block[i] == '{':
				fragment.nameStart, fragment.nameEnd, next, ok =
					scanToolFunction(block, i, fragment.nameStart, fragment.nameEnd)
				if !ok {
					return fragment, 0, false
				}
				i = next
			case hasToolLiteral(block, i, "null"):
				// null is a no-op for the function struct and its name.
				i += len("null")
			default:
				return fragment, 0, false
			}
		default:
			next, ok = skipToolValue(block, i)
			if !ok {
				return fragment, 0, false
			}
			i = next
		}
		i = metrics.SkipSpace(block, i)
		if i >= len(block) {
			return fragment, 0, false
		}
		switch block[i] {
		case '}':
			return fragment, i + 1, true
		case ',':
			i = metrics.SkipSpace(block, i+1)
		default:
			return fragment, 0, false
		}
	}
}

// scanToolKey reads one object key at i and the colon after it, returning the
// unquoted key and the index just past the colon. An escaped key reports false:
// the lexical switch compares raw spellings, so decoding is left to the
// fallback.
func scanToolKey(block []byte, i int) ([]byte, int, bool) {
	if i >= len(block) || block[i] != '"' {
		return nil, 0, false
	}
	start := i
	i, ok := skipToolString(block, i)
	if !ok {
		return nil, 0, false
	}
	key := block[start+1 : i-1]
	if bytes.IndexByte(key, '\\') >= 0 {
		return nil, 0, false
	}
	i = metrics.SkipSpace(block, i)
	if i >= len(block) || block[i] != ':' {
		return nil, 0, false
	}
	return key, metrics.SkipSpace(block, i+1), true
}

// toolKeyIs matches one unquoted object key against a struct field name under
// encoding/json's ASCII case-insensitive tag rule, so "ID" and "iD" reach the
// same field as "id".
func toolKeyIs(key []byte, want string) bool {
	if len(key) != len(want) {
		return false
	}
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

// scanToolFunction extracts the name member of one function object starting at
// its '{', carrying the name offsets of an earlier duplicate function key:
// decoding a later object into the same struct merges into it, so an absent or
// null name keeps the previous one. The returned index is one past the closing
// '}'.
func scanToolFunction(block []byte, i int, nameStart, nameEnd uint32) (uint32, uint32, int, bool) {
	i = metrics.SkipSpace(block, i+1)
	if i < len(block) && block[i] == '}' {
		return nameStart, nameEnd, i + 1, true
	}
	for {
		key, next, ok := scanToolKey(block, i)
		if !ok {
			return 0, 0, 0, false
		}
		i = next
		if toolKeyIs(key, "name") {
			switch {
			case i < len(block) && block[i] == '"':
				start := i
				next, ok = skipToolString(block, i)
				if !ok {
					return 0, 0, 0, false
				}
				nameStart, nameEnd = uint32(start), uint32(next)
				i = next
			case hasToolLiteral(block, i, "null"):
				// null is a no-op for a string field.
				i += len("null")
			default:
				return 0, 0, 0, false
			}
		} else if i, ok = skipToolValue(block, i); !ok {
			return 0, 0, 0, false
		}
		i = metrics.SkipSpace(block, i)
		if i >= len(block) {
			return 0, 0, 0, false
		}
		switch block[i] {
		case '}':
			return nameStart, nameEnd, i + 1, true
		case ',':
			i = metrics.SkipSpace(block, i+1)
		default:
			return 0, 0, 0, false
		}
	}
}

// skipToolValue returns the index just past one complete JSON value starting
// at i, validating its syntax. It mirrors encoding/json's grammar: a raw
// control byte or bad escape inside a string, a leading-zero or otherwise
// malformed number, and a misspelled literal all report false.
func skipToolValue(block []byte, i int) (int, bool) {
	if i >= len(block) {
		return 0, false
	}
	switch block[i] {
	case '"':
		return skipToolString(block, i)
	case '{':
		return skipToolObject(block, i)
	case '[':
		return skipToolArray(block, i)
	case 't':
		return skipToolLiteral(block, i, "true")
	case 'f':
		return skipToolLiteral(block, i, "false")
	case 'n':
		return skipToolLiteral(block, i, "null")
	default:
		return skipToolNumber(block, i)
	}
}

// skipToolString returns the index just past the closing quote of the string
// starting at block[i].
func skipToolString(block []byte, i int) (int, bool) {
	for i++; i < len(block); i++ {
		switch c := block[i]; {
		case c == '"':
			return i + 1, true
		case c == '\\':
			i++
			if i >= len(block) {
				return 0, false
			}
			switch block[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if i+4 >= len(block) {
					return 0, false
				}
				for _, h := range block[i+1 : i+5] {
					if !isHexByte(h) {
						return 0, false
					}
				}
				i += 4
			default:
				return 0, false
			}
		case c < 0x20:
			return 0, false
		}
	}
	return 0, false
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// hasToolLiteral reports whether literal starts at block[i].
func hasToolLiteral(block []byte, i int, literal string) bool {
	return bytes.HasPrefix(block[i:], []byte(literal))
}

func skipToolLiteral(block []byte, i int, literal string) (int, bool) {
	if !hasToolLiteral(block, i, literal) {
		return 0, false
	}
	return i + len(literal), true
}

func skipToolNumber(block []byte, i int) (int, bool) {
	if i < len(block) && block[i] == '-' {
		i++
	}
	if i >= len(block) {
		return 0, false
	}
	switch {
	case block[i] == '0':
		i++
	case block[i] >= '1' && block[i] <= '9':
		for i < len(block) && block[i] >= '0' && block[i] <= '9' {
			i++
		}
	default:
		return 0, false
	}
	if i < len(block) && block[i] == '.' {
		i++
		if i >= len(block) || block[i] < '0' || block[i] > '9' {
			return 0, false
		}
		for i < len(block) && block[i] >= '0' && block[i] <= '9' {
			i++
		}
	}
	if i < len(block) && (block[i] == 'e' || block[i] == 'E') {
		i++
		if i < len(block) && (block[i] == '+' || block[i] == '-') {
			i++
		}
		if i >= len(block) || block[i] < '0' || block[i] > '9' {
			return 0, false
		}
		for i < len(block) && block[i] >= '0' && block[i] <= '9' {
			i++
		}
	}
	return i, true
}

func skipToolObject(block []byte, i int) (int, bool) {
	i = metrics.SkipSpace(block, i+1)
	if i < len(block) && block[i] == '}' {
		return i + 1, true
	}
	for {
		if i >= len(block) || block[i] != '"' {
			return 0, false
		}
		next, ok := skipToolString(block, i)
		if !ok {
			return 0, false
		}
		i = metrics.SkipSpace(block, next)
		if i >= len(block) || block[i] != ':' {
			return 0, false
		}
		next, ok = skipToolValue(block, metrics.SkipSpace(block, i+1))
		if !ok {
			return 0, false
		}
		i = metrics.SkipSpace(block, next)
		if i >= len(block) {
			return 0, false
		}
		switch block[i] {
		case '}':
			return i + 1, true
		case ',':
			i = metrics.SkipSpace(block, i+1)
		default:
			return 0, false
		}
	}
}

func skipToolArray(block []byte, i int) (int, bool) {
	i = metrics.SkipSpace(block, i+1)
	if i < len(block) && block[i] == ']' {
		return i + 1, true
	}
	for {
		next, ok := skipToolValue(block, i)
		if !ok {
			return 0, false
		}
		i = metrics.SkipSpace(block, next)
		if i >= len(block) {
			return 0, false
		}
		switch block[i] {
		case ']':
			return i + 1, true
		case ',':
			i = metrics.SkipSpace(block, i+1)
		default:
			return 0, false
		}
	}
}
