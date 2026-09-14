package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The non-streaming cursor completion and the streaming usage chunk must
// render usage through the SAME owner (usageWire). OpenAI carries
// completion_tokens_details.reasoning_tokens on both surfaces; the
// non-streaming path once hand-rolled its usage map without the detail.
// This is the pin for that divergence: a finished turn with reasoning
// tokens must carry the detail on the single JSON completion.
func TestCursorNonStreamingUsageCarriesReasoningDetail(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	res := providerformat.TurnResult{Outcome: providerformat.TurnFinished, Prompt: 10, Output: 20, Reasoning: 5}
	rec := &metrics.Record{ID: "req-1", StatusCode: 200}
	w := httptest.NewRecorder()
	s.writeRunJSON(w, run, res, "hi", nil, rec, "req-1", cursorTurnRender{model: "m", est: 7}, false)

	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("completion response: %d %s", w.Code, w.Body.String())
	}
	var completion struct {
		Object string `json:"object"`
		Usage  struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			Details          struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil {
		t.Fatalf("completion body is not valid JSON: %v (%s)", err, w.Body.String())
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", completion.Object)
	}
	if completion.Usage.PromptTokens != 10 || completion.Usage.CompletionTokens != 20 || completion.Usage.TotalTokens != 30 {
		t.Fatalf("usage counts = %+v, want 10/20/30", completion.Usage)
	}
	if completion.Usage.Details.ReasoningTokens != 5 {
		t.Fatalf("completion_tokens_details.reasoning_tokens = %d, want 5 (the streaming path sends it; OpenAI carries it on both surfaces)",
			completion.Usage.Details.ReasoningTokens)
	}

	// The zero-reasoning turn omits the detail object on both surfaces.
	rec2 := &metrics.Record{ID: "req-2", StatusCode: 200}
	res2 := providerformat.TurnResult{Outcome: providerformat.TurnFinished, Prompt: 3, Output: 4}
	w2 := httptest.NewRecorder()
	s.writeRunJSON(w2, run, res2, "hi", nil, rec2, "req-2", cursorTurnRender{model: "m", est: 7}, false)
	if json.Unmarshal(w2.Body.Bytes(), &struct{}{}) != nil {
		t.Fatalf("second completion body is not valid JSON: %s", w2.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["usage"].(map[string]any)["completion_tokens_details"]; has {
		t.Fatalf("zero-reasoning completion must omit the detail object: %s", w2.Body.String())
	}
}
