package proxy

// Non-streaming Cursor output belongs to one accumulated-output budget. These
// regressions pin the boundary, the shared content/tool-argument accounting, and
// the byte-identical normal path.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

func cursorBudgetBytesField(field int, payload []byte) []byte {
	out := []byte{byte(field<<3 | 2)}
	out = binary.AppendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

func cursorBudgetVarintField(field int, value uint64) []byte {
	out := []byte{byte(field << 3)}
	return binary.AppendUvarint(out, value)
}

func cursorBudgetFrame(payload []byte) []byte {
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

// cursorBudgetTextStream builds an agent.v1 interaction_update text_delta
// stream, one frame per chunk, followed by turn_ended.
func cursorBudgetTextStream(chunks ...string) []byte {
	var frames bytes.Buffer
	for _, chunk := range chunks {
		text := cursorBudgetBytesField(1, []byte(chunk))
		update := cursorBudgetBytesField(1, text)
		frames.Write(cursorBudgetFrame(cursorBudgetBytesField(1, update)))
	}
	ended := cursorBudgetBytesField(14, nil)
	frames.Write(cursorBudgetFrame(cursorBudgetBytesField(1, ended)))
	return frames.Bytes()
}

func cursorBudgetRun(t *testing.T, frames []byte) *providerformat.CursorRun {
	t.Helper()
	run := providerformat.NewCursorRun(io.Discard, bytes.NewReader(frames), nil, func() {}, time.Hour)
	run.Start()
	t.Cleanup(run.Close)
	return run
}

// TestCursorJSONAccumulatorSharesContentAndToolBudget proves content and
// tool-call arguments draw from the same accumulator, and a rejected delta
// leaves no partial output behind.
func TestCursorJSONAccumulatorSharesContentAndToolBudget(t *testing.T) {
	acc := &cursorJSONAccumulator{max: 2048}
	if err := acc.add(map[string]any{"content": strings.Repeat("x", 1500)}); err != nil {
		t.Fatalf("below-budget content rejected: %v", err)
	}
	beforeBytes := acc.bytes
	err := acc.add(map[string]any{"tool_calls": []map[string]any{{
		"index": 0, "id": "call", "type": "function",
		"function": map[string]any{"name": "t", "arguments": strings.Repeat("a", 700)},
	}}})
	if !errors.Is(err, errCursorJSONOutputBytes) {
		t.Fatalf("tool arguments crossed the shared budget: err=%v", err)
	}
	if acc.bytes != beforeBytes {
		t.Fatalf("rejected delta changed accounted bytes: got %d, want %d", acc.bytes, beforeBytes)
	}
	if got := acc.content.String(); got != strings.Repeat("x", 1500) {
		t.Fatalf("rejected delta changed accumulated content: %d bytes", len(got))
	}
	if len(acc.toolCalls) != 0 {
		t.Fatalf("rejected delta retained %d tool calls", len(acc.toolCalls))
	}
}

// TestJSONStringSizeMatchesEncoder pins the accounting helper to the encoder's
// actual escaped length, including HTML escaping, control bytes, U+2028 and
// invalid UTF-8.
func TestJSONStringSizeMatchesEncoder(t *testing.T) {
	for _, s := range []string{
		"plain",
		`quote " backslash \`,
		"line\nreturn\rtab\tbell\bformfeed\f",
		"<tag> & value",
		"\u2028\u2029",
		"caf\u00e9 \u4e16\u754c",
		string([]byte{'a', 0xff, 'b'}),
	} {
		encoded, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cursorJSONStringSize(s), int64(len(encoded)-2); got != want {
			t.Fatalf("cursorJSONStringSize(%q) = %d, want %d (encoder wrote %d bytes)", s, got, want, len(encoded))
		}
	}
}

// TestCursorJSONNormalCompletionByteIdentical drives an 8 MiB normal answer
// through the real non-streaming path: it must remain a complete, exact
// completion rather than being clipped by the budget.
func TestCursorJSONNormalCompletionByteIdentical(t *testing.T) {
	const chunks = 8
	const chunkSize = 1 << 20
	budgetChunks := make([]string, chunks)
	for i := range budgetChunks {
		budgetChunks[i] = strings.Repeat("x", chunkSize)
	}
	run := cursorBudgetRun(t, cursorBudgetTextStream(budgetChunks...))
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	s.serveRunJSON(context.Background(), w, run, "id", rec, cursorTurnRender{model: "m"}, false, nil)

	if w.Code != 200 {
		t.Fatalf("normal completion status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var doc struct {
		ID      string         `json:"id"`
		Created json.Number    `json:"created"`
		Model   string         `json:"model"`
		Usage   map[string]any `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("normal completion is not valid JSON: %v", err)
	}
	if len(doc.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(doc.Choices))
	}
	content := strings.Repeat("x", chunks*chunkSize)
	if got := doc.Choices[0].Message.Content; got != content {
		t.Fatalf("normal completion content length = %d, want %d (budget must not clip a normal answer)", len(got), len(content))
	}
	created, err := doc.Created.Int64()
	if err != nil {
		t.Fatalf("created is not an integer: %v", err)
	}
	var want bytes.Buffer
	if err := json.NewEncoder(&want).Encode(sse.Completion(doc.ID, created, doc.Model,
		sse.AssistantMessage(content, nil), "stop", doc.Usage)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), want.Bytes()) {
		t.Fatalf("normal completion bytes changed: got %d bytes, want the byte-identical completion (%d bytes)",
			w.Body.Len(), want.Len())
	}
	if rec.ClientDisconnected || rec.ErrorType != "" || rec.StatusCode != 200 {
		t.Fatalf("normal completion record changed: %+v", rec)
	}
}

// TestCursorJSONOversizedStreamFailsBounded proves a fabricated 33 MiB stream
// stops at the accumulated-output budget and returns a small 502 envelope
// instead of buffering the whole turn.
func TestCursorJSONOversizedStreamFailsBounded(t *testing.T) {
	const chunks = 33
	oversized := make([]string, chunks)
	for i := range oversized {
		oversized[i] = strings.Repeat("x", 1<<20)
	}
	run := cursorBudgetRun(t, cursorBudgetTextStream(oversized...))
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	s.serveRunJSON(context.Background(), w, run, "id", rec, cursorTurnRender{model: "m"}, false, nil)

	if w.Code != 502 || rec.StatusCode != 502 {
		t.Fatalf("oversized turn status: client=%d record=%d, want 502/502", w.Code, rec.StatusCode)
	}
	if w.Body.Len() > 4096 {
		t.Fatalf("oversized turn body = %d bytes, want a bounded error envelope", w.Body.Len())
	}
	if !strings.Contains(w.Body.String(), errCursorJSONOutputBytes.Error()) {
		t.Fatalf("bounded body lost the budget reason: %s", w.Body.String())
	}
	if rec.ClientDisconnected {
		t.Fatal("an internal output budget must not be recorded as a client disconnect")
	}
}
