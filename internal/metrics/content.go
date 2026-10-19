package metrics

import "bytes"

// ContentPartMax bounds one message's content-part array before a translation
// or analysis consumer materializes the typed parts. Multimodal messages do
// not need thousands of parts; without this bound a body carrying millions of
// empty objects in one content array expands into hundreds of MiB of typed
// slices. Internal safety guardrail, not a wire-format or user-tunable limit.
const ContentPartMax = 4096

// ContentPartsWithinLimit reports whether a raw message content value can be
// decoded into a typed parts slice without exceeding ContentPartMax. A
// non-array value is accepted because the caller's decoder owns its shape; an
// array is counted lexically and the scan stops once the bound is exceeded,
// so an over-bound array costs one short prefix pass and no allocation. A
// caller must skip its typed decode when this returns false: content
// composition is best effort here, not a reason to reject the request.
func ContentPartsWithinLimit(raw []byte) bool {
	i := SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return true
	}
	n, _, ok := CountJSONArrayBytes(raw, i, ContentPartMax)
	return ok && n <= ContentPartMax
}

// CountJSONArrayBytes counts top-level members of a JSON array at i without
// materializing them. A non-array value is skipped and reported as zero so
// only actual collection overflows are rejected. Counting stops as soon as
// max is exceeded, so a hostile unbounded array costs only the prefix needed
// to prove the overflow. next is the index just past the value, or just past
// the member that proved the overflow when counting stops early. ok is false
// on malformed or unterminated input.
func CountJSONArrayBytes(data []byte, i, max int) (int, int, bool) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '[' {
		next := SkipJSONValue(data, i)
		return 0, next, next >= 0
	}
	i = SkipSpace(data, i+1)
	n := 0
	for i < len(data) && data[i] != ']' {
		next := SkipJSONValue(data, i)
		if next <= i {
			return 0, 0, false
		}
		n++
		if n > max {
			return n, next, true
		}
		i = SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, 0, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, 0, false
	}
	return n, i + 1, true
}

// skipJSONValue advances past one JSON value - a string, a nested object or
// array tracked as a bracket depth, or a bare number/literal - returning the
// index just past the value, or -1 when the bytes run out inside an
// unterminated structure. Lexical only: it never validates the value.
func SkipJSONValue(data []byte, i int) int {
	if i < 0 || i >= len(data) {
		return -1
	}
	switch data[i] {
	case '"':
		return SkipJSONString(data, i)
	case '{', '[':
		depth := 1
		i++
		for i < len(data) && depth > 0 {
			switch data[i] {
			case '"':
				next := SkipJSONString(data, i)
				if next < 0 {
					return -1
				}
				i = next - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
		}
		if depth != 0 {
			return -1
		}
		return i
	default:
		// A number or literal (on invalid input, garbage): the next member
		// separator or closing bracket ends the scalar.
		for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' {
			i++
		}
		return i
	}
}

// skipJSONString returns the index just past the closing quote of the JSON
// string opening at i, or -1 when the string is unterminated. The common
// unescaped body is one IndexByte search; only a string that actually contains
// a backslash falls back to the escape-aware byte loop.
func SkipJSONString(data []byte, i int) int {
	if i >= len(data) || data[i] != '"' {
		return -1
	}
	rest := data[i+1:]
	if j := bytes.IndexByte(rest, '"'); j >= 0 && bytes.IndexByte(rest[:j], '\\') < 0 {
		return i + 1 + j + 1
	}
	i++
	for i < len(data) {
		if data[i] == '\\' {
			i += 2
			continue
		}
		if data[i] == '"' {
			return i + 1
		}
		i++
	}
	return -1
}
