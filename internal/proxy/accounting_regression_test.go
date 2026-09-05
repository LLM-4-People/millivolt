package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestBridgeKeepsNativeAccountingAndSourceCost(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := config.Default()
			cfg.Providers = map[string]config.ProviderOverride{"up.example": {CostKeys: []string{"billing.amount"}}}
			s := New(cfg, metrics.NewBuffer(8))
			rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
			w := httptest.NewRecorder()
			if stream {
				body := strings.Join([]string{
					`data: {"type":"message_start","message":{"id":"req-a","model":"model-a","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5},"billing":{"amount":0.02}}`,
					`data: {"type":"message_stop"}`,
				}, "\n\n") + "\n\n"
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
				s.transformResponse(t.Context(), w, resp, rec, "anthropic", true)
			} else {
				body := `{"id":"req-a","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30},"billing":{"amount":0.02}}`
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				r := httptest.NewRequest("POST", "http://proxy.example/v1/chat/completions", nil)
				s.serveNonStreaming(t.Context(), w, resp, r, &target{format: "anthropic"}, "", nil, rec, "", scheduler.WaiterHooks{})
			}
			if rec.Usage.InputTokens != 60 || rec.Usage.OutputTokens != 5 || rec.Usage.TotalTokens != 65 || rec.Usage.CacheReadTokens != 20 || rec.Usage.CacheWrite != 30 || rec.Cost != .02 || rec.FinishReason != "stop" || !rec.HadAnswerContent {
				t.Fatalf("bridge record=%+v body=%s", rec, w.Body.String())
			}
		})
	}
}

func TestBridgeToolAccountingCountsIdentityNotFragments(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"req-a","model":"model-a","usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-a","name":"lookup"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"}"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	s := New(config.Default(), metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	s.transformResponse(t.Context(), w, resp, rec, "anthropic", true)
	if rec.ToolCalls != 1 || len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" || rec.HadAnswerContent || rec.FinishReason != "tool_calls" || rec.Usage.TotalTokens != 7 {
		t.Fatalf("tool accounting=%+v", rec)
	}
}

type bridgeFailureWriter struct{ *httptest.ResponseRecorder }

func (bridgeFailureWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type bridgeBlockingBody struct {
	first  *strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *bridgeBlockingBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *bridgeBlockingBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestBridgeClientFailureJoinsSourceObserver(t *testing.T) {
	body := &bridgeBlockingBody{first: strings.NewReader("data: " + `{"type":"message_start","message":{"id":"req-a","model":"model-a"}}` + "\n\n"), closed: make(chan struct{})}
	s := New(config.Default(), metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.transformResponse(t.Context(), bridgeFailureWriter{httptest.NewRecorder()}, &http.Response{Header: make(http.Header), Body: body}, rec, "anthropic", true)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		body.Close()
		t.Fatal("bridge did not join after client failure")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body not closed")
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("cancellation accounting=%+v", rec)
	}
}

func TestConversationExpiryRetainsNoPartitionCounters(t *testing.T) {
	tracker := NewConversationTracker(time.Second, 2)
	now := time.Now()
	first := tracker.Assign("client", "first", "", 1, now)
	for i := 0; i < 1000; i++ {
		tracker.Assign("client", fmt.Sprint(i), "", 1, now)
	}
	second := tracker.Assign("client", "first", "", 1, now.Add(2*time.Second))
	if first == second {
		t.Fatal("expired partition reused a conversation ID")
	}
	if len(tracker.parts) != 1 {
		t.Fatalf("retained %d partitions", len(tracker.parts))
	}
	// Every per-partition map must release expired keys; scalar ID sequencing
	// must not keep a second registry alive behind the session sweep.
	value := reflect.ValueOf(tracker).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() == reflect.Map && field.Len() > 1 {
			t.Fatalf("retained partition map %s: %d entries", value.Type().Field(i).Name, field.Len())
		}
	}
}

func TestMultilineContentWirePreservation(t *testing.T) {
	wire := "data: " + `{"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\n" +
		"data: {\ndata: " + `"choices":[{"delta":{"content":"hello"}}]` + "\ndata: }\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	s := New(config.Default(), metrics.NewBuffer(8))
	w := httptest.NewRecorder()
	rec := &metrics.Record{StatusCode: 200}
	s.streamBody(t.Context(), w, strings.NewReader(wire), rec)
	if w.Body.String() != wire || !rec.HadAnswerContent || rec.ErrorType != "" {
		t.Fatalf("legal multiline content changed: answer=%v error=%s wire=%s", rec.HadAnswerContent, rec.ErrorType, w.Body.String())
	}
}
