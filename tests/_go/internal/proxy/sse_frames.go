package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// The proxy's in-band SSE error emission is byte-pinned end to end: these
// tests assert the exact frames an OpenAI-compatible client receives on the
// wire, so the shared owner (internal/sse) and every proxy site rendering
// through it must keep the bytes below unchanged. The values are the bytes
// the emitters produced when the frames owner was extracted.

func TestEmittedSSEErrorFramesAreByteExact(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := emitErrorSSE(rec, "req-1", "stream_read_error", "boom"); err != nil {
		t.Fatal(err)
	}
	want := "data: {\"error\":{\"code\":null,\"message\":\"boom\",\"param\":null,\"type\":\"stream_read_error\"},\"id\":\"req-1\"}\n\n" +
		"data: [DONE]\n\n"
	if rec.Body.String() != want {
		t.Fatalf("emitErrorSSE frames = %q, want %q", rec.Body.String(), want)
	}

	// Empty type defaults to upstream_error; no id means no root id field.
	rec2 := httptest.NewRecorder()
	if err := emitErrorSSE(rec2, "", "", "gone"); err != nil {
		t.Fatal(err)
	}
	want2 := "data: {\"error\":{\"code\":null,\"message\":\"gone\",\"param\":null,\"type\":\"upstream_error\"}}\n\n" +
		"data: [DONE]\n\n"
	if rec2.Body.String() != want2 {
		t.Fatalf("emitErrorSSE default-type frames = %q, want %q", rec2.Body.String(), want2)
	}
}

// TestEmittedDegenerateFramesAreByteExact also proves the analyzer observes
// exactly what the client saw: the record must carry the synthetic in-band
// failure, not a clean 200.
func TestEmittedDegenerateFramesAreByteExact(t *testing.T) {
	rec := httptest.NewRecorder()
	a := sse.Analyzer{}
	out := &metrics.Record{}
	emitDegenerateSSE(rec, &a, time.Unix(1700000000, 0), metrics.CodeTruncated, out)
	want := "data: {\"error\":{\"code\":\"truncated\",\"message\":\"stream ended without its completion marker - response is truncated\",\"param\":null,\"type\":\"upstream_error\"}}\n\n" +
		"data: [DONE]\n\n"
	if rec.Body.String() != want {
		t.Fatalf("degenerate frames = %q, want %q", rec.Body.String(), want)
	}
	a.Fill(out)
	if out.ErrorType != "upstream_error" || out.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record after degenerate emission = %s/%s, want the in-band truncated error",
			out.ErrorType, out.ErrorCode)
	}
}

// TestEmittedChunkFrameShape pins the frame wrapper of the ordinary chunk
// emitters (created timestamps vary per call, so the JSON is decoded rather
// than string-compared): exactly one "data: " frame per chunk, closed by the
// blank-line terminator.
func TestEmittedChunkFrameShape(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := usageChunk(rec, "req-a", "model-a", true, 10, 20, 5); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {") || !strings.HasSuffix(body, "\n\n") || strings.Count(body, "\n\n") != 1 {
		t.Fatalf("usage frame = %q, want one data frame with the blank-line terminator", body)
	}
	var chunk struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []any  `json:"choices"`
		Usage   struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			Details          struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")), &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.ID != "req-a" || chunk.Object != "chat.completion.chunk" || chunk.Model != "model-a" || len(chunk.Choices) != 0 {
		t.Fatalf("usage chunk = %+v", chunk)
	}
	if chunk.Usage.PromptTokens != 10 || chunk.Usage.CompletionTokens != 20 || chunk.Usage.TotalTokens != 30 ||
		chunk.Usage.Details.ReasoningTokens != 5 {
		t.Fatalf("usage chunk counts = %+v", chunk.Usage)
	}

	rec2 := httptest.NewRecorder()
	emit := sseEmitter(rec2, "id-1", "m", nil)
	if err := emit(map[string]any{"content": "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	body2 := rec2.Body.String()
	if !strings.HasPrefix(body2, "data: {") || !strings.HasSuffix(body2, "\n\n") || strings.Count(body2, "\n\n") != 1 {
		t.Fatalf("delta frame = %q, want one data frame with the blank-line terminator", body2)
	}
	var delta struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int            `json:"index"`
			Delta        map[string]any `json:"delta"`
			FinishReason any            `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(body2, "data: "), "\n\n")), &delta); err != nil {
		t.Fatal(err)
	}
	if delta.ID != "id-1" || delta.Model != "m" || len(delta.Choices) != 1 ||
		delta.Choices[0].Delta["content"] != "hi" || delta.Choices[0].FinishReason != nil {
		t.Fatalf("delta chunk = %+v", delta)
	}
}
