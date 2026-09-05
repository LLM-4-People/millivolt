package metrics

import "encoding/json"

func jsonSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

// JSONKey locates the first named JSON key at any nesting depth, returning
// the input tail beginning at its value. The result aliases data. String
// values are skipped, key escapes and all JSON whitespace are accepted, and
// no allocation is needed for ordinary unescaped keys. This is a lexical
// locator, not a replacement for a caller's authoritative JSON validation.
func JSONKey(data []byte, key string) []byte {
	for i := 0; i < len(data); {
		if data[i] != '"' {
			i++
			continue
		}
		start := i
		i++
		escaped := false
		for i < len(data) && data[i] != '"' {
			if data[i] == '\\' {
				escaped = true
				i++
			}
			i++
		}
		if i >= len(data) {
			return nil
		}
		end := i
		i++
		j := i
		for j < len(data) && jsonSpace(data[j]) {
			j++
		}
		if j >= len(data) || data[j] != ':' {
			continue
		}
		match := string(data[start+1:end]) == key
		if escaped {
			var name string
			match = json.Unmarshal(data[start:end+1], &name) == nil && name == key
		}
		if !match {
			continue
		}
		j++
		for j < len(data) && jsonSpace(data[j]) {
			j++
		}
		if j < len(data) {
			return data[j:]
		}
		return nil
	}
	return nil
}
