package sse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The frame primitives are the single owner of the proxy's client-facing
// OpenAI-compatible SSE data frames (envelope, frame wrapper, terminal
// marker). These tests pin the exact bytes so a change to the envelope, the
// frame wrapper, or the terminal marker can never drift silently: the
// values below are the bytes clients see today.

func TestDoneFrameBytes(t *testing.T) {
	if DoneFrame != "data: [DONE]\n\n" {
		t.Fatalf("DoneFrame = %q, want the exact terminal frame", DoneFrame)
	}
}

func TestDataFrameBytes(t *testing.T) {
	if got := string(DataFrame([]byte(`{"a":1}`))); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("DataFrame = %q, want the exact data frame", got)
	}
	if got := string(DataFrame(nil)); got != "data: \n\n" {
		t.Fatalf("DataFrame(nil) = %q, want the empty-payload frame", got)
	}
}

func TestErrorEnvelopeBytes(t *testing.T) {
	b, err := ErrorEnvelope("req-1", "stream_read_error", nil, "boom")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"code":null,"message":"boom","param":null,"type":"stream_read_error"},"id":"req-1"}`
	if string(b) != want {
		t.Fatalf("envelope = %s, want %s", b, want)
	}
	// The cursor void turn: no root id, a string code.
	b, err = ErrorEnvelope("", "upstream_error", "empty_turn", "m")
	if err != nil {
		t.Fatal(err)
	}
	want = `{"error":{"code":"empty_turn","message":"m","param":null,"type":"upstream_error"}}`
	if string(b) != want {
		t.Fatalf("envelope = %s, want %s", b, want)
	}
}

func TestDataPayload(t *testing.T) {
	cases := []struct {
		line    string
		payload string
		ok      bool
	}{
		{"data: {\"a\":1}", `{"a":1}`, true},
		{"data:{\"a\":1}", `{"a":1}`, true},
		{"data:  x", " x", true}, // one optional space only
		{"data: [DONE]\r", "[DONE]\r", true},
		{"data: ", "", true},
		{"data:", "", true},
		{"data", "", false},     // never match on a short prefix
		{"d", "", false},        // never match on a lone 'd'
		{"event: x", "", false}, // non-data lines are not payloads
		{"", "", false},
	}
	for _, c := range cases {
		payload, ok := DataPayload([]byte(c.line))
		if ok != c.ok || (ok && string(payload) != c.payload) {
			t.Fatalf("DataPayload(%q) = (%q, %v), want (%q, %v)", c.line, payload, ok, c.payload, c.ok)
		}
	}
}

// The marshal-failure arm of the emit pipeline: an unmarshalable payload
// surfaces the marshal error and writes nothing - no empty or partial frame
// ever reaches the wire.
func TestEmitFrameMarshalFailureWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitFrame(&buf, nil, map[string]any{"x": make(chan int)}); err == nil {
		t.Fatal("unmarshalable payload: EmitFrame returned nil, want the marshal error")
	}
	if buf.Len() != 0 {
		t.Fatalf("buffer = %q, want nothing written on marshal failure", buf.String())
	}
}

// The nil-flusher arm of EmitFrame, pinned directly (the emitters otherwise
// cover it only transitively through recorders that always flush): a valid
// payload with a nil flusher writes the frame bytes to the writer, attempts
// no flush, never panics, and returns nil.
func TestEmitFrameNilFlusherWritesFrameWithoutFlush(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitFrame(&buf, nil, map[string]any{"a": 1}); err != nil {
		t.Fatalf("EmitFrame with a nil flusher = %v, want nil error", err)
	}
	if got := buf.String(); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("buffer = %q, want the exact data frame", got)
	}
}

// TestEmitFrameDoesNotHTMLEscape pins the wire encoding: the emitter must
// carry <, > and & literally (one byte each), because encoding/json's default
// escaping inflates them to six bytes and can push an otherwise valid
// translated content frame past the relay's frame cap. The payload stays
// valid JSON and decodes to the original value.
func TestEmitFrameDoesNotHTMLEscape(t *testing.T) {
	obj := map[string]any{
		"id": "req<&>",
		"choices": []map[string]any{{
			"delta": map[string]any{"content": "<tag> & done"},
		}},
	}
	var buf bytes.Buffer
	if err := EmitFrame(&buf, nil, obj); err != nil {
		t.Fatalf("EmitFrame = %v", err)
	}
	frame := buf.String()
	if !bytes.HasPrefix([]byte(frame), []byte("data: ")) || !bytes.HasSuffix([]byte(frame), []byte("\n\n")) {
		t.Fatalf("frame envelope changed: %q", frame)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(frame, "data: "), "\n\n")
	if strings.Contains(payload, `\u003c`) || strings.Contains(payload, `\u003e`) || strings.Contains(payload, `\u0026`) {
		t.Fatalf("payload still HTML-escapes: %q", payload)
	}
	if !strings.Contains(payload, `<tag> & done`) {
		t.Fatalf("payload lost literal HTML characters: %q", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%q)", err, payload)
	}
}

// TestEmitFramePreservesNormalJSON pins that omitting HTML escaping changes
// nothing else: for a payload without <, > or &, the emitted payload is
// byte-for-byte encoding/json's Marshal output (map order included).
func TestEmitFramePreservesNormalJSON(t *testing.T) {
	obj := map[string]any{
		"id":      "chat-1",
		"created": int64(1700000000),
		"ok":      true,
		"none":    nil,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": nil,
			"delta":         map[string]any{"content": "plain text\nwith \"quotes\" and unicode é"},
		}},
	}
	want, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := EmitFrame(&buf, nil, obj); err != nil {
		t.Fatalf("EmitFrame = %v", err)
	}
	if got, expected := buf.String(), string(DataFrame(want)); got != expected {
		t.Fatalf("frame = %q, want exact json.Marshal payload %q", got, expected)
	}
}
