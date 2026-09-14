package adminjson

import (
	"encoding/json"
	"net/http"
)

// flatError is the one operator-plane error body shape: {"error": "..."}.
type flatError struct {
	Error string `json:"error"`
}

// WriteError writes the flat operator-plane error body with the given status.
// The message is marshaled by encoding/json, never spliced into a string
// literal, so a cause containing quotes, backslashes, newlines or any other
// metacharacter can never produce malformed JSON. It keeps http.Error's exact
// observable shape (Content-Type text/plain, nosniff, trailing newline) so
// call sites converted from hand-written literals stay byte-identical for
// static messages.
func WriteError(w http.ResponseWriter, status int, msg string) {
	// Marshal of a struct with one string field cannot fail.
	b, _ := json.Marshal(flatError{Error: msg})
	http.Error(w, string(b), status)
}
