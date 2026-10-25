package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
