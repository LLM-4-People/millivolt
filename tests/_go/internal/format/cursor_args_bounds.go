package format

// Regression tests for cursorAssistantBlob's tool-call argument translation.
// The history blob can carry client-supplied arguments of any size, so a
// non-object argument must take the {_raw: raw} fallback without first
// materializing the whole value as nested Go maps/slices. A 1.5 MB array of
// empty objects expanded into tens of MiB that way; the request body is
// client-bounded, not translation-bounded.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// cursorToolArgAllocationMax is the hard allocation ceiling for translating one
// request whose assistant tool arguments are a large non-object array. The raw
// fallback necessarily copies the argument text and its JSON-escaped blob, but
// nothing may expand it into a per-element nested value.
const cursorToolArgAllocationMax = 8 << 20

// heavyToolCallArguments builds one JSON array of n empty objects: the cheapest
// shape for amplifying a small body into a large nested Go value.
func heavyToolCallArguments(n int) string {
	var b strings.Builder
	b.Grow(2*n + 1)
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	b.WriteByte(']')
	return b.String()
}

// assistantArgsJSON returns the translated JSON value of the first tool-call
// args in an assistant blob.
func assistantArgsJSON(t *testing.T, raw string) string {
	t.Helper()
	call := chatWireToolCall{ID: "c1"}
	call.Function.Name = "read"
	call.Function.Arguments = raw
	blob := cursorAssistantBlob("", []chatWireToolCall{call})
	b, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Content []struct {
			Args json.RawMessage `json:"args"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("blob is not JSON: %v", err)
	}
	if len(parsed.Content) != 1 {
		t.Fatalf("assistant blob content = %d parts, want 1", len(parsed.Content))
	}
	return string(parsed.Content[0].Args)
}

// TestCursorToolArgumentsPreserveShapes pins the documented args contract while
// the large-array path stays on the raw fallback: objects pass through with
// their values intact, and empty/null/malformed/non-object values normalize to
// {} or {_raw: raw}.
func TestCursorToolArgumentsPreserveShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     string
		want     string // exact translated args JSON for an accepted object
		fallback bool   // expect {_raw: args} instead
	}{
		{name: "empty", args: "", want: `{}`},
		{name: "literal null", args: "null", want: `{}`},
		{name: "whitespace null", args: " null ", want: `{}`},
		{name: "object", args: `{"path":"a.txt"}`, want: `{"path":"a.txt"}`},
		{
			name: "nested object keeps big numbers",
			args: `{"n":123456789012345678901234567890,"o":{"ok":true}}`,
			want: `{"n":123456789012345678901234567890,"o":{"ok":true}}`,
		},
		{name: "truncated object", args: `{"path":`, fallback: true},
		{name: "array", args: `[{"a":1}]`, fallback: true},
		{name: "number", args: `42`, fallback: true},
		{name: "string", args: `"path"`, fallback: true},
		{name: "literal", args: `true`, fallback: true},
		{name: "object with trailing garbage", args: `{"a":1} trailing`, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := assistantArgsJSON(t, tc.args)
			if !tc.fallback {
				if got != tc.want {
					t.Fatalf("args = %s, want %s", got, tc.want)
				}
				return
			}
			var fallback map[string]any
			if err := json.Unmarshal([]byte(got), &fallback); err != nil {
				t.Fatalf("args fallback is not a JSON object: %s", got)
			}
			if len(fallback) != 1 || fallback["_raw"] != strings.TrimSpace(tc.args) {
				t.Fatalf("args fallback = %s, want {_raw: %q}", got, strings.TrimSpace(tc.args))
			}
		})
	}
}

// TestCursorToolArgumentsLargeArrayBounded drives the full request translation
// with 500k empty objects in one tool-call argument. The non-object argument
// must reach the history blob as {_raw: raw} without decoding it into a
// per-element slice, so the translation stays within a small multiple of the
// body itself.
func TestCursorToolArgumentsLargeArrayBounded(t *testing.T) {
	huge := heavyToolCallArguments(500_000)
	quoted, err := json.Marshal(huge)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"run it"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":`)
	body = append(body, quoted...)
	body = append(body, []byte(`}}]},`+
		`{"role":"tool","tool_call_id":"c1","name":"read","content":"ok"}]}`)...)

	var (
		msg    []byte
		blobs  *KVBlobStore
		resume bool
		terr   error
	)
	alloc := allocDelta(func() {
		msg, blobs, resume, terr = TranslateCursorRunRequestWithConversation(body, "conv-large")
	})
	if terr != nil {
		t.Fatalf("translation failed: %v", terr)
	}
	if len(msg) == 0 {
		t.Fatal("translation produced no client message")
	}
	if !resume {
		t.Fatal("a tool-result tail must translate as a resume action")
	}
	if alloc > cursorToolArgAllocationMax {
		t.Fatalf("translation allocated %d bytes for %d-byte arguments (%d-byte body), want <= %d",
			alloc, len(huge), len(body), cursorToolArgAllocationMax)
	}
	if blobs == nil {
		t.Fatal("history blob store missing")
	}
	found := false
	blobs.mu.Lock()
	for _, b := range blobs.m {
		if bytes.Contains(b, []byte(`"args":{"_raw":"[{}`)) {
			found = true
			if !bytes.Contains(b, []byte(huge)) {
				t.Fatalf("history blob lost the raw argument text (blob %d bytes)", len(b))
			}
			break
		}
	}
	blobs.mu.Unlock()
	if !found {
		t.Fatal("history blob does not carry the {_raw: raw} fallback")
	}
	t.Logf("large tool args: input=%d body=%d alloc=%d", len(huge), len(body), alloc)
}
