package adminjson

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WriteError is the single owner of the flat operator-plane error body, so a
// cause carrying JSON metacharacters can never splice malformed JSON the way
// the old raw-interpolation and strconv.Quote call sites could. This is the
// pin for that bug class: every message must round-trip through a real JSON
// decode back to the exact input, and http.Error's observable shape (status,
// text/plain content type, nosniff, trailing newline) must not drift.
func TestWriteErrorRoundTripsArbitraryMessages(t *testing.T) {
	for _, msg := range []string{
		`quote " and backslash \ and newline
and tab	plus unicode caf é‾ ∧`,
		"",
		"plain static message",
	} {
		w := httptest.NewRecorder()
		WriteError(w, http.StatusBadRequest, msg)
		if w.Code != http.StatusBadRequest {
			t.Errorf("message %q: status = %d, want 400", msg, w.Code)
		}
		body := w.Body.String()
		if !strings.HasSuffix(body, "\n") {
			t.Errorf("message %q: body %q lost http.Error's trailing newline", msg, body)
		}
		var decoded struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("message %q: body is not valid JSON: %v (%q)", msg, err, body)
		}
		if decoded.Error != msg {
			t.Errorf("message did not round-trip: got %q want %q", decoded.Error, msg)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("content type = %q, want http.Error's text/plain", ct)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff header missing")
		}
	}
	// Converted fixed-literal sites must stay byte-identical: a static
	// message marshals to exactly the hand-written literal plus http.Error's
	// newline.
	w := httptest.NewRecorder()
	WriteError(w, http.StatusConflict, "pause hold not found")
	if w.Body.String() != "{\"error\":\"pause hold not found\"}\n" {
		t.Errorf("static body = %q, want the exact hand-written literal", w.Body.String())
	}
}
