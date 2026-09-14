package proxy

// Pins for the cursor_helpers owners: writeSSEHeaders (the cursor surface of
// the shared metrics.SetStreamHeaders triple), flattenContent (the one
// content walker shared by the tool-result reader, the input estimate and
// the relay's non-streaming answer-content walk), and sseEmitter's
// per-chunk created stamping (the cursor bridge's policy - the Anthropic
// bridge pins one per stream instead).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The cursor stream's SSE headers come from the shared metrics.SetStreamHeaders
// owner via writeSSEHeaders. The pacer applies them through
// copyResponseHeaders, which strips Connection as hop-by-hop (RFC 7230 §6.1),
// so the observable cursor-surface triple is Content-Type + Cache-Control.
// This test exercises the real streaming path end to end: dropping either
// surviving header from the shared owner reddens here, not only on the
// dashboard feed surface.
func TestCursorStreamSSEHeadersFromSharedTriple(t *testing.T) {
	cfg := stormTestConfig()
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		readFrame(t, r.Body)
		return stormCursorReply(http.StatusOK, stormCursorAnswer()), nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"neutral-model","stream":true,"messages":[{"role":"user","content":"fixture"}]}`))
	r.Header.Set(hdrBaseURL, "http://neutral.invalid")
	r.Header.Set(hdrFormat, "cursor")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q (the shared SetStreamHeaders triple no longer reaches the cursor stream)", name, got, want)
		}
	}
}

// flattenContent owns one content walk for the tool-result reader, the
// input estimate and the relay's non-streaming answer-content walk (where
// HadAnswerContent and the gated response preview both read it). Rows: a
// bare string stays itself, a multi-part text array joins, a mixed array
// keeps only its text parts, and any other shape reports false.
func TestFlattenContentRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"bare string", `"plain"`, "plain", true},
		{"multi-part join", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab", true},
		{"mixed keeps text parts", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]`, "a", true},
		{"object is neither string nor parts", `{"text":"nope"}`, "", false},
		{"number is neither string nor parts", `42`, "", false},
		{"array of scalars is not a parts array", `[1,2]`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := flattenContent(json.RawMessage(tc.raw))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("flattenContent(%s) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The relay's non-streaming answer-content leg reads flattenContent too
// (HadAnswerContent and the gated response preview both derive from it): a
// mixed parts answer counts as content with only its text parts carrying
// text, and an all-non-text parts body carries no text at all.
func TestRelayAnswerContentWalksPartsThroughFlattenContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		answer  bool
		preview string
	}{
		{"mixed parts keep only their text", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"audio","audio":{}}]`, true, "a"},
		{"all-non-text parts carry no text", `[{"type":"image_url","image_url":{"url":"x"}}]`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"choices":[{"message":{"role":"assistant","content":` + tc.content + `}}]}`)
			rec := &metrics.Record{StatusCode: 200}
			extractUsageBytes(body, rec, nil, true)
			if rec.HadAnswerContent != tc.answer {
				t.Fatalf("HadAnswerContent = %v, want %v", rec.HadAnswerContent, tc.answer)
			}
			if rec.ResponsePreview != tc.preview {
				t.Fatalf("ResponsePreview = %q, want %q (non-text parts carry no text)", rec.ResponsePreview, tc.preview)
			}
		})
	}
}

// sseEmitter stamps the created timestamp per chunk - the cursor bridge's
// documented policy, the opposite of the Anthropic bridge's one-per-stream
// pin. The gap between the two emits exceeds one second, so a per-stream
// mutation (or a per-chunk regression in the other direction) cannot pass by
// landing inside a single Unix second.
func TestSSEEmitterStampsCreatedPerChunk(t *testing.T) {
	w := httptest.NewRecorder()
	emit := sseEmitter(w, "chatcmpl-per-chunk", "neutral-model", w)
	if err := emit(map[string]any{"content": "first"}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := emit(map[string]any{"content": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	var created []int64
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Created int64 `json:"created"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		created = append(created, chunk.Created)
	}
	if len(created) != 2 {
		t.Fatalf("chunks = %d, want 2: %q", len(created), w.Body.String())
	}
	if created[0] == created[1] {
		t.Fatalf("created = %d for both chunks, want per-chunk stamping (cursor policy)", created[0])
	}
	if created[0] == 0 || created[1] == 0 {
		t.Fatalf("created = %v, want nonzero stamps", created)
	}
}
