package adminjson

import (
	"encoding/json"
	"net/http"
)

// flatError is the one operator-plane error body shape: {"error": "..."}.
type flatError struct {
	Error string `json:"error"`
}

// ErrorBody marshals the flat operator-plane error body. It is the single
// encoding owner every operator error surface renders through: the message
// is marshaled by encoding/json, never spliced into a string literal, so a
// cause containing quotes, backslashes, newlines or any other metacharacter
// can never produce malformed JSON. Marshal of a struct with one string
// field cannot fail.
func ErrorBody(msg string) []byte {
	b, _ := json.Marshal(flatError{Error: msg})
	return b
}

// WriteError writes the flat operator-plane error body with the given status.
// It keeps http.Error's exact observable shape (Content-Type text/plain,
// nosniff, trailing newline) so call sites converted from hand-written
// literals stay byte-identical for static messages. This is the transport
// for the operator mutation/action surfaces (settings, pause, debug,
// throttle, aggregate JSON).
func WriteError(w http.ResponseWriter, status int, msg string) {
	http.Error(w, string(ErrorBody(msg)), status)
}

// WriteErrorJSON is the sanctioned transport variant for the operator
// surfaces whose error contract is an application/json body: cmd/proxy's
// /metrics and /admin log/backup routes (export, purge, purge/count, backup,
// restore), storage's /metrics/query (the bounded SELECT reader), which
// answered application/json before the body gained one marshaling owner,
// and the /healthz method gate, whose hand-spliced JSON body http.Error
// previously mislabeled as text/plain. Same shape, same
// single encoding owner (ErrorBody); the Content-Type is the only difference
// from WriteError. The trailing newline matches json.Encoder's, which these
// routes used previously.
//
// Nosniff position, decided once here so call sites do not re-derive it:
// this transport deliberately does not set X-Content-Type-Options. The body
// is application/json from the single encoding owner, which browsers do
// not sniff-execute, and the JSON operator routes never sent the header on
// their success paths - setting it only on errors at one site would be a
// per-caller patch over a shared transport (WriteError keeps it only
// because http.Error always set it). Adding nosniff here means changing the
// pinned /metrics, /admin and /healthz error contracts together, as one
// owner-wide decision.
func WriteErrorJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(ErrorBody(msg), '\n'))
}
