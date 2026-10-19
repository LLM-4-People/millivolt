package format

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// wireHTMLLiteral is content that encoding/json's default HTML escaping would
// expand six-fold (<, > and & each become a five-byte \u00XX sequence plus the
// backslash; the exact expansion is 6 bytes per input byte).
func wireHTMLLiteral(n int) string {
	unit := "<&>"
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// TestTranslateRequestDoesNotHTMLEscapeProviderContent pins the translated
// request wire encoding: literal <, > and & stay one byte each and remain
// valid JSON. The default json.Marshal escaping would inflate this body about
// six-fold before the proxy's max_request_bytes check.
func TestTranslateRequestDoesNotHTMLEscapeProviderContent(t *testing.T) {
	content := wireHTMLLiteral(24 << 10)
	quoted, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(`{"model":"m","messages":[{"role":"user","content":` + string(quoted) + `}]}`)

	out, err := TranslateRequest(in, 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`\u003c`)) || bytes.Contains(out, []byte(`\u003e`)) || bytes.Contains(out, []byte(`\u0026`)) {
		t.Fatalf("translated request still HTML-escapes: first bytes %q", out[:min(len(out), 256)])
	}
	var got struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated request is not valid JSON: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != content {
		t.Fatalf("translated content changed: got %d bytes, want %d", len(got.Messages[0].Content), len(content))
	}
	if limit := len(in) + 1024; len(out) > limit {
		t.Fatalf("translated request = %d bytes for a %d-byte input, want <= %d (no HTML expansion)", len(out), len(in), limit)
	}
}

// TestTranslateResponseDoesNotHTMLEscapeProviderContent is the response-side
// twin: the translated completion carries the provider's literal characters,
// stays valid JSON, and costs about its input size instead of six times it.
func TestTranslateResponseDoesNotHTMLEscapeProviderContent(t *testing.T) {
	content := wireHTMLLiteral(24 << 10)
	quoted, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(`{"id":"m","model":"m","content":[{"type":"text","text":` + string(quoted) +
		`}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)

	out, err := TranslateResponse(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`\u003c`)) || bytes.Contains(out, []byte(`\u003e`)) || bytes.Contains(out, []byte(`\u0026`)) {
		t.Fatalf("translated response still HTML-escapes: first bytes %q", out[:min(len(out), 256)])
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("translated response is not valid JSON: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != content {
		t.Fatalf("translated content changed: got %d bytes, want %d", len(got.Choices[0].Message.Content), len(content))
	}
	if limit := len(in) + 1024; len(out) > limit {
		t.Fatalf("translated response = %d bytes for a %d-byte input, want <= %d (no HTML expansion)", len(out), len(in), limit)
	}
}

// TestTranslateWireEncodingNormalControl guards the ordinary path against a
// rewrite that changes key order, quoting, or JSON semantics.
func TestTranslateWireEncodingNormalControl(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":9}`)
	out, err := TranslateRequest(in, 64, "")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model    string `json:"model"`
		MaxToken int    `json:"max_tokens"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.MaxToken != 9 || len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Fatalf("normal translation changed: %s", out)
	}
}
