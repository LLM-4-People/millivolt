package proxy

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// requestMessagesCount is the result of one walk over a messages array. over
// names the bound the walk crossed - "messages", "content parts" or "tool
// calls" - so the caller words the shared rejection; ok is false on malformed
// input. partsOver stops the walk because the aggregate content-part bound is
// the first rejection the former separate walks reported, and callsOver only
// defers the report to the end of the array so a later content overflow still
// wins exactly as it did when the content walk ran first.
type requestMessagesCount struct {
	elements  int
	parts     int
	calls     int
	next      int
	over      string
	partsOver bool
	callsOver bool
	ok        bool
}

// countRequestMessagesNested is the single walk over the messages array at
// start. It counts the array elements and, per message object, the content-part
// array lengths, the tool_calls members and the deprecated function_call. It
// replaces a separate array count plus one walk per counted collection, so a
// message's text is scanned once rather than three times. Every duplicate
// occurrence is charged because each one reaches a decoder. elementMax,
// partBudget and callBudget are the remaining allowances; a count above one of
// them sets over and stops the walk. Non-object elements and non-array values
// contribute zero, matching the typed decoder's shape tolerance. ok is false on
// malformed input so the document keeps its passthrough behavior.
func countRequestMessagesNested(data []byte, start, elementMax, partBudget, callBudget int) requestMessagesCount {
	var out requestMessagesCount
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '[' {
		out.next = skipJSONValue(data, i)
		out.ok = out.next >= 0
		return out
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != ']' {
		message := countRequestMessageNested(data, i, partBudget-out.parts, callBudget-out.calls)
		if !message.ok {
			return requestMessagesCount{}
		}
		out.elements++
		out.parts += message.parts
		out.calls += message.calls
		out.callsOver = out.callsOver || message.callsOver
		switch {
		case out.elements > elementMax:
			out.over = "messages"
			out.ok = true
			return out
		case message.partsOver:
			out.over = "content parts"
			out.ok = true
			return out
		}
		i = metrics.SkipSpace(data, message.next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return requestMessagesCount{}
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return requestMessagesCount{}
	}
	if i >= len(data) || data[i] != ']' {
		return requestMessagesCount{}
	}
	out.next, out.ok = i+1, true
	if out.callsOver {
		out.over = "tool calls"
	}
	return out
}

// countRequestMessageNested counts one message object's content-part arrays and
// tool-call members in one pass over its members. Duplicate content and
// tool_calls members each reach requestContent.UnmarshalJSON and a typed call
// slice before encoding/json's last-wins field assignment, so every occurrence
// is charged against the budget; a non-array content or tool_calls value
// contributes zero. A content overflow stops the walk (partsOver); a tool-call
// overflow only marks the result, so a content member after it is still counted
// and keeps the content rejection's precedence.
func countRequestMessageNested(data []byte, start, partBudget, callBudget int) requestMessagesCount {
	var out requestMessagesCount
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return out
	}
	if data[i] != '{' {
		out.next = skipJSONValue(data, i)
		out.ok = out.next >= 0
		return out
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return requestMessagesCount{}
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return requestMessagesCount{}
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return requestMessagesCount{}
		}
		switch {
		case requestKeyEqual(key, "content"):
			n, next, ok := metrics.CountJSONArrayBytes(data, i, partBudget-out.parts)
			if !ok {
				return requestMessagesCount{}
			}
			out.parts += n
			if out.parts > partBudget {
				out.partsOver, out.ok = true, true
				return out
			}
			i = next
		case requestKeyEqual(key, "tool_calls"):
			// The remaining call budget may already be exhausted, so the bound
			// clamps at zero: the array is still walked, but counting can stop
			// at the first member.
			limit := max(callBudget-out.calls, 0)
			n, next, ok := metrics.CountJSONArrayBytes(data, i, limit)
			if !ok {
				return requestMessagesCount{}
			}
			out.calls += n
			if out.calls > callBudget {
				out.callsOver = true
			}
			if n > limit {
				// The bounded count stopped at the member that proved the
				// overflow, so its index is not the array end. Skip the rest of
				// the array to keep walking the message and its later members.
				if next = skipJSONValue(data, i); next < 0 {
					return requestMessagesCount{}
				}
			}
			i = next
		case requestKeyEqual(key, "function_call"):
			// The deprecated one-call shape materializes a typed call like a
			// tool_calls member. A non-null value counts even if malformed,
			// because the decode still allocates it before rejecting the body.
			next := skipJSONValue(data, i)
			if next < 0 {
				return requestMessagesCount{}
			}
			if !bytes.Equal(bytes.TrimSpace(data[i:next]), []byte("null")) {
				out.calls++
				if out.calls > callBudget {
					out.callsOver = true
				}
			}
			i = next
		default:
			next := skipJSONValue(data, i)
			if next < 0 {
				return requestMessagesCount{}
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
		return requestMessagesCount{}
	}
	if i >= len(data) || data[i] != '}' {
		return requestMessagesCount{}
	}
	out.next, out.ok = i+1, true
	return out
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
