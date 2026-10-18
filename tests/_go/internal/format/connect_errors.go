package format

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestEndStreamErrorBoundsUpstreamText pins the Connect terminal-frame error
// construction: a normal provider message is unchanged, while a body-sized
// message (and code) is cut with the canonical preview truncator before it can
// become Record.ErrorMsg or a 502/SSE body.
func TestEndStreamErrorBoundsUpstreamText(t *testing.T) {
	normal := []byte(`{"error":{"code":"unavailable","message":"try later"}}`)
	if err := endStreamError(normal); err == nil || err.Error() != "connect upstream error (unavailable): try later" {
		t.Fatalf("normal end-stream error = %v", err)
	}

	hostile := strings.Repeat("x", 1<<20)
	payload := []byte(`{"error":{"code":"unavailable","message":"` + hostile + `"}}`)
	err := endStreamError(payload)
	if err == nil {
		t.Fatal("hostile end-stream message was accepted")
	}
	got := err.Error()
	if strings.Contains(got, hostile) {
		t.Fatal("end-stream error retained the hostile message")
	}
	if len(got) > 256 {
		t.Fatalf("end-stream error = %d bytes, want a bounded message", len(got))
	}
	want := metrics.TruncatePreview(hostile)
	if !strings.Contains(got, want) {
		t.Fatalf("end-stream error lost the bounded message: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("end-stream error = %q, want the truncation marker", got)
	}

	hostileCode := strings.Repeat("c", 1<<20)
	payload = []byte(`{"error":{"code":"` + hostileCode + `","message":"try later"}}`)
	err = endStreamError(payload)
	if err == nil || len(err.Error()) > 256 || strings.Contains(err.Error(), hostileCode) {
		t.Fatalf("hostile end-stream code = %v", err)
	}
}
