package format

import (
	"encoding/json"
	"errors"
	"testing"
)

// boundedContentAllocationMax is the hard byte ceiling for one bounded
// content-part decode. A typed decode of a million empty parts allocates
// hundreds of MiB; the lexical bound must keep the whole consumer well below
// the body's own footprint.
const boundedContentAllocationMax = 16 << 20

// heavyContentParts builds one JSON content array of n empty objects, the
// cheapest shape for amplifying a small body into a large typed slice.
func heavyContentParts(n int) json.RawMessage {
	body := make([]byte, 0, n*3+1)
	body = append(body, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '{', '}')
	}
	body = append(body, ']')
	return body
}

// TestContentTextBoundsPartArrays pins the normal content-array behavior and
// proves an over-bound array is skipped without materializing its parts.
func TestContentTextBoundsPartArrays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{"plain string", `"hello"`, "hello", true},
		{"text parts join", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\n\nb", true},
		{"mixed parts keep only text", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]`, "a", true},
		{"non-text parts yield no text", `[{"type":"image_url","image_url":{"url":"x"}}]`, "", false},
		{"empty array yields no text", `[]`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := openaiMessage{Content: json.RawMessage(tc.content)}
			got, ok := msg.ContentText()
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ContentText() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}

	heavy := heavyContentParts(1_000_000)
	msg := openaiMessage{Content: heavy}
	var got string
	var ok bool
	alloc := allocDelta(func() { got, ok = msg.ContentText() })
	if got != "" || ok {
		t.Fatalf("over-bound ContentText() = (%q, %v), want empty best-effort skip", got, ok)
	}
	if alloc > boundedContentAllocationMax {
		t.Fatalf("ContentText allocated %d bytes for %d-byte over-bound array, want <= %d",
			alloc, len(heavy), boundedContentAllocationMax)
	}
	t.Logf("ContentText over-bound: input=%d alloc=%d", len(heavy), alloc)
}

// TestTranslateRequestSkipsOverBoundContentBestEffort proves an over-bound
// composition array does not reject the whole Anthropic translation: the
// system fragment is skipped while the adjacent normal user turn survives.
func TestTranslateRequestSkipsOverBoundContentBestEffort(t *testing.T) {
	heavy := string(heavyContentParts(1_000_000))
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` + heavy + `},{"role":"user","content":"hi"}]}`)
	out, err := TranslateRequest(body, 64, "")
	if err != nil {
		t.Fatalf("over-bound system content rejected the translation: %v", err)
	}
	var got struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.System != "" {
		t.Fatalf("over-bound system content = %q, want skipped", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hi" {
		t.Fatalf("translated messages = %#v, want only the normal user turn", got.Messages)
	}
}

// TestTranslateResponseRejectsOverBoundContentBlocks closes the response-side
// twin: an upstream body carrying a million empty blocks fails closed before
// the typed block slice is materialized.
func TestTranslateResponseRejectsOverBoundContentBlocks(t *testing.T) {
	body := append([]byte(`{"id":"m","model":"m","content":`), heavyContentParts(1_000_000)...)
	body = append(body, []byte(`,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)...)

	var out []byte
	var err error
	alloc := allocDelta(func() { out, err = TranslateResponse(body) })
	if !errors.Is(err, errNativeContentParts) {
		t.Fatalf("TranslateResponse over-bound error = %v, want %v", err, errNativeContentParts)
	}
	if out != nil {
		t.Fatalf("TranslateResponse over-bound output = %d bytes, want fail-closed nil", len(out))
	}
	if alloc > boundedContentAllocationMax {
		t.Fatalf("TranslateResponse allocated %d bytes for a %d-byte over-bound body, want <= %d",
			alloc, len(body), boundedContentAllocationMax)
	}
	t.Logf("TranslateResponse over-bound: input=%d alloc=%d", len(body), alloc)
}

// TestCursorMessageTextBoundsPartArrays pins the normal extraction shapes and
// proves the Cursor history/action extractor also skips an over-bound array.
func TestCursorMessageTextBoundsPartArrays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"plain string", `"hello"`, "hello"},
		{"text parts join", `[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`, "part one part two"},
		{"multimodal keeps text and labels others", `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"x"}}]`, "look[image_url]"},
		{"empty array", `[]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := openaiMessage{Content: json.RawMessage(tc.content)}
			if got := cursorMessageText(msg); got != tc.want {
				t.Fatalf("cursorMessageText() = %q, want %q", got, tc.want)
			}
		})
	}

	heavy := heavyContentParts(1_000_000)
	msg := openaiMessage{Content: heavy}
	var got string
	alloc := allocDelta(func() { got = cursorMessageText(msg) })
	if got != "" {
		t.Fatalf("over-bound cursorMessageText() = %q, want empty best-effort skip", got)
	}
	if alloc > boundedContentAllocationMax {
		t.Fatalf("cursorMessageText allocated %d bytes for %d-byte over-bound array, want <= %d",
			alloc, len(heavy), boundedContentAllocationMax)
	}
	t.Logf("cursorMessageText over-bound: input=%d alloc=%d", len(heavy), alloc)
}
