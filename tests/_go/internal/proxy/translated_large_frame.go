package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestTranslatedStreamLargeHTMLLiteralContentRelays pins the translated SSE
// frame cap against encoding/json's HTML escaping. The fixture is a legitimate
// ~307 KiB content delta made of literal <, > and & bytes. Escaping each one
// to six bytes would inflate its translated frame past the 1 MiB scanner cap
// and turn a successful translation into an in-band translation_error; the
// frame emitter must carry the literal bytes so the chunk relays unchanged.
func TestTranslatedStreamLargeHTMLLiteralContentRelays(t *testing.T) {
	content := strings.Repeat("<&>", (307<<10)/3+1)[:307<<10]
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-large","model":"claude-test","usage":{"input_tokens":4}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + strconv.Quote(content) + `}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	upstream, srv, buf := translatedSSEFixture(t, native)

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200 (body prefix %q)", status, body[:min(len(body), 200)])
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 0 {
		t.Fatalf("large literal chunk emitted %d error frames: %q", got, body[:min(len(body), 400)])
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] frames = %d, want 1", got)
	}
	if strings.Contains(body, `\u003c`) || strings.Contains(body, `\u003e`) || strings.Contains(body, `\u0026`) {
		t.Fatal("translated stream still HTML-escapes its content")
	}
	if got := translatedStreamContent(t, body); got != content {
		t.Fatalf("translated content changed: got %d bytes, want %d", len(got), len(content))
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.IsError() || rec.ErrorType != "" || !rec.HadAnswerContent || rec.FinishReason != "stop" {
		t.Fatalf("record = %+v, want a healthy translated completion", rec)
	}
}

// TestTranslatedStreamFrameAtNativeLineCapRelays pins the other side of the
// bound: a source content line that fits the translator's 1 MiB input scanner
// is still a legitimate chunk, even though the OpenAI chunk envelope makes
// its translated frame slightly larger than that source line. The
// translated-frame cap must carry the envelope headroom instead of rejecting
// the frame and reporting a translation_error.
func TestTranslatedStreamFrameAtNativeLineCapRelays(t *testing.T) {
	const prefix = `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`
	const suffix = `"}}`
	contentLen := sseScannerBufMax - 1 - len(prefix) - len(suffix)
	content := strings.Repeat("<", contentLen)
	sourceLine := prefix + content + suffix
	if len(sourceLine) != sseScannerBufMax-1 {
		t.Fatalf("fixture source line = %d bytes, want %d", len(sourceLine), sseScannerBufMax-1)
	}
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-cap","model":"claude-test","usage":{"input_tokens":4}}}`,
		sourceLine,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	upstream, srv, buf := translatedSSEFixture(t, native)

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200", status)
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 0 {
		t.Fatalf("source-cap chunk emitted %d error frames", got)
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] frames = %d, want 1", got)
	}
	if got := translatedStreamContent(t, body); got != content {
		t.Fatalf("translated content changed: got %d bytes, want %d", len(got), len(content))
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.IsError() || rec.ErrorType != "" || !rec.HadAnswerContent {
		t.Fatalf("record = %+v, want a healthy translated completion", rec)
	}
}

// translatedStreamContent concatenates every content delta in a translated
// stream. The translator emits one delta per source chunk today; joining them
// keeps the assertion about the delivered bytes rather than one frame
// boundary if a future translator splits a large chunk.
func translatedStreamContent(t *testing.T, body string) string {
	t.Helper()
	var got strings.Builder
	for _, payload := range translatedSSEPayloads(t, body) {
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("translated frame is not JSON: %v", err)
		}
		for _, choice := range chunk.Choices {
			got.WriteString(choice.Delta.Content)
		}
	}
	return got.String()
}
