package format

// Regression tests for the translated-frame output bound. The translator
// accepts source lines up to sseLineBufMax and re-encodes their decoded or
// retained strings inside the OpenAI chunk envelope. Retained control-byte
// id/model values are echoed into every chunk, a raw U+2028 expands from three
// bytes to six, and an invalid UTF-8 byte becomes the three-byte U+FFFD
// replacement. NativeStreamFrameMaxBytes must cover those expansions so the
// proxy's translated relay scanner never rejects an accepted source frame.

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	frameContentPrefix = `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`
	frameContentSuffix = `"}}`
)

// TestNativeStreamFrameMaxBytesCoversWorstCase builds source lines at the
// translator's accepted input bound for each expansion path and proves every
// emitted data line fits the exported bound. The largest line must also exceed
// the former 2x source-line estimate, pinning that this bound is not a
// restatement of the bug.
func TestNativeStreamFrameMaxBytesCoversWorstCase(t *testing.T) {
	contentLen := sseLineBufMax - 1 - len(frameContentPrefix) - len(frameContentSuffix)
	escapedIdentity := strings.Repeat(`\u0001`, nativeIdentityMaxBytes)
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"` + escapedIdentity +
			`","model":"` + escapedIdentity + `","usage":{"input_tokens":4}}}`,
		frameContentPrefix + strings.Repeat("\u2028", contentLen/3) + frameContentSuffix,
		frameContentPrefix + strings.Repeat("\xff", contentLen) + frameContentSuffix,
		frameContentPrefix + strings.Repeat("<", contentLen) + frameContentSuffix,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatalf("StreamToOpenAI rejected an accepted source frame: %v", err)
	}

	oldCap := 2*sseLineBufMax + 2*1024
	maxLine := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if len(line) > maxLine {
			maxLine = len(line)
		}
		if line == "data: [DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("translated data line is not JSON: %v", err)
		}
	}
	if maxLine > NativeStreamFrameMaxBytes {
		t.Fatalf("largest translated line = %d bytes, want <= NativeStreamFrameMaxBytes (%d)",
			maxLine, NativeStreamFrameMaxBytes)
	}
	if maxLine <= oldCap {
		t.Fatalf("largest translated line = %d bytes, want > former cap %d to exercise the expansion bound",
			maxLine, oldCap)
	}
	t.Logf("translated frame: source=%d bytes maxLine=%d bound=%d former=%d",
		sseLineBufMax, maxLine, NativeStreamFrameMaxBytes, oldCap)
}

// TestStreamToOpenAIInvalidUTF8Replacement pins the invalid-UTF-8 arm
// separately from the control-identity expansion: the standard JSON decoder
// replaces each invalid byte with U+FFFD, so the re-encoded content is valid
// UTF-8 and the emitted frame stays parseable JSON.
func TestStreamToOpenAIInvalidUTF8Replacement(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		frameContentPrefix + "ok" + invalid + "!" + frameContentSuffix,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatalf("StreamToOpenAI = %v", err)
	}
	translated := out.String()
	if !utf8.ValidString(translated) {
		t.Fatal("translated stream still carries invalid UTF-8 bytes")
	}
	want := "ok" + strings.Repeat("\uFFFD", len(invalid)) + "!"
	for _, line := range strings.Split(translated, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("translated data line is not JSON: %v", err)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == want {
				return
			}
		}
	}
	t.Fatalf("translated content = %q, want %q", translated, want)
}
