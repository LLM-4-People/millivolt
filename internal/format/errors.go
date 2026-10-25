package format

import (
	"bytes"
	"encoding/json"
)

// genericProviderErrorMessage is the local replacement for a non-empty
// provider error value whose members are all the wrong JSON shape. It is never
// presented as the provider's own text.
const genericProviderErrorMessage = "upstream provider error"

// errorValuePresent reports whether a raw error value carries any provider
// statement at all. The canonical parser returns no members for both a decoy
// and a structurally valid envelope whose members are all wrong-typed; only
// the latter is a real failure and must classify generically. Null, an empty
// object and an empty string are decoys; every other value is present.
func errorValuePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal(trimmed, &obj) != nil {
			return false
		}
		return len(obj) > 0
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return false
		}
		return s != ""
	default:
		return true
	}
}
