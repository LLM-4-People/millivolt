package format

// Regression test for the configurable event bound: a source line below the
// configured maximum relays, and a line above it fails closed instead of
// growing the scanner.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatChunkWithContent(n int) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("x", n) + `"},"finish_reason":null}]}` + "\n\n"
}

func TestStreamEventMaxConfiguredBound(t *testing.T) {
	var ok bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Chat, &ok, strings.NewReader(chatChunkWithContent(128)+"data: [DONE]\n\n"),
		nil, nil, StreamOptions{EventMaxBytes: 4096}); err != nil {
		t.Fatalf("line below the configured event max rejected: %v", err)
	}
	var over bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Chat, &over, strings.NewReader(chatChunkWithContent(8192)+"data: [DONE]\n\n"),
		nil, nil, StreamOptions{EventMaxBytes: 4096})
	if err == nil {
		t.Fatal("line above the configured event max accepted")
	}
	if strings.Contains(over.String(), "data: [DONE]") {
		t.Fatalf("over-bound stream reached a clean terminal: %s", over.String())
	}
}
