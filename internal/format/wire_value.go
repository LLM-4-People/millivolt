package format

import (
	"bytes"
	"encoding/json"
)

// wireValuePresent reports whether a raw wire member was supplied with a
// value other than null. Optional provider knobs whose target default may
// differ use it to decide whether a loss warning is due.
func wireValuePresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}
