package sse

import "testing"

// The frame primitives are the single owner of the proxy's client-facing SSE
// wire bytes. These tests pin the exact bytes so a change to the envelope,
// the frame wrapper, or the terminal marker can never drift silently: the
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
