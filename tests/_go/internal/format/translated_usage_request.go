package format

// Round 5 A2 regression: a translated stream targeting Chat must request the
// usage chunk. Only a direct Chat client's stream_options describes that
// client's preference; Responses and Anthropic clients have no such field, and
// a compliant Chat upstream omits usage unless asked.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func translatedStreamOptions(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode translated request: %v (%s)", err, body)
	}
	return doc.StreamOptions
}

func TestTranslatedStreamRequestsUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound ir.Protocol
		body    string
	}{
		{
			name:    "responses",
			inbound: ir.Responses,
			body:    `{"model":"m","input":"hi","stream":true}`,
		},
		{
			name:    "anthropic",
			inbound: ir.Anthropic,
			body:    `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, _, err := TranslateRequestBetweenIR(tc.inbound, ir.Chat, []byte(tc.body), 0, "")
			if err != nil {
				t.Fatal(err)
			}
			opts := translatedStreamOptions(t, out)
			if opts["include_usage"] != true {
				t.Fatalf("stream_options = %v, want include_usage true: %s", opts, out)
			}
		})
	}
}

func TestTranslatedNonStreamRequestsNoUsageOption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound ir.Protocol
		body    string
	}{
		{
			name:    "responses",
			inbound: ir.Responses,
			body:    `{"model":"m","input":"hi"}`,
		},
		{
			name:    "anthropic",
			inbound: ir.Anthropic,
			body:    `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, _, err := TranslateRequestBetweenIR(tc.inbound, ir.Chat, []byte(tc.body), 0, "")
			if err != nil {
				t.Fatal(err)
			}
			if opts := translatedStreamOptions(t, out); opts != nil {
				t.Fatalf("non-stream request gained stream_options: %v (%s)", opts, out)
			}
		})
	}
}

func TestDirectChatKeepsUsagePreference(t *testing.T) {
	_, out, _, err := TranslateRequestBetweenIR(ir.Chat, ir.Chat,
		[]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if opts := translatedStreamOptions(t, out); opts != nil {
		t.Fatalf("direct Chat stream gained stream_options: %v (%s)", opts, out)
	}

	_, out, _, err = TranslateRequestBetweenIR(ir.Chat, ir.Chat,
		[]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if opts := translatedStreamOptions(t, out); opts["include_usage"] != true {
		t.Fatalf("direct Chat preference was lost: %v (%s)", opts, out)
	}
}
