package sse

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestJSONSpellingPreservesStreamAccounting(t *testing.T) {
	var a Analyzer
	a.CapturePreview = true
	a.CostKeys = []string{"billing.amount"}
	a.Feed([]byte(`data: {"id" : "req-1", "model" : "model\u002da", "choices" : [{"delta":{"cont\u0065nt" : "hello\nworld"}}]}`), time.Now())
	a.Feed([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\"\r : \"stop\"}],\"us\\u0061ge\" : {\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15},\"billing\":{\"amount\":0.02}}"), time.Now())
	var rec metrics.Record
	a.Fill(&rec)
	if rec.Usage.InputTokens != 10 || rec.Usage.OutputTokens != 5 || rec.Usage.TotalTokens != 15 || rec.Cost != .02 || !rec.HadAnswerContent || rec.FinishReason != "stop" || rec.ProviderModel != "model-a" || rec.ResponsePreview != "hello\nworld" {
		t.Fatalf("accounting lost under legal JSON spelling: %+v", rec)
	}
}

func TestJSONTokenDecoysDoNotBecomeContent(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[{"delta":{"reasoning_details":[  ],"thinking":{  },"tool_calls":[ ]}}]}`,
		`{"choices":[{"delta":{}}],"note":"\"content\":\"text\" response.output_text.delta"}`,
	} {
		var a Analyzer
		a.Feed([]byte("data: "+payload), time.Now())
		if !a.firstTokenAt.IsZero() {
			t.Fatalf("non-content marked a token: %s", payload)
		}
	}
}

func TestJSONToolArrayBoundsAndEscapes(t *testing.T) {
	var a Analyzer
	a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls" : [{"index":0,"id" : "call-a","function":{"name" : "look\u0075p","arguments":""}},{"index":1,"id":"call-b","function":{"name":"other","arguments":""}}]}}],"id":"outer","metadata":{"name":"decoy","id":"decoy"}}`), time.Now())
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ToolCalls != 2 || len(rec.ToolNames) != 2 || rec.ToolNames[0] != "lookup" || rec.ToolNames[1] != "other" {
		t.Fatalf("tool array escaped or escaped its bounds: calls=%d names=%v", rec.ToolCalls, rec.ToolNames)
	}
}

func TestResponseTerminalTypeIsAJSONField(t *testing.T) {
	for _, tt := range []struct {
		payload string
		want    bool
	}{
		{`{"type" : "response.completed"}`, true},
		{`{"ty\u0070e":"response.completed"}`, true},
		{`{"type":"other","note":"response.completed"}`, false},
		{`{"note":"\"type\":\"response.completed\""}`, false},
		{`{"note":{"type":"response.completed"}}`, false},
	} {
		var a Analyzer
		a.Feed([]byte("data: "+tt.payload), time.Now())
		if a.Terminated() != tt.want {
			t.Fatalf("terminal(%s)=%v, want %v", tt.payload, a.Terminated(), tt.want)
		}
		if TerminalLine([]byte("data: "+tt.payload)) != tt.want {
			t.Fatalf("relay terminal gate disagrees for %s", tt.payload)
		}
	}
}

func BenchmarkFeedCompactJSON(b *testing.B) {
	line := []byte(`data: {"id":"req-a","model":"model-a","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`)
	now := time.Now()
	var a Analyzer
	b.ReportAllocs()
	for b.Loop() {
		a.Feed(line, now)
		a.Feed(nil, now)
	}
}
